"""Standard-library search worker executed in memory inside the bound SSH instance.

The private stdin request supplies the existing path policy and scan limits. No package,
configuration or worker file is installed in the instance. Matching lines travel whole, encoded
as base64, so the caller can redact before clipping without an intermediate text-output truncation.
"""
import base64
import fnmatch
import json
import os
import posixpath
import re
import stat
import sys


class FileSystem:
    def lstat(self, path):
        return os.lstat(path)

    def entries(self, path):
        with os.scandir(path) as entries:
            return sorted(((entry.name, entry.stat(follow_symlinks=False)) for entry in entries),
                          key=lambda item: item[0])

    def read(self, path, limit):
        # A file swapped for a symlink or pipe after directory enumeration is not a text file.
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as handle:
            if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
                return None
            return handle.read(limit)


def path_allowed(path, policy):
    """Apply the caller's shared remote_text policy, not an independent directory allowlist."""
    if (not isinstance(path, str) or not path.startswith("/") or "\x00" in path
            or len(path) > policy["max_chars"] or path == "/"
            or posixpath.normpath(path) != path):
        return False
    low = path.lower()
    if low in policy["exact"] or any(low.startswith(prefix) for prefix in policy["prefixes"]):
        return False
    parts = [part.lower() for part in path.split("/") if part]
    if any(part in policy["components"] for part in parts):
        return False
    base = parts[-1] if parts else ""
    return not (base in policy["basenames"] or base.startswith(".env.")
                or re.search(policy["private_key_suffix"], base, re.I))


def run(request, filesystem=None):
    fs = filesystem if filesystem is not None else FileSystem()
    operation, spec, limits, policy = (request[key] for key in ("operation", "spec", "limits", "policy"))
    root = spec["root"]
    try:
        attrs = fs.lstat(root)
        if stat.S_ISLNK(attrs.st_mode):
            return {"ok": False, "error_class": "root_symlink_refused", "root": root}
        if not stat.S_ISDIR(attrs.st_mode):
            return {"ok": False, "error_class": "root_not_directory", "root": root}
        return _scan(fs, operation, spec, limits, policy)
    except FileNotFoundError:
        return {"ok": False, "error_class": "root_not_found", "root": root}
    except PermissionError:
        return {"ok": False, "error_class": "permission_denied", "root": root}
    except Exception as exc:  # noqa: BLE001 — remote diagnostics never echo exception payloads
        return {"ok": False, "error_class": "search_failed", "root": root,
                "detail": type(exc).__name__}


def _scan(fs, operation, spec, limits, policy):
    content = operation == "search"
    cap = spec["max_matches"] if content else spec["max_results"]
    pattern = spec["query"] if content else spec["name_glob"]
    pattern = pattern.casefold() if spec["ignore_case"] else pattern
    directories = [(spec["root"], 0)]
    directory_count = files_seen = files_scanned = bytes_scanned = 0
    matches, truncated = [], False
    skipped = {"path_not_allowed": 0, "symlink": 0, "scan_limit": 0}
    if content:
        skipped.update(not_utf8=0, file_too_large=0)
    while directories and len(matches) < cap:
        if directory_count >= limits["directories"]:
            skipped["scan_limit"] += len(directories)
            truncated = True
            break
        current, depth = directories.pop(0)
        directory_count += 1
        for name, attrs in fs.entries(current):
            path = posixpath.join(current, name)
            if not path_allowed(path, policy):
                skipped["path_not_allowed"] += 1
                continue
            if stat.S_ISLNK(attrs.st_mode):
                skipped["symlink"] += 1
                continue
            is_dir, is_file = stat.S_ISDIR(attrs.st_mode), stat.S_ISREG(attrs.st_mode)
            if not is_dir and not is_file:
                continue
            if not content:
                entry_depth = depth + 1
                candidate = name.casefold() if spec["ignore_case"] else name
                if entry_depth <= spec["max_depth"] and fnmatch.fnmatchcase(candidate, pattern):
                    matches.append({"path": path, "type": "directory" if is_dir else "file",
                                    "depth": entry_depth})
                    if len(matches) >= cap:
                        truncated = True
                        break
                if is_dir and entry_depth < spec["max_depth"]:
                    directories.append((path, entry_depth))
                continue
            if is_dir:
                directories.append((path, depth + 1))
                continue
            files_seen += 1
            if files_seen > limits["files"] or bytes_scanned >= limits["total_bytes"]:
                skipped["scan_limit"] += 1
                truncated = True
                break
            if not fnmatch.fnmatchcase(name, spec["file_glob"]):
                continue
            if bytes_scanned + attrs.st_size > limits["total_bytes"]:
                skipped["scan_limit"] += 1
                truncated = True
                break
            if attrs.st_size > limits["file_bytes"]:
                skipped["file_too_large"] += 1
                continue
            data = fs.read(path, limits["file_bytes"] + 1)
            if data is None:
                continue
            if len(data) > limits["file_bytes"]:
                skipped["file_too_large"] += 1
                continue
            try:
                text = data.decode("utf-8")
            except UnicodeDecodeError:
                skipped["not_utf8"] += 1
                continue
            files_scanned += 1
            bytes_scanned += attrs.st_size
            for number, line in enumerate(text.splitlines(), 1):
                haystack = line.casefold() if spec["ignore_case"] else line
                if pattern not in haystack:
                    continue
                matches.append({"path": path, "line_number": number,
                                "line_base64": base64.b64encode(line.encode("utf-8")).decode("ascii")})
                if len(matches) >= cap:
                    truncated = True
                    break
            if truncated:
                break
        if truncated:
            break
    result = {"ok": True, "root": spec["root"], "ignore_case": spec["ignore_case"],
              "matches": matches, "directories_scanned": directory_count,
              "skipped": skipped, "truncated": truncated}
    if content:
        result.update(query=spec["query"], file_glob=spec["file_glob"],
                      files_seen=min(files_seen, limits["files"]), files_scanned=files_scanned,
                      bytes_scanned=bytes_scanned)
    else:
        result.update(name_glob=spec["name_glob"], max_depth=spec["max_depth"])
    return result


if __name__ == "__main__":
    print(json.dumps(run(json.load(sys.stdin)), ensure_ascii=True, separators=(",", ":")))
