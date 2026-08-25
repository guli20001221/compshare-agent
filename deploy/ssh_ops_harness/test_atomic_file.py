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
    _modes = {}

    @staticmethod
    def _local(path):
        return os.path.join(root, path.lstrip("/").replace("/", os.sep))

    def lstat(self, path):
        attrs = _Attrs(self._local(path))
        if path in self._modes:
            attrs.st_mode = (attrs.st_mode & ~0o777) | self._modes[path]
        return attrs

    def normalize(self, path):
        local = os.path.realpath(self._local(path))
        base = os.path.realpath(root)
        relative = os.path.relpath(local, base)
        if relative == ".":
            return "/"
        return "/" + relative.replace(os.sep, "/")

    def file(self, path, mode):
        local = self._local(path)
        os.makedirs(os.path.dirname(local), exist_ok=True)
        return open(local, mode)  # noqa: PTH123 — local test double only

    def chmod(self, path, mode):
        os.chmod(self._local(path), mode)
        self._modes[path] = mode

    def chown(self, path, uid, gid):
        if hasattr(os, "chown"):
            os.chown(self._local(path), uid, gid)

    def posix_rename(self, source, target):
        os.replace(self._local(source), self._local(target))
        if source in self._modes:
            self._modes[target] = self._modes.pop(source)

    def rename(self, source, target):
        if os.path.lexists(self._local(target)):
            raise FileExistsError(self._local(target))
        os.rename(self._local(source), self._local(target))
        if source in self._modes:
            self._modes[target] = self._modes.pop(source)

    def remove(self, path):
        os.remove(self._local(path))
        self._modes.pop(path, None)

    def close(self):
        pass


class _Client:
    def open_sftp(self):
        return _SFTP()

    def close(self):
        pass


def _open(_conn):
    return _Client(), None


class _RenameRaceSFTP(_SFTP):
    def rename(self, source, target):
        with open(self._local(target), "xb") as handle:
            handle.write(b"won-race\n")
        super().rename(source, target)


class _RenameRaceClient:
    def open_sftp(self):
        return _RenameRaceSFTP()

    def close(self):
        pass


def _open_rename_race(_conn):
    return _RenameRaceClient(), None


class _ResolvedBoundaryRaceSFTP(_SFTP):
    def normalize(self, path):
        return "/etc/ssh" if path.endswith("/workspace") else super().normalize(path)


class _ResolvedBoundaryRaceClient:
    def open_sftp(self):
        return _ResolvedBoundaryRaceSFTP()

    def close(self):
        pass


def _open_resolved_boundary_race(_conn):
    return _ResolvedBoundaryRaceClient(), None


class _RenameResponseLostSFTP(_SFTP):
    def rename(self, source, target):
        super().rename(source, target)
        raise ConnectionError("response lost after create rename")

    def posix_rename(self, source, target):
        super().posix_rename(source, target)
        raise ConnectionError("response lost after replace rename")


class _RenameResponseLostClient:
    def open_sftp(self):
        return _RenameResponseLostSFTP()

    def close(self):
        pass


def _open_rename_response_lost(_conn):
    return _RenameResponseLostClient(), None


class _RenameOutcomeUnverifiableSFTP(_SFTP):
    def __init__(self):
        self._rename_lost = False

    def lstat(self, path):
        if self._rename_lost:
            raise ConnectionError("cannot verify rename outcome")
        return super().lstat(path)

    def rename(self, source, target):
        super().rename(source, target)
        self._rename_lost = True
        raise ConnectionError("response and connection lost after create rename")

    def posix_rename(self, source, target):
        super().posix_rename(source, target)
        self._rename_lost = True
        raise ConnectionError("response and connection lost after replace rename")


class _RenameOutcomeUnverifiableClient:
    def open_sftp(self):
        return _RenameOutcomeUnverifiableSFTP()

    def close(self):
        pass


def _open_rename_outcome_unverifiable(_conn):
    return _RenameOutcomeUnverifiableClient(), None


root = tempfile.mkdtemp(prefix="sshops-atomic-test-")
try:
    path = "/workspace/app.conf"
    local_path = _SFTP._local(path)
    os.makedirs(os.path.dirname(local_path), exist_ok=True)
    with open(local_path, "wb") as handle:
        handle.write(b"port=8080\nmode=prod\n")
    os.chmod(local_path, 0o640)
    before = hashlib.sha256(open(local_path, "rb").read()).hexdigest()
    args = {"operation": "replace_fragment", "path": path,
            "expected_sha256": before, "old_text": "port=8080",
            "new_text": "port=8188", "change_summary": "align the configured listener"}
    plan = atomic_file.prepare_edit({}, args, opener=_open)
    check("plan-is-hash-bound", plan["ok"] is True and plan["before_sha256"] == before)
    public = atomic_file.public_plan(plan)
    check("public-plan-hides-file-content", "_data" not in public and "port=8188" not in str(public))
    display = atomic_file.confirmation_display(plan)
    check("confirmation-binds-path-and-hashes-not-content",
          path in display and before in display and "port=8080" not in display and "port=8188" not in display)
    result = atomic_file.apply_edit({}, plan, opener=_open)
    check("replace-is-atomic-and-verified", result["ok"] is True and result["atomic"] is True)
    check("file-has-exact-change", open(local_path, encoding="utf-8").read() == "port=8188\nmode=prod\n")
    check("mode-is-preserved", os.name == "nt" or stat.S_IMODE(os.stat(local_path).st_mode) == 0o640)
    check("recoverable-backup-is-retained",
          open(_SFTP._local(result["backup_path"]), "rb").read() == b"port=8080\nmode=prod\n")

    lost_replace_path = "/workspace/lost-replace.conf"
    lost_replace_local = _SFTP._local(lost_replace_path)
    with open(lost_replace_local, "wb") as handle:
        handle.write(b"x=1\n")
    lost_replace_plan = atomic_file.prepare_edit({}, {
        "operation": "replace_fragment", "path": lost_replace_path,
        "expected_sha256": hashlib.sha256(b"x=1\n").hexdigest(),
        "old_text": "x=1", "new_text": "x=2", "change_summary": "change test value",
    }, opener=_open)
    lost_replace_result = atomic_file.apply_edit(
        {}, lost_replace_plan, opener=_open_rename_response_lost)
    check("replace-response-loss-recovers-the-applied-postcondition",
          lost_replace_result["ok"] is True
          and lost_replace_result["rename_outcome_recovered"] is True
          and open(lost_replace_local, "rb").read() == b"x=2\n")
    check("replace-response-loss-retains-the-recoverable-backup",
          open(_SFTP._local(lost_replace_result["backup_path"]), "rb").read() == b"x=1\n")

    unknown_replace_path = "/workspace/unknown-replace.conf"
    unknown_replace_local = _SFTP._local(unknown_replace_path)
    with open(unknown_replace_local, "wb") as handle:
        handle.write(b"x=1\n")
    unknown_replace_plan = atomic_file.prepare_edit({}, {
        "operation": "replace_fragment", "path": unknown_replace_path,
        "expected_sha256": hashlib.sha256(b"x=1\n").hexdigest(),
        "old_text": "x=1", "new_text": "x=2", "change_summary": "change test value",
    }, opener=_open)
    unknown_replace_result = atomic_file.apply_edit(
        {}, unknown_replace_plan, opener=_open_rename_outcome_unverifiable)
    check("unverifiable-replace-reports-possible-change-and-keeps-backup",
          unknown_replace_result["ok"] is False
          and unknown_replace_result["error_class"] == "atomic_replace_outcome_unknown"
          and unknown_replace_result["box_may_be_changed"] is True
          and open(_SFTP._local(unknown_replace_result["backup_path"]), "rb").read() == b"x=1\n")

    raced_path = "/workspace/raced.conf"
    raced_local = _SFTP._local(raced_path)
    with open(raced_local, "wb") as handle:
        handle.write(b"workers=1\n")
    raced_before = hashlib.sha256(b"workers=1\n").hexdigest()
    raced_plan = atomic_file.prepare_edit(
        {}, dict(args, path=raced_path, expected_sha256=raced_before,
                 old_text="workers=1", new_text="workers=2"), opener=_open)
    with open(raced_local, "wb") as handle:
        handle.write(b"workers=3\n")
    raced_result = atomic_file.apply_edit({}, raced_plan, opener=_open)
    check("change-after-approval-is-refused",
          raced_result["ok"] is False
          and raced_result["error_class"] == "stale_after_approval")
    check("concurrent-writer-content-is-preserved",
          open(raced_local, "rb").read() == b"workers=3\n")

    stale = atomic_file.prepare_edit({}, args, opener=_open)
    check("stale-hash-is-refused", stale["ok"] is False and stale["error_class"] == "stale_precondition")
    duplicate_path = "/workspace/duplicate.conf"
    with open(_SFTP._local(duplicate_path), "wb") as handle:
        handle.write(b"x=1\nx=1\n")
    duplicate_args = dict(args, path=duplicate_path,
                          expected_sha256=hashlib.sha256(b"x=1\nx=1\n").hexdigest(),
                          old_text="x=1", new_text="x=2")
    duplicate = atomic_file.prepare_edit({}, duplicate_args, opener=_open)
    check("ambiguous-match-is-refused",
          duplicate["ok"] is False and duplicate["error_class"] == "match_count_not_one")
    denied = atomic_file.prepare_edit({}, dict(args, path="/etc/ssh/sshd_config"), opener=_open)
    check("critical-ssh-file-is-refused-before-connect",
          denied["ok"] is False and denied["error_class"] == "path_not_allowed")
    check("main-sudoers-file-remains-a-recovery-boundary",
          atomic_file._valid_path("/etc/sudoers") is False)
    check("removable-sudoers-drop-in-remains-confirmable",
          atomic_file._valid_path("/etc/sudoers.d/90-compshare") is True)
    check("hash-bound-state-file-edit-remains-recoverable",
          atomic_file._valid_path("/var/lib/example/app.conf") is True)

    # A harmless-looking logical path must not use an ancestor symlink to cross into an SSH/login
    # recovery boundary. Canonical resolution is checked both before and after approval.
    denied_dir = _SFTP._local("/etc/ssh")
    os.makedirs(denied_dir, exist_ok=True)
    ancestor_link = _SFTP._local("/workspace/redirect")
    try:
        os.symlink(_SFTP._local("/etc"), ancestor_link, target_is_directory=True)
    except (OSError, NotImplementedError):
        print("-- ancestor-symlink-boundary: SKIPPED (host does not permit symlink creation)")
    else:
        ancestor_create = atomic_file.prepare_edit({}, {
            "operation": "create", "path": "/workspace/redirect/ssh/new.conf",
            "content": "x=1\n", "mode": "0644", "change_summary": "test canonical boundary",
        }, opener=_open)
        check("ancestor-symlink-cannot-bypass-a-denied-create-path",
              ancestor_create["ok"] is False
              and ancestor_create["error_class"] == "resolved_path_not_allowed")
        denied_existing = os.path.join(denied_dir, "existing.conf")
        with open(denied_existing, "wb") as handle:
            handle.write(b"x=1\n")
        ancestor_replace = atomic_file.prepare_edit({}, {
            "operation": "replace_fragment", "path": "/workspace/redirect/ssh/existing.conf",
            "expected_sha256": hashlib.sha256(b"x=1\n").hexdigest(),
            "old_text": "x=1", "new_text": "x=2", "change_summary": "test canonical boundary",
        }, opener=_open)
        check("ancestor-symlink-cannot-bypass-a-denied-replace-path",
              ancestor_replace["ok"] is False
              and ancestor_replace["error_class"] == "resolved_path_not_allowed")

    symlink_path = "/workspace/link.conf"
    symlink_local = _SFTP._local(symlink_path)
    try:
        os.symlink(local_path, symlink_local)
    except (OSError, NotImplementedError):
        print("-- symlink-refused: SKIPPED (host does not permit symlink creation)")
    else:
        linked = atomic_file.prepare_edit(
            {}, dict(args, path=symlink_path,
                     expected_sha256=hashlib.sha256(open(local_path, "rb").read()).hexdigest(),
                     old_text="port=8188", new_text="port=8288"), opener=_open)
        check("symlink-is-refused",
              linked["ok"] is False and linked["error_class"] == "symlink_refused")

    create_path = "/workspace/managed-start.sh"
    create_args = {
        "operation": "create", "path": create_path,
        "content": "#!/bin/sh\nexec python3 /workspace/app.py\n", "mode": "0750",
        "change_summary": "add the requested application launcher",
    }
    create_plan = atomic_file.prepare_edit({}, create_args, opener=_open)
    check("create-plan-is-bounded-and-hides-content",
          create_plan["ok"] is True and create_plan["operation"] == "create"
          and "exec python3" not in str(atomic_file.public_plan(create_plan)))
    create_display = atomic_file.confirmation_display(create_plan)
    check("create-card-binds-path-hash-and-mode-not-content",
          "operation=create" in create_display and create_path in create_display
          and create_plan["after_sha256"] in create_display and "mode=0750" in create_display
          and "exec python3" not in create_display)
    create_result = atomic_file.apply_edit({}, create_plan, opener=_open)
    create_local = _SFTP._local(create_path)
    check("create-is-atomic-and-verified",
          create_result["ok"] is True and create_result["atomic"] is True
          and create_result["after_sha256"] == create_plan["after_sha256"])
    check("create-writes-exact-content-with-requested-mode",
          open(create_local, encoding="utf-8").read() == create_args["content"]
          and (os.name == "nt" or stat.S_IMODE(os.stat(create_local).st_mode) == 0o750))

    lost_create_path = "/workspace/lost-create.conf"
    lost_create_plan = atomic_file.prepare_edit(
        {}, dict(create_args, path=lost_create_path, content="created-value\n"), opener=_open)
    lost_create_result = atomic_file.apply_edit(
        {}, lost_create_plan, opener=_open_rename_response_lost)
    check("create-response-loss-recovers-the-applied-postcondition",
          lost_create_result["ok"] is True
          and lost_create_result["rename_outcome_recovered"] is True
          and open(_SFTP._local(lost_create_path), "rb").read() == b"created-value\n")

    unknown_create_path = "/workspace/unknown-create.conf"
    unknown_create_plan = atomic_file.prepare_edit(
        {}, dict(create_args, path=unknown_create_path, content="created-value\n"), opener=_open)
    unknown_create_result = atomic_file.apply_edit(
        {}, unknown_create_plan, opener=_open_rename_outcome_unverifiable)
    check("unverifiable-create-reports-possible-change",
          unknown_create_result["ok"] is False
          and unknown_create_result["error_class"] == "atomic_create_outcome_unknown"
          and unknown_create_result["box_may_be_changed"] is True)
    existing_create = atomic_file.prepare_edit({}, create_args, opener=_open)
    check("create-never-overwrites-an-existing-target",
          existing_create["ok"] is False
          and existing_create["error_class"] == "target_already_exists")

    raced_create_path = "/workspace/raced-create.conf"
    raced_create_args = dict(create_args, path=raced_create_path, content="agent-value\n")
    raced_create_plan = atomic_file.prepare_edit({}, raced_create_args, opener=_open)
    with open(_SFTP._local(raced_create_path), "wb") as handle:
        handle.write(b"concurrent-value\n")
    raced_create_result = atomic_file.apply_edit({}, raced_create_plan, opener=_open)
    check("create-race-is-refused-after-approval",
          raced_create_result["ok"] is False
          and raced_create_result["error_class"] == "target_already_exists_after_approval")
    check("create-race-preserves-the-concurrent-file",
          open(_SFTP._local(raced_create_path), "rb").read() == b"concurrent-value\n")

    resolved_race_path = "/workspace/resolved-race.conf"
    resolved_race_plan = atomic_file.prepare_edit(
        {}, dict(create_args, path=resolved_race_path, content="agent-value\n"), opener=_open)
    resolved_race_result = atomic_file.apply_edit(
        {}, resolved_race_plan, opener=_open_resolved_boundary_race)
    check("ancestor-resolution-is-rechecked-after-approval",
          resolved_race_result["ok"] is False
          and resolved_race_result["error_class"] == "resolved_path_not_allowed"
          and resolved_race_result["box_may_be_changed"] is False)
    check("resolved-path-race-does-not-create-the-target",
          not os.path.lexists(_SFTP._local(resolved_race_path)))

    rename_race_path = "/workspace/rename-race.conf"
    rename_race_plan = atomic_file.prepare_edit(
        {}, dict(create_args, path=rename_race_path, content="agent-value\n"), opener=_open)
    rename_race_result = atomic_file.apply_edit(
        {}, rename_race_plan, opener=_open_rename_race)
    check("create-rename-race-is-detected-without-overwrite",
          rename_race_result["ok"] is False
          and rename_race_result["error_class"] == "target_created_after_approval"
          and rename_race_result["box_may_be_changed"] is False)
    check("create-rename-race-leaves-the-winner-intact",
          open(_SFTP._local(rename_race_path), "rb").read() == b"won-race\n")

    missing_parent = atomic_file.prepare_edit(
        {}, dict(create_args, path="/missing/child.conf"), opener=_open)
    check("create-requires-an-existing-parent",
          missing_parent["ok"] is False and missing_parent["error_class"] == "parent_not_found")
    check("create-requires-an-absolute-normalized-path",
          atomic_file.prepare_edit({}, dict(create_args, path="workspace/x"), opener=_open)["error_class"]
          == "path_not_allowed")
    check("create-requires-an-explicit-ordinary-mode",
          atomic_file.prepare_edit({}, dict(create_args, path="/workspace/bad-mode", mode="4755"),
                                   opener=_open)["error_class"] == "invalid_mode")

    parent_link = "/workspace-link"
    parent_link_local = _SFTP._local(parent_link)
    try:
        os.symlink(_SFTP._local("/workspace"), parent_link_local, target_is_directory=True)
    except (OSError, NotImplementedError):
        print("-- create-parent-symlink-refused: SKIPPED (host does not permit symlink creation)")
    else:
        parent_link_result = atomic_file.prepare_edit(
            {}, dict(create_args, path=parent_link + "/child.conf"), opener=_open)
        check("create-parent-symlink-is-refused",
              parent_link_result["ok"] is False
              and parent_link_result["error_class"] == "parent_symlink_refused")
finally:
    shutil.rmtree(root, ignore_errors=True)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("atomic_file: ALL GREEN")
