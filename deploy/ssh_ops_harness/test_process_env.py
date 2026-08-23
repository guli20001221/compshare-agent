"""Local fake-SFTP tests for selected remote process environment inspection."""

import os
import shutil
import tempfile

import process_env

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


class _SFTP:
    @staticmethod
    def _local(path):
        return os.path.join(root, path.lstrip("/").replace("/", os.sep))

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
    raise AssertionError("invalid input must fail before SSH is opened")


root = tempfile.mkdtemp(prefix="sshops-process-env-test-")
try:
    local = _SFTP._local("/proc/63/environ")
    os.makedirs(os.path.dirname(local), exist_ok=True)
    fake_key = "sk-test-" + ("q" * 24)
    with open(local, "wb") as handle:
        handle.write(
            b"CUDA_VISIBLE_DEVICES=0\0CONDA_PREFIX=/root/miniconda3\0"
            + ("UNRELATED_SECRET=" + fake_key).encode() + b"\0WORLD_SIZE=4\0")

    result = process_env.read({}, {
        "pid": 63,
        "names": ["CUDA_VISIBLE_DEVICES", "CONDA_PREFIX", "NVIDIA_VISIBLE_DEVICES"],
    }, opener=_open)
    check("returns-only-requested-allowlisted-values",
          result == {"ok": True, "pid": 63,
                     "values": {"CUDA_VISIBLE_DEVICES": "0",
                                "CONDA_PREFIX": "/root/miniconda3"},
                     "missing": ["NVIDIA_VISIBLE_DEVICES"]})
    check("unrequested-secret-never-returns", fake_key not in repr(result))
    check("schema-does-not-accept-arbitrary-names",
          process_env._validated_args({"pid": 63, "names": ["AWS_SECRET_ACCESS_KEY"]})[1]
          ["error_class"] == "invalid_names")
    check("duplicates-are-rejected",
          process_env._validated_args({"pid": 63, "names": ["RANK", "RANK"]})[1]
          ["error_class"] == "invalid_names")
    invalid = process_env.read({}, {"pid": "63", "names": ["RANK"]}, opener=_unexpected_open)
    check("invalid-pid-fails-before-connect", invalid["error_class"] == "invalid_pid")
    missing = process_env.read({}, {"pid": 999, "names": ["RANK"]}, opener=_open)
    check("missing-process-is-explicit", missing["error_class"] == "process_not_found")

    with open(local, "wb") as handle:
        handle.write(b"PYTHONPATH=" + b"x" * (process_env._MAX_ENV_BYTES + 1))
    oversized = process_env.read({}, {"pid": 63, "names": ["PYTHONPATH"]}, opener=_open)
    check("environment-read-is-bounded", oversized["error_class"] == "environment_too_large")
finally:
    shutil.rmtree(root, ignore_errors=True)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("process_env: ALL GREEN")
