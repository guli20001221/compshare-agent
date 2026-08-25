"""Bounded, read-only UTF-8 file inspection over the existing SSH credential boundary.

Claude Code's built-in Read tool operates on the control-plane host, so it cannot safely stand in
for a read inside the customer's instance.  This module provides the equivalent *remote* primitive:
one caller-named regular file, no symlink following, a bounded line window, a whole-file SHA-256 for
the hash-bound editor, and the same output redaction used by shell probes.
"""
import hashlib
import posixpath
import re
import stat

import guardrails
import ssh_transport

_MAX_FILE_BYTES = 1 << 20
_MAX_RETURN_BYTES = 32 << 10
_DEFAULT_LINE_COUNT = 200
_MAX_LINE_COUNT = 400

# These are credential/control-plane boundaries, not an application-file allowlist.  Ordinary
# application and system configuration remains readable; known credential stores, private keys,
# process argv/environments and virtual files stay unavailable without trying to guess their
# contents.  lstat + symlink refusal prevents a benign-looking path from redirecting this tool.
_DENIED_EXACT = {
    "/etc/shadow", "/etc/gshadow", "/etc/sudoers",
}
_DENIED_PREFIXES = (
    "/proc/", "/sys/", "/dev/", "/run/secrets/", "/var/run/secrets/",
    "/etc/ssl/private/", "/etc/kubernetes/",
)
_DENIED_COMPONENTS = {
    ".ssh", ".aws", ".gnupg", ".kube", ".docker",
}
_DENIED_BASENAMES = {
    ".env", ".netrc", ".npmrc", ".pypirc", ".git-credentials",
    "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "authorized_keys",
}
_PRIVATE_KEY_SUFFIX = re.compile(r"\.(?:pem|key|p12|pfx)$", re.I)


TOOL_DESCRIPTION = (
    "Read a bounded line window from one regular UTF-8 text file inside the target instance over "
    "SSH. Use this instead of cat/head/sed for a known configuration, manifest, catalog or other "
    "caller-named text file. It is read-only, follows no symlink, refuses credential stores, private "
    "keys and kernel/device pseudo-files, caps files at 1 MiB and returned content at 32 KiB, and "
    "redacts credential-shaped values. The result includes the whole-file SHA-256 required by "
    "atomic_text_edit plus line-window metadata. Use ssh_exec for generated process, listener, "
    "hardware and service-manager state, and for bounded log searches rather than reading a large "
    "log as one file."
)


def input_schema():
    return {
        "type": "object",
        "properties": {
            "path": {
                "type": "string",
                "description": "Absolute POSIX path of one regular UTF-8 file in the target instance.",
            },
            "line_start": {
                "type": "integer", "minimum": 1, "default": 1,
                "description": "One-based first line to return.",
            },
            "line_count": {
                "type": "integer", "minimum": 1, "maximum": _MAX_LINE_COUNT,
                "default": _DEFAULT_LINE_COUNT,
                "description": "Maximum number of lines to return.",
            },
        },
        "required": ["path"],
        "additionalProperties": False,
    }


def _error(error_class, **fields):
    return {"ok": False, "error_class": error_class, **fields}


def _valid_path(path):
    if not isinstance(path, str) or not path.startswith("/") or "\x00" in path or len(path) > 512:
        return False
    if path == "/" or posixpath.normpath(path) != path:
        return False
    low = path.lower()
    if low in _DENIED_EXACT or any(low.startswith(prefix) for prefix in _DENIED_PREFIXES):
        return False
    parts = [part.lower() for part in path.split("/") if part]
    if any(part in _DENIED_COMPONENTS for part in parts):
        return False
    base = parts[-1] if parts else ""
    if (base in _DENIED_BASENAMES or base.startswith(".env.")
            or _PRIVATE_KEY_SUFFIX.search(base)):
        return False
    return True


def _validated_args(args):
    if not isinstance(args, dict):
        return None, _error("invalid_arguments")
    path = args.get("path")
    line_start = args.get("line_start", 1)
    line_count = args.get("line_count", _DEFAULT_LINE_COUNT)
    if not _valid_path(path):
        return None, _error("path_not_allowed")
    if isinstance(line_start, bool) or not isinstance(line_start, int) or line_start < 1:
        return None, _error("invalid_line_start", path=path)
    if (isinstance(line_count, bool) or not isinstance(line_count, int)
            or line_count < 1 or line_count > _MAX_LINE_COUNT):
        return None, _error("invalid_line_count", path=path)
    return {"path": path, "line_start": line_start, "line_count": line_count}, None


def _read_regular(sftp, path):
    attrs = sftp.lstat(path)
    if stat.S_ISLNK(attrs.st_mode):
        raise ValueError("symlink_refused")
    if not stat.S_ISREG(attrs.st_mode):
        raise ValueError("not_regular_file")
    if attrs.st_size > _MAX_FILE_BYTES:
        raise ValueError("file_too_large")
    with sftp.file(path, "rb") as handle:
        data = handle.read(_MAX_FILE_BYTES + 1)
    if len(data) > _MAX_FILE_BYTES:
        raise ValueError("file_too_large")
    if len(data) != attrs.st_size:
        raise ValueError("short_read")
    return data


def _line_window(text, line_start, line_count):
    lines = text.splitlines(keepends=True)
    # A final empty logical line is not useful as a separately addressable line.  Empty files have
    # zero lines; callers may still read them successfully and receive content="".
    total_lines = len(lines)
    start_index = line_start - 1
    if start_index > total_lines:
        return None, _error("line_start_out_of_range", total_lines=total_lines)
    selected = lines[start_index:start_index + line_count]
    content = "".join(selected)
    encoded = content.encode("utf-8")
    clipped_line = False
    if len(encoded) > _MAX_RETURN_BYTES:
        selected, used = [], 0
        for line in lines[start_index:start_index + line_count]:
            line_bytes = line.encode("utf-8")
            if used + len(line_bytes) > _MAX_RETURN_BYTES:
                clipped_line = not selected
                if clipped_line:
                    fragment = line_bytes[:_MAX_RETURN_BYTES]
                    while fragment:
                        try:
                            selected.append(fragment.decode("utf-8"))
                            break
                        except UnicodeDecodeError:
                            fragment = fragment[:-1]
                break
            selected.append(line)
            used += len(line_bytes)
        content = "".join(selected)
    complete_lines = 0 if clipped_line else len(selected)
    line_end = line_start if clipped_line else (
        line_start + complete_lines - 1 if complete_lines else line_start - 1)
    more_lines = start_index + complete_lines < total_lines
    truncated = clipped_line or more_lines
    next_line = None if clipped_line or not more_lines else line_end + 1
    return {
        "content": content,
        "total_lines": total_lines,
        "line_start": line_start,
        "line_end": line_end,
        "returned_bytes": len(content.encode("utf-8")),
        "truncated": truncated,
        "next_line": next_line,
        "line_too_long": clipped_line,
    }, None


def read(conn, args, secrets=(), opener=ssh_transport.open_client):
    """Read one validated text window; no remote state is changed."""
    spec, err = _validated_args(args)
    if err:
        return err
    client, connect_error = opener(conn)
    if connect_error:
        return _error(connect_error.get("error", "connect_failed"),
                      detail=connect_error.get("detail", ""))
    sftp = None
    try:
        sftp = client.open_sftp()
        data = _read_regular(sftp, spec["path"])
        digest = hashlib.sha256(data).hexdigest()
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            return _error("not_utf8", path=spec["path"], size_bytes=len(data), sha256=digest)
        window, err = _line_window(text, spec["line_start"], spec["line_count"])
        if err:
            return _error(err["error_class"], path=spec["path"], size_bytes=len(data),
                          sha256=digest, **{key: value for key, value in err.items()
                                            if key not in ("ok", "error_class")})
        window["content"] = guardrails.scrub_output(window["content"], secrets)
        return {"ok": True, "path": spec["path"], "size_bytes": len(data),
                "sha256": digest, **window}
    except ValueError as exc:
        return _error(str(exc), path=spec["path"])
    except Exception as exc:  # noqa: BLE001 — class only; remote detail may contain private data
        return _error("sftp_read_failed", path=spec["path"], detail=type(exc).__name__)
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
