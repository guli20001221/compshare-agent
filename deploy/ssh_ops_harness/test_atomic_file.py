"""Local fake-SFTP tests for the hash-bound atomic file tool."""
import hashlib
import os
import shutil
import stat
import tempfile

import atomic_file

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


class _Attrs:
    def __init__(self, path):
        value = os.lstat(path)
        self.st_mode, self.st_size = value.st_mode, value.st_size
        self.st_uid, self.st_gid = getattr(value, "st_uid", 0), getattr(value, "st_gid", 0)


class _SFTP:
    @staticmethod
    def _local(path):
        return os.path.join(root, path.lstrip("/").replace("/", os.sep))

    def lstat(self, path):
        return _Attrs(self._local(path))

    def file(self, path, mode):
        local = self._local(path)
        os.makedirs(os.path.dirname(local), exist_ok=True)
        return open(local, mode)  # noqa: PTH123 — local test double only

    def chmod(self, path, mode):
        os.chmod(self._local(path), mode)

    def chown(self, path, uid, gid):
        if hasattr(os, "chown"):
            os.chown(self._local(path), uid, gid)

    def posix_rename(self, source, target):
        os.replace(self._local(source), self._local(target))

    def remove(self, path):
        os.remove(self._local(path))

    def close(self):
        pass


class _Client:
    def open_sftp(self):
        return _SFTP()

    def close(self):
        pass


def _open(_conn):
    return _Client(), None


root = tempfile.mkdtemp(prefix="sshops-atomic-test-")
try:
    path = "/workspace/app.conf"
    local_path = _SFTP._local(path)
    os.makedirs(os.path.dirname(local_path), exist_ok=True)
    with open(local_path, "wb") as handle:
        handle.write(b"port=8080\nmode=prod\n")
    os.chmod(local_path, 0o640)
    before = hashlib.sha256(open(local_path, "rb").read()).hexdigest()
    args = {"path": path, "expected_sha256": before, "old_text": "port=8080",
            "new_text": "port=8188", "change_summary": "align the configured listener"}
    plan = atomic_file.prepare_replace({}, args, opener=_open)
    check("plan-is-hash-bound", plan["ok"] is True and plan["before_sha256"] == before)
    public = atomic_file.public_plan(plan)
    check("public-plan-hides-file-content", "_data" not in public and "port=8188" not in str(public))
    display = atomic_file.confirmation_display(plan)
    check("confirmation-binds-path-and-hashes-not-content",
          path in display and before in display and "port=8080" not in display and "port=8188" not in display)
    result = atomic_file.apply_replace({}, plan, opener=_open)
    check("replace-is-atomic-and-verified", result["ok"] is True and result["atomic"] is True)
    check("file-has-exact-change", open(local_path, encoding="utf-8").read() == "port=8188\nmode=prod\n")
    check("mode-is-preserved", os.name == "nt" or stat.S_IMODE(os.stat(local_path).st_mode) == 0o640)
    check("recoverable-backup-is-retained",
          open(_SFTP._local(result["backup_path"]), "rb").read() == b"port=8080\nmode=prod\n")

    raced_path = "/workspace/raced.conf"
    raced_local = _SFTP._local(raced_path)
    with open(raced_local, "wb") as handle:
        handle.write(b"workers=1\n")
    raced_before = hashlib.sha256(b"workers=1\n").hexdigest()
    raced_plan = atomic_file.prepare_replace(
        {}, dict(args, path=raced_path, expected_sha256=raced_before,
                 old_text="workers=1", new_text="workers=2"), opener=_open)
    with open(raced_local, "wb") as handle:
        handle.write(b"workers=3\n")
    raced_result = atomic_file.apply_replace({}, raced_plan, opener=_open)
    check("change-after-approval-is-refused",
          raced_result["ok"] is False
          and raced_result["error_class"] == "stale_after_approval")
    check("concurrent-writer-content-is-preserved",
          open(raced_local, "rb").read() == b"workers=3\n")

    stale = atomic_file.prepare_replace({}, args, opener=_open)
    check("stale-hash-is-refused", stale["ok"] is False and stale["error_class"] == "stale_precondition")
    duplicate_path = "/workspace/duplicate.conf"
    with open(_SFTP._local(duplicate_path), "wb") as handle:
        handle.write(b"x=1\nx=1\n")
    duplicate_args = dict(args, path=duplicate_path,
                          expected_sha256=hashlib.sha256(b"x=1\nx=1\n").hexdigest(),
                          old_text="x=1", new_text="x=2")
    duplicate = atomic_file.prepare_replace({}, duplicate_args, opener=_open)
    check("ambiguous-match-is-refused",
          duplicate["ok"] is False and duplicate["error_class"] == "match_count_not_one")
    denied = atomic_file.prepare_replace({}, dict(args, path="/etc/ssh/sshd_config"), opener=_open)
    check("critical-ssh-file-is-refused-before-connect",
          denied["ok"] is False and denied["error_class"] == "path_not_allowed")
    check("main-sudoers-file-remains-a-recovery-boundary",
          atomic_file._valid_path("/etc/sudoers") is False)
    check("removable-sudoers-drop-in-remains-confirmable",
          atomic_file._valid_path("/etc/sudoers.d/90-compshare") is True)
    check("hash-bound-state-file-edit-remains-recoverable",
          atomic_file._valid_path("/var/lib/example/app.conf") is True)

    symlink_path = "/workspace/link.conf"
    symlink_local = _SFTP._local(symlink_path)
    try:
        os.symlink(local_path, symlink_local)
    except (OSError, NotImplementedError):
        print("-- symlink-refused: SKIPPED (host does not permit symlink creation)")
    else:
        linked = atomic_file.prepare_replace(
            {}, dict(args, path=symlink_path,
                     expected_sha256=hashlib.sha256(open(local_path, "rb").read()).hexdigest(),
                     old_text="port=8188", new_text="port=8288"), opener=_open)
        check("symlink-is-refused",
              linked["ok"] is False and linked["error_class"] == "symlink_refused")
finally:
    shutil.rmtree(root, ignore_errors=True)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("atomic_file: ALL GREEN")
