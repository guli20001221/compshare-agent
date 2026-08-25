"""Bounded literal-text search inside one caller-named remote application tree.

Claude Code's built-in Grep/Glob tools operate on the control-plane host.  This module provides the
remote equivalent over the existing SSH/SFTP boundary without exposing a shell, following symlinks,
or recursively reading credential stores.  It is intentionally a literal line search rather than a
regular-expression engine: the model can make several narrow calls, while the implementation keeps
CPU and output bounds predictable.
"""
import fnmatch
import posixpath
import stat

import guardrails
import remote_text
import ssh_transport

_MAX_QUERY_CHARS = 256
_MAX_ROOT_CHARS = 512
_MAX_GLOB_CHARS = 96
_MAX_DIRECTORIES = 256
_MAX_FILES = 512
_MAX_FILE_BYTES = 1 << 20
_MAX_TOTAL_BYTES = 8 << 20
_MAX_MATCHES = 100
_DEFAULT_MATCHES = 50
_MAX_LINE_BYTES = 1024
_MAX_FIND_DEPTH = 12
_DEFAULT_FIND_DEPTH = 6

# Searching an entire home/system namespace is materially different from searching an identified
# application tree.  These exact roots are rejected; their concrete descendants remain eligible and
# are still checked by remote_text's shared credential-path boundary.
_TOO_BROAD_ROOTS = {
    "/", "/root", "/home", "/etc", "/var", "/tmp", "/proc", "/sys", "/dev", "/run",
}

TOOL_DESCRIPTION = (
    "Search for a literal text fragment in regular UTF-8 files below one known application directory "
    "inside the target instance. This is the remote equivalent of a bounded Grep/Glob: give an "
    "absolute app root, a literal query, and optionally one basename glob such as *.py or *.js. It "
    "never follows symlinks, rejects broad system/home roots and credential paths, scans at most 256 "
    "directories, 512 files and 8 MiB, returns at most 100 bounded matching lines, and redacts "
    "credential-shaped values. Use this tool, not recursive grep through ssh_exec, for recursive "
    "content search: it validates every descendant and avoids a write-confirmation card. Use "
    "process/listener/launcher evidence to identify the app root; use read_text_file for a full "
    "bounded window once this search identifies a file."
)

FIND_DESCRIPTION = (
    "Find files or directories by one basename glob below a known application directory inside the "
    "target instance. This is the remote equivalent of a bounded Glob: give an absolute app root, a "
    "basename pattern such as *Cache* or *.conf, a maximum depth and result limit. It invokes no "
    "remote shell, never follows symlinks, rejects broad system/home roots and credential paths, "
    "visits at most 256 directories and returns at most 100 paths. Use it to discover an exact path, "
    "then use search_text_tree or read_text_file for content."
)


def input_schema():
    return {
        "type": "object",
        "properties": {
            "root": {
                "type": "string",
                "maxLength": _MAX_ROOT_CHARS,
                "description": "Absolute POSIX path of one identified application directory.",
            },
            "query": {
                "type": "string",
                "minLength": 1,
                "maxLength": _MAX_QUERY_CHARS,
                "description": "Literal text fragment to find; this is not a regular expression.",
            },
            "file_glob": {
                "type": "string",
                "maxLength": _MAX_GLOB_CHARS,
                "default": "*",
                "description": "Optional basename-only glob, for example *.py, *.js, or config.*.",
            },
            "ignore_case": {
                "type": "boolean",
                "default": False,
                "description": "Perform Unicode case-insensitive literal matching.",
            },
            "max_matches": {
                "type": "integer",
                "minimum": 1,
                "maximum": _MAX_MATCHES,
                "default": _DEFAULT_MATCHES,
                "description": "Maximum matching lines to return.",
            },
        },
        "required": ["root", "query"],
        "additionalProperties": False,
    }


def find_schema():
    return {
        "type": "object",
        "properties": {
            "root": {
                "type": "string",
                "maxLength": _MAX_ROOT_CHARS,
                "description": "Absolute POSIX path of one identified application directory.",
            },
            "name_glob": {
                "type": "string",
                "maxLength": _MAX_GLOB_CHARS,
                "description": "Basename-only glob, for example *Cache*, *.conf, or start.*.",
            },
            "ignore_case": {
                "type": "boolean",
                "default": False,
                "description": "Match basenames case-insensitively.",
            },
            "max_depth": {
                "type": "integer",
                "minimum": 0,
                "maximum": _MAX_FIND_DEPTH,
                "default": _DEFAULT_FIND_DEPTH,
                "description": "Maximum descendant depth below root; root children are depth 1.",
            },
            "max_results": {
                "type": "integer",
                "minimum": 1,
                "maximum": _MAX_MATCHES,
                "default": _DEFAULT_MATCHES,
                "description": "Maximum matching paths to return.",
            },
        },
        "required": ["root", "name_glob"],
        "additionalProperties": False,
    }


def _error(error_class, **fields):
    return {"ok": False, "error_class": error_class, **fields}


def _valid_root(root):
    if (not isinstance(root, str) or not root.startswith("/") or "\x00" in root
            or len(root) > _MAX_ROOT_CHARS or posixpath.normpath(root) != root):
        return False
    if root.lower() in _TOO_BROAD_ROOTS:
        return False
    return remote_text._valid_path(root)  # one shared credential/path boundary with read_text_file


def _valid_glob(value):
    return (isinstance(value, str) and 1 <= len(value) <= _MAX_GLOB_CHARS
            and "\x00" not in value and "/" not in value and "\\" not in value
            and value not in (".", "..") and ".." not in value)


def _validated_args(args):
    if not isinstance(args, dict):
        return None, _error("invalid_arguments")
    root = args.get("root")
    query = args.get("query")
    file_glob = args.get("file_glob", "*")
    ignore_case = args.get("ignore_case", False)
    max_matches = args.get("max_matches", _DEFAULT_MATCHES)
    if not _valid_root(root):
        return None, _error("root_not_allowed")
    if not isinstance(query, str) or not 1 <= len(query) <= _MAX_QUERY_CHARS or "\x00" in query:
        return None, _error("invalid_query", root=root)
    if not _valid_glob(file_glob):
        return None, _error("invalid_file_glob", root=root)
    if not isinstance(ignore_case, bool):
        return None, _error("invalid_ignore_case", root=root)
    if (isinstance(max_matches, bool) or not isinstance(max_matches, int)
            or not 1 <= max_matches <= _MAX_MATCHES):
        return None, _error("invalid_max_matches", root=root)
    return {
        "root": root,
        "query": query,
        "file_glob": file_glob,
        "ignore_case": ignore_case,
        "max_matches": max_matches,
    }, None


def _validated_find_args(args):
    if not isinstance(args, dict):
        return None, _error("invalid_arguments")
    root = args.get("root")
    name_glob = args.get("name_glob")
    ignore_case = args.get("ignore_case", False)
    max_depth = args.get("max_depth", _DEFAULT_FIND_DEPTH)
    max_results = args.get("max_results", _DEFAULT_MATCHES)
    if not _valid_root(root):
        return None, _error("root_not_allowed")
    if not _valid_glob(name_glob):
        return None, _error("invalid_name_glob", root=root)
    if not isinstance(ignore_case, bool):
        return None, _error("invalid_ignore_case", root=root)
    if (isinstance(max_depth, bool) or not isinstance(max_depth, int)
            or not 0 <= max_depth <= _MAX_FIND_DEPTH):
        return None, _error("invalid_max_depth", root=root)
    if (isinstance(max_results, bool) or not isinstance(max_results, int)
            or not 1 <= max_results <= _MAX_MATCHES):
        return None, _error("invalid_max_results", root=root)
    return {"root": root, "name_glob": name_glob, "ignore_case": ignore_case,
            "max_depth": max_depth, "max_results": max_results}, None


def _clip_line(line):
    raw = line.encode("utf-8")
    if len(raw) <= _MAX_LINE_BYTES:
        return line, False
    raw = raw[:_MAX_LINE_BYTES]
    while raw:
        try:
            return raw.decode("utf-8"), True
        except UnicodeDecodeError:
            raw = raw[:-1]
    return "", True


def _read_file(sftp, path, size):
    if size > _MAX_FILE_BYTES:
        return None, "file_too_large"
    with sftp.file(path, "rb") as handle:
        data = handle.read(_MAX_FILE_BYTES + 1)
    if len(data) > _MAX_FILE_BYTES:
        return None, "file_too_large"
    try:
        return data.decode("utf-8"), ""
    except UnicodeDecodeError:
        return None, "not_utf8"


def search(conn, args, secrets=(), opener=ssh_transport.open_client):
    """Search one bounded remote tree without following symlinks or invoking remote commands."""
    spec, err = _validated_args(args)
    if err:
        return err
    client, connect_error = opener(conn)
    if connect_error:
        return _error(connect_error.get("error", "connect_failed"),
                      detail=connect_error.get("detail", ""))
    sftp = None
    directories = [spec["root"]]
    directory_count = 0
    files_seen = 0
    files_scanned = 0
    bytes_scanned = 0
    skipped = {"path_not_allowed": 0, "symlink": 0, "not_utf8": 0,
               "file_too_large": 0, "scan_limit": 0}
    matches = []
    truncated = False
    needle = spec["query"].casefold() if spec["ignore_case"] else spec["query"]
    try:
        sftp = client.open_sftp()
        root_attrs = sftp.lstat(spec["root"])
        if stat.S_ISLNK(root_attrs.st_mode):
            return _error("root_symlink_refused", root=spec["root"])
        if not stat.S_ISDIR(root_attrs.st_mode):
            return _error("root_not_directory", root=spec["root"])
        while directories and len(matches) < spec["max_matches"]:
            if directory_count >= _MAX_DIRECTORIES:
                skipped["scan_limit"] += len(directories)
                truncated = True
                break
            current = directories.pop(0)
            directory_count += 1
            entries = sorted(sftp.listdir_attr(current), key=lambda item: item.filename)
            for entry in entries:
                path = posixpath.join(current, entry.filename)
                if not remote_text._valid_path(path):
                    skipped["path_not_allowed"] += 1
                    continue
                if stat.S_ISLNK(entry.st_mode):
                    skipped["symlink"] += 1
                    continue
                if stat.S_ISDIR(entry.st_mode):
                    directories.append(path)
                    continue
                if not stat.S_ISREG(entry.st_mode):
                    continue
                files_seen += 1
                if files_seen > _MAX_FILES or bytes_scanned >= _MAX_TOTAL_BYTES:
                    skipped["scan_limit"] += 1
                    truncated = True
                    break
                if not fnmatch.fnmatchcase(entry.filename, spec["file_glob"]):
                    continue
                if bytes_scanned + entry.st_size > _MAX_TOTAL_BYTES:
                    skipped["scan_limit"] += 1
                    truncated = True
                    break
                text, reason = _read_file(sftp, path, entry.st_size)
                if text is None:
                    skipped[reason] += 1
                    continue
                files_scanned += 1
                bytes_scanned += entry.st_size
                for line_number, line in enumerate(text.splitlines(), 1):
                    haystack = line.casefold() if spec["ignore_case"] else line
                    if needle not in haystack:
                        continue
                    clipped, line_truncated = _clip_line(
                        guardrails.scrub_output(line, secrets))
                    matches.append({"path": path, "line_number": line_number,
                                    "line": clipped, "line_truncated": line_truncated})
                    if len(matches) >= spec["max_matches"]:
                        truncated = True
                        break
                if truncated:
                    break
            if truncated:
                break
        return {
            "ok": True,
            "root": spec["root"],
            "query": spec["query"],
            "file_glob": spec["file_glob"],
            "ignore_case": spec["ignore_case"],
            "matches": matches,
            "directories_scanned": directory_count,
            "files_seen": min(files_seen, _MAX_FILES),
            "files_scanned": files_scanned,
            "bytes_scanned": bytes_scanned,
            "skipped": skipped,
            "truncated": truncated,
        }
    except FileNotFoundError:
        return _error("root_not_found", root=spec["root"])
    except PermissionError:
        return _error("permission_denied", root=spec["root"])
    except Exception as exc:  # noqa: BLE001 — class only; remote paths remain model-visible scope
        return _error("sftp_search_failed", root=spec["root"], detail=type(exc).__name__)
    finally:
        if sftp is not None:
            try:
                sftp.close()
            except Exception:  # noqa: BLE001
                pass
        try:
            client.close()
        except Exception:  # noqa: BLE001
            pass


def find_paths(conn, args, opener=ssh_transport.open_client):
    """Find bounded path metadata below one remote application tree without following symlinks."""
    spec, err = _validated_find_args(args)
    if err:
        return err
    client, connect_error = opener(conn)
    if connect_error:
        return _error(connect_error.get("error", "connect_failed"),
                      detail=connect_error.get("detail", ""))
    sftp = None
    directories = [(spec["root"], 0)]
    directory_count = 0
    skipped = {"path_not_allowed": 0, "symlink": 0, "scan_limit": 0}
    matches = []
    truncated = False
    pattern = spec["name_glob"].casefold() if spec["ignore_case"] else spec["name_glob"]
    try:
        sftp = client.open_sftp()
        root_attrs = sftp.lstat(spec["root"])
        if stat.S_ISLNK(root_attrs.st_mode):
            return _error("root_symlink_refused", root=spec["root"])
        if not stat.S_ISDIR(root_attrs.st_mode):
            return _error("root_not_directory", root=spec["root"])
        while directories and len(matches) < spec["max_results"]:
            if directory_count >= _MAX_DIRECTORIES:
                skipped["scan_limit"] += len(directories)
                truncated = True
                break
            current, depth = directories.pop(0)
            directory_count += 1
            entries = sorted(sftp.listdir_attr(current), key=lambda item: item.filename)
            for entry in entries:
                path = posixpath.join(current, entry.filename)
                if not remote_text._valid_path(path):
                    skipped["path_not_allowed"] += 1
                    continue
                if stat.S_ISLNK(entry.st_mode):
                    skipped["symlink"] += 1
                    continue
                is_dir = stat.S_ISDIR(entry.st_mode)
                is_file = stat.S_ISREG(entry.st_mode)
                if not is_dir and not is_file:
                    continue
                entry_depth = depth + 1
                candidate = entry.filename.casefold() if spec["ignore_case"] else entry.filename
                if entry_depth <= spec["max_depth"] and fnmatch.fnmatchcase(candidate, pattern):
                    matches.append({"path": path, "type": "directory" if is_dir else "file",
                                    "depth": entry_depth})
                    if len(matches) >= spec["max_results"]:
                        truncated = True
                        break
                if is_dir and entry_depth < spec["max_depth"]:
                    directories.append((path, entry_depth))
            if truncated:
                break
        return {"ok": True, "root": spec["root"], "name_glob": spec["name_glob"],
                "ignore_case": spec["ignore_case"], "max_depth": spec["max_depth"],
                "matches": matches, "directories_scanned": directory_count,
                "skipped": skipped, "truncated": truncated}
    except FileNotFoundError:
        return _error("root_not_found", root=spec["root"])
    except PermissionError:
        return _error("permission_denied", root=spec["root"])
    except Exception as exc:  # noqa: BLE001
        return _error("sftp_find_failed", root=spec["root"], detail=type(exc).__name__)
    finally:
        if sftp is not None:
            try:
                sftp.close()
            except Exception:  # noqa: BLE001
                pass
        try:
            client.close()
        except Exception:  # noqa: BLE001
            pass
