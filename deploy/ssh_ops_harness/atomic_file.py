"""Task-scope-authorized atomic UTF-8 text edits over SFTP.

Two generic operations are supported:

* ``replace_fragment`` changes one exact fragment in an existing regular file whose SHA-256 was
  observed by ``read_text_file``;
* ``create`` creates one new regular file without overwriting an existing path.

Content never enters the legacy confirmation wire or command audit. Both operations re-check their
preconditions at apply time, refuse symlinks, use a same-directory temporary and verify the
resulting bytes. Replacements retain a same-directory backup; creates use the standard SFTP rename
operation whose contract refuses an existing destination rather than the overwrite-capable
OpenSSH ``posix-rename`` extension.
"""
import errno
import hashlib
import posixpath
import stat
import uuid

import ssh_transport

_MAX_FILE_BYTES = 1 << 20
_MAX_FRAGMENT_BYTES = 64 << 10
_MAX_CREATE_BYTES = 64 << 10
_DENIED_EXACT = {
    "/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/sudoers",
    "/etc/fstab", "/etc/crypttab", "/etc/hosts", "/etc/resolv.conf",
}
_DENIED_PREFIXES = (
    "/proc/", "/sys/", "/dev/", "/boot/", "/etc/ssh/", "/root/.ssh/",
    "/run/secrets/", "/var/run/secrets/",
)
_OPERATIONS = {"create", "replace_fragment"}


def input_schema():
    return {
        "type": "object",
        "properties": {
            "operation": {
                "type": "string", "enum": sorted(_OPERATIONS),
                "description": "create a new file, or replace_fragment in an observed file.",
            },
            "path": {"type": "string", "description": "Absolute normalized POSIX path."},
            "expected_sha256": {
                "type": "string", "pattern": "^[0-9a-fA-F]{64}$",
                "description": "For replace_fragment: SHA-256 returned by read_text_file.",
            },
            "old_text": {
                "type": "string", "minLength": 1,
                "description": "For replace_fragment: exact text that must occur once.",
            },
            "new_text": {
                "type": "string", "description": "For replace_fragment: replacement text.",
            },
            "content": {
                "type": "string", "maxLength": _MAX_CREATE_BYTES,
                "description": "For create: complete UTF-8 content of the new file.",
            },
            "mode": {
                "type": "string", "pattern": "^0[0-7]{3}$",
                "description": "For create: ordinary permission bits as four octal digits, e.g. 0644.",
            },
            "change_summary": {
                "type": "string", "minLength": 1, "maxLength": 200,
                "description": "Short audit reason for the task-scoped change; no secrets.",
            },
        },
        "required": ["operation", "path", "change_summary"],
        "additionalProperties": False,
    }


TOOL_DESCRIPTION = (
    "Atomically edit one bounded UTF-8 text file on the target instance. Use operation=create to "
    "create a new file only when its absolute target does not exist and its real parent directory "
    "already exists; provide bounded content and an explicit ordinary mode such as 0644. Use "
    "operation=replace_fragment after read_text_file has returned an existing file's relevant "
    "content and whole-file SHA-256; the exact old text must occur once. Both operations re-check "
    "at apply time, refuse symlinks and selected boot/login/SSH/network paths whose failure can "
    "remove the recovery channel, write through a same-directory temporary, and verify the final "
    "hash. Create never overwrites; replacement preserves mode/owner and retains a recoverable "
    "backup. The bounded audit display records operation, path, purpose, hashes and mode/count but never "
    "file contents. Use this generic file primitive only for the diagnosed repair; do not replace whole "
    "applications or bypass an existing service manager contract.")


def _sha(data):
    return hashlib.sha256(data).hexdigest()


def _error(error_class, **fields):
    return {"ok": False, "error_class": error_class, **fields}


def _exception_fields(exc, stage):
    """Expose only stable diagnostics, never a remote/server error string."""
    fields = {"detail": type(exc).__name__, "stage": stage}
    error_number = getattr(exc, "errno", None)
    if isinstance(error_number, int):
        fields["errno"] = error_number
    return fields


def _valid_path(path):
    if not isinstance(path, str) or not path.startswith("/") or "\x00" in path or len(path) > 512:
        return False
    if posixpath.normpath(path) != path or path == "/":
        return False
    if path in _DENIED_EXACT or any(path.startswith(prefix) for prefix in _DENIED_PREFIXES):
        return False
    if ".ssh" in path.split("/"):
        return False
    return True


def _summary(args):
    return " ".join(str(args.get("change_summary") or "").split())[:200]


def _validated_args(args):
    if not isinstance(args, dict):
        return None, _error("invalid_arguments")
    operation, path, summary = args.get("operation"), args.get("path"), _summary(args)
    if operation not in _OPERATIONS:
        return None, _error("invalid_operation")
    if not _valid_path(path):
        return None, _error("path_not_allowed")
    if not summary:
        return None, _error("invalid_change_summary", path=path)

    if operation == "create":
        content, mode_text = args.get("content"), args.get("mode")
        if not isinstance(content, str):
            return None, _error("invalid_content", path=path)
        data = content.encode("utf-8")
        if len(data) > _MAX_CREATE_BYTES:
            return None, _error("content_too_large", path=path)
        if (not isinstance(mode_text, str) or len(mode_text) != 4 or mode_text[0] != "0"
                or any(ch not in "01234567" for ch in mode_text[1:])):
            return None, _error("invalid_mode", path=path)
        return {
            "operation": operation, "path": path, "summary": summary, "data": data,
            "mode": int(mode_text, 8), "mode_text": mode_text, "after_sha256": _sha(data),
        }, None

    expected, old, new = args.get("expected_sha256"), args.get("old_text"), args.get("new_text")
    if (not isinstance(expected, str) or len(expected) != 64
            or any(ch not in "0123456789abcdefABCDEF" for ch in expected)):
        return None, _error("invalid_expected_sha256", path=path)
    if not isinstance(old, str) or not old or not isinstance(new, str):
        return None, _error("invalid_replacement", path=path)
    old_b, new_b = old.encode("utf-8"), new.encode("utf-8")
    if len(old_b) > _MAX_FRAGMENT_BYTES or len(new_b) > _MAX_FRAGMENT_BYTES:
        return None, _error("replacement_too_large", path=path)
    return {
        "operation": operation, "path": path, "summary": summary,
        "expected": expected.lower(), "old": old, "new": new,
        "old_sha256": _sha(old_b), "new_sha256": _sha(new_b),
    }, None


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


def _read_directory(sftp, path):
    attrs = sftp.lstat(path)
    if stat.S_ISLNK(attrs.st_mode):
        raise ValueError("parent_symlink_refused")
    if not stat.S_ISDIR(attrs.st_mode):
        raise ValueError("parent_not_directory")
    return attrs


def _is_missing(exc):
    return isinstance(exc, FileNotFoundError) or getattr(exc, "errno", None) == errno.ENOENT


def _target_absent(sftp, path):
    try:
        attrs = sftp.lstat(path)
    except Exception as exc:  # noqa: BLE001 — SFTP uses IOError(ENOENT) for absence
        if _is_missing(exc):
            return True, None
        raise
    if stat.S_ISLNK(attrs.st_mode):
        return False, "symlink_refused"
    return False, "target_already_exists"


def _resolved_target(sftp, path, target_exists):
    """Resolve ancestor symlinks and re-apply the recovery-boundary policy.

    Stable application symlinks remain usable. What is refused is a logical allowed path that
    resolves into a denied boot/login/SSH/network path. The resolved value is retained only in the
    private approval plan and compared again after approval to close the ancestor-symlink race.
    """
    if target_exists:
        resolved = sftp.normalize(path)
    else:
        directory, name = posixpath.split(path)
        resolved = posixpath.join(sftp.normalize(directory), name)
    if not isinstance(resolved, str) or not _valid_path(resolved):
        raise ValueError("resolved_path_not_allowed")
    return resolved


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


def _prepare_create(sftp, spec):
    absent, existing_error = _target_absent(sftp, spec["path"])
    if not absent:
        return _error(existing_error, path=spec["path"])
    directory = posixpath.dirname(spec["path"])
    parent = _read_directory(sftp, directory)
    resolved = _resolved_target(sftp, spec["path"], False)
    return {
        "ok": True, "operation": "create", "path": spec["path"],
        "change_summary": spec["summary"], "after_sha256": spec["after_sha256"],
        "mode": spec["mode_text"], "result_bytes": len(spec["data"]),
        "_data": spec["data"], "_mode": spec["mode"],
        "_parent_mode": stat.S_IMODE(parent.st_mode),
        "_parent_uid": parent.st_uid, "_parent_gid": parent.st_gid,
        "_resolved_path": resolved,
    }


def _prepare_replace(sftp, spec):
    attrs, data = _read_regular(sftp, spec["path"])
    resolved = _resolved_target(sftp, spec["path"], True)
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
    return {
        "ok": True, "operation": "replace_fragment", "path": spec["path"],
        "change_summary": spec["summary"], "before_sha256": before,
        "after_sha256": _sha(new_data), "old_text_sha256": spec["old_sha256"],
        "new_text_sha256": spec["new_sha256"], "replacement_count": 1,
        "result_bytes": len(new_data), "_data": new_data,
        "_mode": stat.S_IMODE(attrs.st_mode), "_uid": attrs.st_uid, "_gid": attrs.st_gid,
        "_resolved_path": resolved,
    }


def prepare_edit(conn, args, opener=ssh_transport.open_client):
    """Read and validate an edit plan. Private content remains only under underscore keys."""
    spec, err = _validated_args(args)
    if err:
        return err
    client, sftp, err = _open_sftp(conn, opener)
    if err:
        return err
    try:
        if spec["operation"] == "create":
            return _prepare_create(sftp, spec)
        return _prepare_replace(sftp, spec)
    except ValueError as exc:
        return _error(str(exc), path=spec["path"])
    except Exception as exc:  # noqa: BLE001
        error_class = "sftp_read_failed"
        if spec["operation"] == "create" and _is_missing(exc):
            error_class = "parent_not_found"
        return _error(error_class, path=spec["path"], detail=type(exc).__name__)
    finally:
        try:
            sftp.close()
        finally:
            client.close()


def public_plan(plan):
    return {key: value for key, value in plan.items() if not key.startswith("_")}


def confirmation_display(plan):
    if plan["operation"] == "create":
        return ("atomic_text_edit operation=create path=%s after_sha256=%s mode=%s summary=%s"
                % (plan["path"], plan["after_sha256"], plan["mode"], plan["change_summary"]))
    return ("atomic_text_edit operation=replace_fragment path=%s before_sha256=%s "
            "after_sha256=%s replacements=1 summary=%s" %
            (plan["path"], plan["before_sha256"], plan["after_sha256"],
             plan["change_summary"]))


def _write_new_file(sftp, path, data, mode, uid=None, gid=None):
    # Exclusive creation prevents a guessed temporary/backup name from being a symlink target.
    # Paramiko 2.8.1 maps `x` to CREATE|EXCL but, unlike Python open(), does not imply the SFTP
    # WRITE flag. Include `w` explicitly; EXCL still makes an existing path fail before truncation.
    with sftp.file(path, "wx") as handle:
        handle.write(data)
        handle.flush()
    sftp.chmod(path, mode)
    if uid is not None and gid is not None:
        sftp.chown(path, uid, gid)


def _parent_metadata_matches(attrs, plan):
    return (not stat.S_ISLNK(attrs.st_mode) and stat.S_ISDIR(attrs.st_mode)
            and stat.S_IMODE(attrs.st_mode) == plan["_parent_mode"]
            and attrs.st_uid == plan["_parent_uid"] and attrs.st_gid == plan["_parent_gid"])


def _apply_create(sftp, plan):
    path = plan["path"]
    directory, name = posixpath.split(path)
    nonce = uuid.uuid4().hex[:12]
    temporary = posixpath.join(directory, ".%s.sshops-tmp-%s" % (name, nonce))
    created = False
    rename_attempted = False
    stage = "recheck_preconditions"
    try:
        absent, existing_error = _target_absent(sftp, path)
        if not absent:
            return _error(existing_error + "_after_approval", path=path)
        parent = _read_directory(sftp, directory)
        if not _parent_metadata_matches(parent, plan):
            return _error("parent_changed_after_approval", path=path)
        if _resolved_target(sftp, path, False) != plan["_resolved_path"]:
            return _error("resolved_path_changed_after_approval", path=path)
        stage = "write_temporary"
        _write_new_file(sftp, temporary, plan["_data"], plan["_mode"])
        try:
            # Standard SFTP rename requires the destination not to exist. Do not use posix_rename:
            # its overwrite semantics would turn a create race into silent data loss.
            rename_attempted = True
            stage = "rename_no_overwrite"
            sftp.rename(temporary, path)
        except Exception as exc:  # noqa: BLE001
            try:
                absent, _ = _target_absent(sftp, path)
                if absent:
                    return _error("atomic_create_failed", path=path,
                                  box_may_be_changed=False, **_exception_fields(exc, stage))
                attrs, verified = _read_regular(sftp, path)
                actual, actual_mode = _sha(verified), stat.S_IMODE(attrs.st_mode)
                if actual == plan["after_sha256"] and actual_mode == plan["_mode"]:
                    created = True
                    return {
                        "ok": True, "operation": "create", "path": path,
                        "after_sha256": actual, "mode": "0%03o" % actual_mode,
                        "result_bytes": len(verified), "atomic": True,
                        "rename_outcome_recovered": True,
                    }
                return _error("atomic_create_outcome_unknown", path=path,
                              actual_sha256=actual, **_exception_fields(exc, stage),
                              actual_mode="0%03o" % actual_mode, box_may_be_changed=True)
            except ValueError:
                return _error("target_created_after_approval", path=path,
                              box_may_be_changed=False, **_exception_fields(exc, stage))
            except Exception as verify_exc:  # noqa: BLE001 — rename outcome is now unknowable
                return _error("atomic_create_outcome_unknown", path=path,
                              box_may_be_changed=True,
                              **_exception_fields(verify_exc, "verify_rename_outcome"))
        created = True
        stage = "verify_postcondition"
        attrs, verified = _read_regular(sftp, path)
        actual = _sha(verified)
        actual_mode = stat.S_IMODE(attrs.st_mode)
        if actual != plan["after_sha256"] or actual_mode != plan["_mode"]:
            return _error("postcondition_failed", path=path, actual_sha256=actual,
                          actual_mode="0%03o" % actual_mode, box_may_be_changed=True)
        return {
            "ok": True, "operation": "create", "path": path,
            "after_sha256": actual, "mode": "0%03o" % actual_mode,
            "result_bytes": len(verified), "atomic": True,
        }
    except ValueError as exc:
        return _error(str(exc), path=path, box_may_be_changed=created)
    except Exception as exc:  # noqa: BLE001
        return _error("atomic_create_failed", path=path,
                      box_may_be_changed=created or rename_attempted,
                      **_exception_fields(exc, stage))
    finally:
        if not created:
            try:
                sftp.remove(temporary)
            except Exception:  # noqa: BLE001
                pass


def _apply_replace(sftp, plan):
    path = plan["path"]
    directory, name = posixpath.split(path)
    nonce = uuid.uuid4().hex[:12]
    backup = posixpath.join(directory, ".%s.sshops-backup-%s-%s" %
                            (name, plan["before_sha256"][:12], nonce))
    temporary = posixpath.join(directory, ".%s.sshops-tmp-%s" % (name, nonce))
    replaced = False
    mutation_attempted = False
    backup_written = False
    preserve_backup = False
    try:
        attrs, current = _read_regular(sftp, path)
        if _resolved_target(sftp, path, True) != plan["_resolved_path"]:
            return _error("resolved_path_changed_after_approval", path=path)
        if _sha(current) != plan["before_sha256"]:
            return _error("stale_after_approval", path=path, actual_sha256=_sha(current))
        mode = stat.S_IMODE(attrs.st_mode)
        if mode != plan["_mode"] or attrs.st_uid != plan["_uid"] or attrs.st_gid != plan["_gid"]:
            return _error("metadata_changed_after_approval", path=path)
        _write_new_file(sftp, backup, current, mode, attrs.st_uid, attrs.st_gid)
        backup_written = True
        _write_new_file(sftp, temporary, plan["_data"], mode, attrs.st_uid, attrs.st_gid)
        mutation_attempted = True
        sftp.posix_rename(temporary, path)
        replaced = True
        _, verified = _read_regular(sftp, path)
        actual = _sha(verified)
        if actual != plan["after_sha256"]:
            raise RuntimeError("postcondition_failed")
        return {
            "ok": True, "operation": "replace_fragment", "path": path,
            "before_sha256": plan["before_sha256"], "after_sha256": actual,
            "replacement_count": 1, "result_bytes": len(verified),
            "backup_path": backup, "atomic": True,
        }
    except Exception as exc:  # noqa: BLE001
        if mutation_attempted and not replaced:
            try:
                attrs, observed = _read_regular(sftp, path)
                actual = _sha(observed)
                metadata_ok = (stat.S_IMODE(attrs.st_mode) == plan["_mode"]
                               and attrs.st_uid == plan["_uid"] and attrs.st_gid == plan["_gid"])
                if actual == plan["after_sha256"] and metadata_ok:
                    replaced = True
                    return {
                        "ok": True, "operation": "replace_fragment", "path": path,
                        "before_sha256": plan["before_sha256"], "after_sha256": actual,
                        "replacement_count": 1, "result_bytes": len(observed),
                        "backup_path": backup, "atomic": True,
                        "rename_outcome_recovered": True,
                    }
                if actual == plan["before_sha256"] and metadata_ok:
                    return _error("atomic_replace_failed", path=path,
                                  detail=type(exc).__name__, rolled_back=False,
                                  box_may_be_changed=False)
                preserve_backup = True
                return _error("atomic_replace_outcome_unknown", path=path,
                              detail=type(exc).__name__, actual_sha256=actual,
                              backup_path=backup, rolled_back=False, box_may_be_changed=True)
            except Exception as verify_exc:  # noqa: BLE001 — keep recovery evidence on ambiguity
                preserve_backup = True
                return _error("atomic_replace_outcome_unknown", path=path,
                              detail=type(verify_exc).__name__, backup_path=backup,
                              rolled_back=False, box_may_be_changed=True)
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
            for candidate in (temporary, backup if backup_written and not preserve_backup else ""):
                if not candidate:
                    continue
                try:
                    sftp.remove(candidate)
                except Exception:  # noqa: BLE001
                    pass


def apply_edit(conn, plan, opener=ssh_transport.open_client):
    """Re-check the approved operation, atomically apply it, and verify its postcondition."""
    if (not plan.get("ok") or plan.get("operation") not in _OPERATIONS
            or not isinstance(plan.get("_data"), bytes)):
        return _error("invalid_plan")
    client, sftp, err = _open_sftp(conn, opener)
    if err:
        return err
    try:
        if plan["operation"] == "create":
            return _apply_create(sftp, plan)
        return _apply_replace(sftp, plan)
    finally:
        try:
            sftp.close()
        finally:
            client.close()
