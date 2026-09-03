"""Bounded literal-text search inside one caller-named remote application tree.

Claude Code's built-in Grep/Glob tools operate on the control-plane host.  This module provides the
remote equivalent over the existing SSH boundary without exposing a shell, following symlinks,
or recursively reading credential stores.  It is intentionally a literal line search rather than a
regular-expression engine: the model can make several narrow calls, while the implementation keeps
CPU and output bounds predictable.
"""
import base64
import json
from pathlib import Path
import posixpath
import shlex
import time

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
# Whole matching lines are base64-encoded until the control-plane redaction pass. This includes
# 8 MiB of matching text plus bounded JSON/path overhead without the shell tool's 16 KiB clipping.
_MAX_WIRE_BYTES = 12 << 20
_MAX_STDERR_BYTES = 16 << 10

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
    "credential-shaped values. Traversal and matching run inside the instance and only bounded "
    "results return over SSH. Use "
    "process/listener/launcher evidence to identify the app root; use read_text_file for a full "
    "bounded window once this search identifies a file."
)

FIND_DESCRIPTION = (
    "Find files or directories by one basename glob below a known application directory inside the "
    "target instance. This is the remote equivalent of a bounded Glob: give an absolute app root, a "
    "basename pattern such as *Cache* or *.conf, a maximum depth and result limit. Traversal runs "
    "inside the instance, follows no symlink, rejects broad system/home roots and credential paths, "
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


def _worker_request(operation, spec):
    return {
        "operation": operation, "spec": spec,
        "limits": {"directories": _MAX_DIRECTORIES, "files": _MAX_FILES,
                   "file_bytes": _MAX_FILE_BYTES, "total_bytes": _MAX_TOTAL_BYTES},
        "policy": {"max_chars": _MAX_ROOT_CHARS,
                   "exact": sorted(remote_text._DENIED_EXACT),
                   "prefixes": remote_text._DENIED_PREFIXES,
                   "components": sorted(remote_text._DENIED_COMPONENTS),
                   "basenames": sorted(remote_text._DENIED_BASENAMES),
                   "private_key_suffix": remote_text._PRIVATE_KEY_SUFFIX.pattern},
    }


def _worker_command():
    source = Path(__file__).with_name("guest_search.py").read_text(encoding="utf-8")
    # Only packaged code is shell-quoted here; all caller arguments go through private stdin.
    quoted = shlex.quote(source)
    return ("if command -v python3 >/dev/null 2>&1; then exec python3 -I -B -c " + quoted
            + "; elif command -v python >/dev/null 2>&1; then exec python -I -B -c " + quoted
            + "; else exit 127; fi")


def _receive(channel):
    """Keep the structured frame intact, but stop oversized or stalled remote output."""
    deadline = time.monotonic() + ssh_transport._EXEC_TIMEOUT
    out, err = bytearray(), bytearray()
    while True:
        moved = False
        while channel.recv_ready():
            out.extend(channel.recv(65536))
            moved = True
            if len(out) > _MAX_WIRE_BYTES:
                raise ValueError("structured_output_too_large")
        while channel.recv_stderr_ready():
            err.extend(channel.recv_stderr(4096))
            moved = True
            if len(err) > _MAX_STDERR_BYTES:
                raise ValueError("structured_stderr_too_large")
        if channel.exit_status_ready() and not channel.recv_ready() and not channel.recv_stderr_ready():
            return bytes(out), channel.recv_exit_status()
        if time.monotonic() >= deadline:
            raise TimeoutError("structured_search_timeout")
        if not moved:
            time.sleep(0.01)


def _remote(conn, operation, spec, secrets, opener):
    client, connect_error = opener(conn)
    if connect_error:
        return _error(connect_error.get("error", "connect_failed"),
                      detail=connect_error.get("detail", ""))
    channel = None
    try:
        stdin, stdout, _stderr = client.exec_command(
            ssh_transport._bounded(_worker_command()), timeout=ssh_transport._EXEC_TIMEOUT)
        channel = stdout.channel
        stdin.write(json.dumps(_worker_request(operation, spec), separators=(",", ":")))
        stdin.flush()
        channel.shutdown_write()
        payload, exit_code = _receive(channel)
        if exit_code == 124:
            return _error("exec_timeout", root=spec["root"], detail="remote_search_timeout")
        if exit_code != 0:
            return _error("search_failed", root=spec["root"],
                          detail="python_unavailable" if exit_code == 127 else "worker_exit_" + str(exit_code))
        result = json.loads(payload)
        if not isinstance(result, dict) or not isinstance(result.get("ok"), bool):
            raise ValueError("invalid_structured_result")
        if result["ok"]:
            for match in result["matches"]:
                match["path"] = guardrails.scrub_output(match["path"], secrets)
                if operation == "search":
                    line = base64.b64decode(match.pop("line_base64"), validate=True).decode("utf-8")
                    match["line"], match["line_truncated"] = _clip_line(
                        guardrails.scrub_output(line, secrets))
        return result
    except TimeoutError:
        return _error("exec_timeout", root=spec["root"], detail="remote_search_timeout")
    except Exception as exc:  # noqa: BLE001 — only class names, not private connection details
        return _error("search_failed", root=spec["root"], detail=type(exc).__name__)
    finally:
        if channel is not None:
            try:
                channel.close()
            except Exception:  # noqa: BLE001
                pass
        try:
            client.close()
        except Exception:  # noqa: BLE001
            pass


def search(conn, args, secrets=(), opener=ssh_transport.open_client):
    """Search one tree in the Guest; only bounded matching text crosses SSH."""
    spec, err = _validated_args(args)
    return err if err else _remote(conn, "search", spec, secrets, opener)


def find_paths(conn, args, secrets=(), opener=ssh_transport.open_client):
    """Find bounded path metadata in the Guest without downloading directory trees over SFTP."""
    spec, err = _validated_find_args(args)
    return err if err else _remote(conn, "find", spec, secrets, opener)
