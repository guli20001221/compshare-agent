"""Hash-bound, atomic UTF-8 text replacement over SFTP.

The tool is intentionally narrower than a generic Write/Edit primitive: one exact old string in one
regular file is replaced, the caller must provide the SHA-256 it observed, and the hash is checked
again after the user approves.  Content never enters the confirmation wire or command audit.
"""
import hashlib
import posixpath
import stat
import uuid

import ssh_transport

_MAX_FILE_BYTES = 1 << 20
_MAX_REPLACEMENT_BYTES = 64 << 10
_DENIED_EXACT = {
    "/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/sudoers",
    "/etc/fstab", "/etc/crypttab", "/etc/hosts", "/etc/resolv.conf",
}
_DENIED_PREFIXES = (
    "/proc/", "/sys/", "/dev/", "/boot/", "/etc/ssh/", "/root/.ssh/",
    "/run/secrets/", "/var/run/secrets/",
)


def input_schema():
    return {
        "type": "object",
        "properties": {
            "path": {"type": "string", "description": "Absolute POSIX path of a regular UTF-8 file."},
            "expected_sha256": {
                "type": "string",
                "pattern": "^[0-9a-fA-F]{64}$",
                "description": "SHA-256 observed from a prior read; stale files are refused.",
            },
            "old_text": {"type": "string", "minLength": 1,
                         "description": "Exact text that must occur once."},
            "new_text": {"type": "string", "description": "Replacement text."},
            "change_summary": {"type": "string", "maxLength": 200,
                               "description": "Short reason shown on the approval card; no secrets."},
        },
        "required": ["path", "expected_sha256", "old_text", "new_text", "change_summary"],
        "additionalProperties": False,
    }


TOOL_DESCRIPTION = (
    "Atomically replace one exact UTF-8 text fragment in one existing regular file on the target "
    "instance. Use after ssh_exec has read the relevant file and computed its SHA-256. The tool "
    "requires that hash, refuses stale content/symlinks and selected boot, login, SSH and network "
    "files whose failure can remove the recovery channel, requires the old text to occur exactly "
    "once, preserves mode/owner, writes a recoverable "
    "same-directory backup, atomically renames, and verifies the final hash. The approval card shows "
    "path, purpose, occurrence count and before/after hashes but never file contents. Do not use it "
    "to replace whole applications or bypass an existing service manager contract.")


def _sha(data):
    return hashlib.sha256(data).hexdigest()


def _error(error_class, **fields):
    return {"ok": False, "error_class": error_class, **fields}


def _valid_path(path):
    if not isinstance(path, str) or not path.startswith("/") or "\x00" in path or len(path) > 512:
        return False
    if posixpath.normpath(path) != path or path == "/":
        return False
    if path in _DENIED_EXACT or any(path.startswith(prefix) for prefix in _DENIED_PREFIXES):
        return False
    parts = path.split("/")
    if ".ssh" in parts:
        return False
    return True


def _validated_args(args):
    if not isinstance(args, dict):
        return None, _error("invalid_arguments")
    path = args.get("path")
    expected = args.get("expected_sha256")
    old = args.get("old_text")
    new = args.get("new_text")
    summary = " ".join(str(args.get("change_summary") or "").split())[:200]
    if not _valid_path(path):
        return None, _error("path_not_allowed")
    if (not isinstance(expected, str) or len(expected) != 64
            or any(ch not in "0123456789abcdefABCDEF" for ch in expected)):
        return None, _error("invalid_expected_sha256", path=path)
    if not isinstance(old, str) or not old or not isinstance(new, str) or not summary:
        return None, _error("invalid_replacement", path=path)
    old_b, new_b = old.encode("utf-8"), new.encode("utf-8")
    if len(old_b) > _MAX_REPLACEMENT_BYTES or len(new_b) > _MAX_REPLACEMENT_BYTES:
        return None, _error("replacement_too_large", path=path)
    return {"path": path, "expected": expected.lower(), "old": old, "new": new,
            "summary": summary, "old_sha256": _sha(old_b), "new_sha256": _sha(new_b)}, None


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
    return attrs, data


def _open_sftp(conn, opener):
    client, connect_error = opener(conn)
    if connect_error:
        return None, None, _error(connect_error.get("error", "connect_failed"),
                                  detail=connect_error.get("detail", ""))
    try:
        return client, client.open_sftp(), None
    except Exception as exc:  # noqa: BLE001 — class only; remote detail may contain private data
        client.close()
        return None, None, _error("sftp_open_failed", detail=type(exc).__name__)


def prepare_replace(conn, args, opener=ssh_transport.open_client):
    """Read and validate a plan.  No write occurs; private replacement bytes stay under `_data`."""
    spec, err = _validated_args(args)
    if err:
        return err
    client, sftp, err = _open_sftp(conn, opener)
    if err:
        return err
    try:
        attrs, data = _read_regular(sftp, spec["path"])
        before = _sha(data)
        if before != spec["expected"]:
            return _error("stale_precondition", path=spec["path"], actual_sha256=before)
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            return _error("not_utf8", path=spec["path"], before_sha256=before)
        count = text.count(spec["old"])
        if count != 1:
            return _error("match_count_not_one", path=spec["path"], match_count=count,
                          before_sha256=before)
        new_data = text.replace(spec["old"], spec["new"], 1).encode("utf-8")
        if len(new_data) > _MAX_FILE_BYTES:
            return _error("result_too_large", path=spec["path"], before_sha256=before)
        after = _sha(new_data)
        return {
            "ok": True, "path": spec["path"], "change_summary": spec["summary"],
            "before_sha256": before, "after_sha256": after,
            "old_text_sha256": spec["old_sha256"], "new_text_sha256": spec["new_sha256"],
            "replacement_count": 1, "result_bytes": len(new_data),
            "_data": new_data, "_mode": stat.S_IMODE(attrs.st_mode),
            "_uid": attrs.st_uid, "_gid": attrs.st_gid,
        }
    except ValueError as exc:
        return _error(str(exc), path=spec["path"])
    except Exception as exc:  # noqa: BLE001
        return _error("sftp_read_failed", path=spec["path"], detail=type(exc).__name__)
    finally:
        try:
            sftp.close()
        finally:
            client.close()


def public_plan(plan):
    return {key: value for key, value in plan.items() if not key.startswith("_")}


def confirmation_display(plan):
    return ("atomic_text_replace path=%s before_sha256=%s after_sha256=%s replacements=1 summary=%s"
            % (plan["path"], plan["before_sha256"], plan["after_sha256"],
               plan["change_summary"]))


def _write_file(sftp, path, data, mode, uid, gid):
    with sftp.file(path, "wb") as handle:
        handle.write(data)
        handle.flush()
    sftp.chmod(path, mode)
    sftp.chown(path, uid, gid)


def apply_replace(conn, plan, opener=ssh_transport.open_client):
    """Re-check the precondition, atomically replace, verify, and retain a backup."""
    if not plan.get("ok") or not isinstance(plan.get("_data"), bytes):
        return _error("invalid_plan")
    client, sftp, err = _open_sftp(conn, opener)
    if err:
        return err
    path = plan["path"]
    directory, name = posixpath.split(path)
    nonce = uuid.uuid4().hex[:12]
    backup = posixpath.join(directory, ".%s.sshops-backup-%s-%s" %
                            (name, plan["before_sha256"][:12], nonce))
    temporary = posixpath.join(directory, ".%s.sshops-tmp-%s" % (name, nonce))
    replaced = False
    backup_written = False
    try:
        attrs, current = _read_regular(sftp, path)
        if _sha(current) != plan["before_sha256"]:
            return _error("stale_after_approval", path=path, actual_sha256=_sha(current))
        mode = stat.S_IMODE(attrs.st_mode)
        if mode != plan["_mode"] or attrs.st_uid != plan["_uid"] or attrs.st_gid != plan["_gid"]:
            return _error("metadata_changed_after_approval", path=path)
        _write_file(sftp, backup, current, mode, attrs.st_uid, attrs.st_gid)
        backup_written = True
        _write_file(sftp, temporary, plan["_data"], mode, attrs.st_uid, attrs.st_gid)
        # OpenSSH's posix-rename extension replaces the target atomically. Refuse rather than fall
        # back to remove+rename, which creates a window where the application has no config file.
        sftp.posix_rename(temporary, path)
        replaced = True
        _, verified = _read_regular(sftp, path)
        actual = _sha(verified)
        if actual != plan["after_sha256"]:
            raise RuntimeError("postcondition_failed")
        return {"ok": True, "path": path, "before_sha256": plan["before_sha256"],
                "after_sha256": actual, "replacement_count": 1,
                "result_bytes": len(verified), "backup_path": backup, "atomic": True}
    except Exception as exc:  # noqa: BLE001
        rolled_back = False
        if replaced and backup_written:
            try:
                sftp.posix_rename(backup, path)
                rolled_back = True
            except Exception:  # noqa: BLE001 — best effort, reported below
                rolled_back = False
        return _error("atomic_replace_failed", path=path, detail=type(exc).__name__,
                      rolled_back=rolled_back, box_may_be_changed=replaced and not rolled_back)
    finally:
        if not replaced:
            for candidate in (temporary, backup if backup_written else ""):
                if not candidate:
                    continue
                try:
                    sftp.remove(candidate)
                except Exception:  # noqa: BLE001
                    pass
        try:
            sftp.close()
        finally:
            client.close()
