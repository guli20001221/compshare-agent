"""Local fake-SFTP tests for bounded remote text inspection."""
import hashlib
import os
import shutil
import stat
import tempfile

import remote_text

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


class _Attrs:
    def __init__(self, path):
        value = os.lstat(path)
        self.st_mode, self.st_size = value.st_mode, value.st_size


class _SFTP:
    @staticmethod
    def _local(path):
        return os.path.join(root, path.lstrip("/").replace("/", os.sep))

    def lstat(self, path):
        return _Attrs(self._local(path))

    def file(self, path, mode):
        return open(self._local(path), mode)  # noqa: PTH123 — local test double only

    def close(self):
        pass


class _Client:
    def open_sftp(self):
        return _SFTP()

    def close(self):
        pass


def _open(_conn):
    return _Client(), None


def _unexpected_open(_conn):
    raise AssertionError("a rejected path must fail before SSH is opened")


root = tempfile.mkdtemp(prefix="sshops-remote-text-test-")
try:
    path = "/workspace/app/config.ini"
    local_path = _SFTP._local(path)
    os.makedirs(os.path.dirname(local_path), exist_ok=True)
    fake_key = "sk-test-" + ("a" * 16)
    data = f"port=8188\napi_key={fake_key}\nmode=prod\n".encode()
    with open(local_path, "wb") as handle:
        handle.write(data)

    result = remote_text.read({}, {"path": path, "line_start": 1, "line_count": 2},
                              opener=_open)
    check("read-returns-whole-file-hash",
          result["ok"] is True and result["sha256"] == hashlib.sha256(data).hexdigest())
    check("read-returns-requested-line-window",
          result["line_start"] == 1 and result["line_end"] == 2 and result["next_line"] == 3)
    check("read-redacts-credential-shaped-content",
          fake_key not in result["content"] and "REDACTED" in result["content"])
    tail = remote_text.read({}, {"path": path, "line_start": 3, "line_count": 20}, opener=_open)
    check("read-can-continue-by-line", tail["ok"] is True and tail["content"] == "mode=prod\n"
          and tail["truncated"] is False and tail["next_line"] is None)

    check("ordinary-root-application-config-is-readable",
          remote_text._valid_path("/root/application/config.ini") is True)
    check("known-secret-files-remain-a-boundary",
          all(remote_text._valid_path(candidate) is False for candidate in
              ("/root/application/.env", "/root/.ssh/id_rsa", "/etc/shadow",
               "/workspace/app/.env.production", "/home/user/.docker/config.json",
               "/workspace/app/client.key", "/proc/1/environ")))
    denied = remote_text.read({}, {"path": "/root/application/.env"}, opener=_unexpected_open)
    check("secret-path-is-refused-before-connect",
          denied["ok"] is False and denied["error_class"] == "path_not_allowed")

    binary_path = "/workspace/app/binary.conf"
    with open(_SFTP._local(binary_path), "wb") as handle:
        handle.write(b"valid\xffinvalid")
    binary = remote_text.read({}, {"path": binary_path}, opener=_open)
    check("non-utf8-file-is-refused-with-hash",
          binary["ok"] is False and binary["error_class"] == "not_utf8"
          and len(binary["sha256"]) == 64)

    oversized_path = "/workspace/app/oversized.txt"
    with open(_SFTP._local(oversized_path), "wb") as handle:
        handle.write(b"x" * (remote_text._MAX_FILE_BYTES + 1))
    oversized = remote_text.read({}, {"path": oversized_path}, opener=_open)
    check("oversized-file-is-refused",
          oversized["ok"] is False and oversized["error_class"] == "file_too_large")

    long_line_path = "/workspace/app/one-long-line.txt"
    with open(_SFTP._local(long_line_path), "wb") as handle:
        handle.write(b"z" * (remote_text._MAX_RETURN_BYTES + 100))
    long_line = remote_text.read({}, {"path": long_line_path}, opener=_open)
    check("return-is-byte-bounded-even-for-one-long-line",
          long_line["ok"] is True and long_line["returned_bytes"] <= remote_text._MAX_RETURN_BYTES
          and long_line["line_end"] == 1 and long_line["line_too_long"] is True
          and long_line["next_line"] is None)

    symlink_path = "/workspace/app/link.ini"
    try:
        os.symlink(local_path, _SFTP._local(symlink_path))
    except (OSError, NotImplementedError):
        print("-- symlink-refused: SKIPPED (host does not permit symlink creation)")
    else:
        linked = remote_text.read({}, {"path": symlink_path}, opener=_open)
        check("symlink-is-refused",
              linked["ok"] is False and linked["error_class"] == "symlink_refused")

    out_of_range = remote_text.read({}, {"path": path, "line_start": 99}, opener=_open)
    check("out-of-range-line-is-explicit",
          out_of_range["ok"] is False and out_of_range["error_class"] == "line_start_out_of_range"
          and out_of_range["total_lines"] == 3)
finally:
    shutil.rmtree(root, ignore_errors=True)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("remote_text: ALL GREEN")
