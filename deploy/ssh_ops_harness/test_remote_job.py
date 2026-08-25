"""Offline protocol tests for structured long jobs."""
import json
import os
import re
import shutil
import subprocess
import tempfile
import types

import remote_job

FAILS = []


def check(name, condition):
    if not condition:
        FAILS.append(name)
        print("XX ", name)


captured = []


def _runner(_conn, command, secrets=()):
    captured.append(command)
    return {"exit_code": 0, "stdout": "", "stderr": "", "truncated": False}


started = remote_job.start({}, "pip install vllm", "install the requested serving runtime",
                           runner=_runner)
check("start-returns-opaque-job-id", started["ok"] is True and re.fullmatch(r"job-[0-9a-f]{32}", started["job_id"]))
check("trusted-wrapper-detaches-all-streams",
      all(piece in captured[0] for piece in
          ("nohup env", "bash -c", "</dev/null", "stdout.log", "stderr.log", "pid")))
check("trusted-wrapper-records-exit-without-limiting-payload-files",
      "ulimit -f" not in captured[0] and "status.tmp" in captured[0] and
      "pip install vllm" in captured[0] and "log_limit_bytes_each" not in started)
check("trusted-wrapper-marks-process-identity",
      "COMPSHARE_OPS_JOB_ID=" + started["job_id"] in captured[0])
check("result-never-returns-generated-shell", "pip install" not in json.dumps(started))
multiline_payload = "cat <<'PY'\nhello from heredoc\nPY\n# trailing comment"
body = remote_job._completion_body(multiline_payload, "/tmp/status.tmp", "/tmp/status")
check("arbitrary-command-is-one-nested-shell-argument",
      body.startswith("bash -c " + remote_job.shlex.quote(multiline_payload) + "; rc=$?;"))
if os.name != "nt" and shutil.which("bash"):
    with tempfile.TemporaryDirectory(prefix="sshops-remote-job-") as tmp:
        status_tmp = os.path.join(tmp, "status.tmp")
        status = os.path.join(tmp, "status")
        executed = subprocess.run(
            ["bash", "-c", remote_job._completion_body(multiline_payload, status_tmp, status)],
            capture_output=True, text=True, check=False)
        status_text = ""
        if os.path.exists(status):
            with open(status, encoding="utf-8") as handle:
                status_text = handle.read().strip()
        check("heredoc-and-trailing-comment-preserve-completion-record",
              executed.returncode == 0 and executed.stdout == "hello from heredoc\n" and
              status_text == "0")
else:
    print("--  heredoc-and-trailing-comment-preserve-completion-record SKIPPED (bash absent)")
fixed_job_id = "job-" + "b" * 32
fixed_commands = []
fixed = remote_job.start(
    {}, "python3 -m pip install torch", "install torch",
    runner=lambda _conn, command, secrets=():
        fixed_commands.append(command) or {"exit_code": 0, "stdout": "", "stderr": ""},
    job_id=fixed_job_id)
check("caller-can-publish-the-opaque-id-before-launch",
      fixed.get("job_id") == fixed_job_id and
      "COMPSHARE_OPS_JOB_ID=" + fixed_job_id in fixed_commands[0])
check("self-backgrounding-payload-is-refused",
      remote_job.start({}, "pip install vllm &", "bad detach", runner=_runner)["error_class"] ==
      "invalid_arguments")
check("nested-self-backgrounding-payload-is-refused",
      remote_job.command_is_self_backgrounding("bash -c 'sleep 2 &'") is True)
check("combined-shell-options-cannot-hide-self-backgrounding",
      remote_job.command_is_self_backgrounding("bash -lc 'sleep 2 & wait'") is True and
      remote_job.command_is_self_backgrounding("/bin/sh -ec 'sleep 2 & wait'") is True)
check("env-prefix-cannot-hide-self-backgrounding",
      remote_job.command_is_self_backgrounding("env bash -lc 'sleep 2 & wait'") is True and
      remote_job.command_is_self_backgrounding("/usr/bin/env -i FOO=1 bash -lc 'sleep 2 & wait'") is True and
      remote_job.command_is_self_backgrounding("FOO=1 bash -lc 'sleep 2 & wait'") is True)
check("env-split-string-cannot-hide-self-backgrounding",
      remote_job.command_is_self_backgrounding("env -S 'bash -lc \\\"sleep 2 & wait\\\"'") is True and
      remote_job.command_is_self_backgrounding(
          "env -S 'FOO=1 bash -lc \\\"sleep 2 & wait\\\"'") is True and
      remote_job.command_is_self_backgrounding(
          "env --split-string='FOO=1 bash -lc \\\"sleep 2 & wait\\\"'") is True)
check("chains-and-transparent-wrappers-cannot-hide-self-backgrounding",
      remote_job.command_is_self_backgrounding(
          "cd /workspace && bash -lc 'sleep 2 & wait'") is True and
      remote_job.command_is_self_backgrounding(
          "timeout 10 bash -lc 'sleep 2 & wait'") is True and
      remote_job.command_is_self_backgrounding(
          "nohup bash -lc 'sleep 2 & wait'") is True)
check("foreground-chain-remains-supported",
      remote_job.command_is_self_backgrounding("apt-get update && apt-get install -y jq") is False and
      remote_job.command_is_self_backgrounding("bash -lc 'printf ready'") is False and
      remote_job.command_is_self_backgrounding("env FOO=1 python3 app.py") is False and
      remote_job.command_is_self_backgrounding(
          "cd /workspace && bash -lc 'printf ready'") is False)
check("approval-display-binds-background-mode-purpose-and-command",
      remote_job.confirmation_display("python3 app.py", "start the requested app") ==
      "ssh_exec run_in_background=true purpose=start the requested app command=python3 app.py")
check("poll-schema-supports-bounded-wait",
      remote_job.poll_schema()["properties"]["wait_seconds"]["maximum"] == 30 and
      "tight loop" in remote_job.POLL_DESCRIPTION and
      "currently active" in remote_job.POLL_DESCRIPTION)


class _File:
    def __init__(self, data):
        self.data = data
        self.offset = 0

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        pass

    def read(self, limit):
        data = self.data[self.offset:self.offset + limit]
        self.offset += len(data)
        return data

    def seek(self, offset):
        self.offset = offset


class _SFTP:
    def __init__(self, files, running=True):
        self.files, self.running = files, running

    def lstat(self, path):
        if path.endswith("/stat") and not self.running:
            raise FileNotFoundError()
        if path not in self.files and "/proc/" not in path and not path.endswith(started["job_id"]):
            raise FileNotFoundError()
        return types.SimpleNamespace(st_mode=0, st_size=len(self.files.get(path, b"")))

    def file(self, path, _mode):
        if path not in self.files:
            raise FileNotFoundError()
        return _File(self.files[path])

    def close(self):
        pass


class _Client:
    def __init__(self, sftp):
        self.sftp = sftp

    def open_sftp(self):
        return self.sftp

    def close(self):
        pass


base = "/tmp/compshare-ops-jobs/" + started["job_id"]
files = {base + "/status": b"0\n", base + "/pid": b"123\n",
         base + "/stdout.log": b"installed\n", base + "/stderr.log": b""}
done = remote_job.poll({}, started["job_id"], opener=lambda _c: (_Client(_SFTP(files)), None))
check("poll-reports-successful-exit", done["ok"] and done["state"] == "succeeded" and done["exit_code"] == 0)
check("poll-returns-bounded-logs", done["stdout"] == "installed\n")
failed_files = dict(files)
failed_files[base + "/status"] = b"153\n"
failed = remote_job.poll({}, started["job_id"], opener=lambda _c: (_Client(_SFTP(failed_files)), None))
check("poll-distinguishes-nonzero-exit-from-success",
      failed["ok"] and failed["state"] == "failed" and failed["exit_code"] == 153)
long_files = dict(files)
long_files[base + "/stdout.log"] = b"old-line\n" + b"x" * remote_job._RETURN_LOG_BYTES + b"new-line\n"
tailed = remote_job.poll({}, started["job_id"], opener=lambda _c: (_Client(_SFTP(long_files)), None))
check("poll-returns-log-tail-not-stale-prefix",
      tailed["stdout_truncated"] is True and tailed["stdout"].endswith("new-line\n") and
      "old-line" not in tailed["stdout"])
check("poll-rejects-path-instead-of-job-id",
      remote_job.poll({}, "../../etc/shadow", opener=lambda _c: (None, {}))["error_class"] == "invalid_job_id")

running_files = {base + "/pid": b"123\n", base + "/stdout.log": b"working\n",
                 base + "/stderr.log": b"",
                 "/proc/123/environ": ("COMPSHARE_OPS_JOB_ID=" + started["job_id"]).encode() + b"\0"}
running = remote_job.poll({}, started["job_id"],
                          opener=lambda _c: (_Client(_SFTP(running_files)), None))
check("poll-verifies-running-process-marker", running["ok"] and running["state"] == "running")
large_environment_files = dict(running_files)
large_environment_files["/proc/123/environ"] = (
    b"PAD=" + b"x" * 5000 + b"\0" +
    ("COMPSHARE_OPS_JOB_ID=" + started["job_id"]).encode() + b"\0")
large_environment = remote_job.poll(
    {}, started["job_id"], opener=lambda _c: (_Client(_SFTP(large_environment_files)), None))
check("marker-after-four-kib-still-proves-running-process",
      large_environment["ok"] and large_environment["state"] == "running")
truncated_environment_files = dict(running_files)
truncated_environment_files["/proc/123/environ"] = (
    b"PAD=" + b"x" * (remote_job._PROCESS_IDENTITY_BYTES + 1) + b"\0")
truncated_environment = remote_job.poll(
    {}, started["job_id"], opener=lambda _c: (_Client(_SFTP(truncated_environment_files)), None))
check("truncated-private-identity-never-falsely-releases-the-slot",
      truncated_environment["ok"] and truncated_environment["state"] == "unknown")
unreadable_identity_files = dict(running_files)
unreadable_identity_files.pop("/proc/123/environ")
unreadable_identity = remote_job.poll(
    {}, started["job_id"], opener=lambda _c: (_Client(_SFTP(unreadable_identity_files)), None))
check("unreadable-existing-process-identity-never-falsely-releases-the-slot",
      unreadable_identity["ok"] and unreadable_identity["state"] == "unknown")
waited = []
remote_job.poll({}, started["job_id"], wait_seconds=7,
                opener=lambda _c: (_Client(_SFTP(running_files)), None), sleeper=waited.append)
check("poll-enforces-requested-bounded-wait", waited == [7])
appended_files = dict(running_files)
appended_files[base + "/stdout.log"] = b"working\nmore\n"
delta = remote_job.poll(
    {}, started["job_id"], offsets={"stdout": len(b"working\n"), "stderr": 0},
    opener=lambda _c: (_Client(_SFTP(appended_files)), None))
check("repeated-poll-returns-only-new-log-bytes",
      delta["stdout"] == "more\n" and delta["log_progress"] is True and
      delta["stdout_bytes_total"] == len(b"working\nmore\n"))
unchanged = remote_job.poll(
    {}, started["job_id"], offsets={"stdout": len(b"working\nmore\n"), "stderr": 0},
    opener=lambda _c: (_Client(_SFTP(appended_files)), None))
check("repeated-poll-marks-unchanged-log",
      unchanged["stdout"] == "" and unchanged["log_progress"] is False)
check("poll-rejects-out-of-range-wait",
      remote_job.poll({}, started["job_id"], wait_seconds=31,
                      opener=lambda _c: (None, {}))["error_class"] == "invalid_wait_seconds")
reused_files = dict(running_files)
reused_files["/proc/123/environ"] = b"PATH=/usr/bin\0"
reused = remote_job.poll({}, started["job_id"],
                         opener=lambda _c: (_Client(_SFTP(reused_files)), None))
check("poll-does-not-misread-reused-pid-as-running", reused["state"] == "interrupted")

unknown = remote_job.start({}, "pip install vllm", "simulate lost launcher response",
                           runner=lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("lost")))
check("lost-launcher-response-is-unknown-not-clean-failure",
      unknown["error_class"] == "launcher_outcome_unknown" and unknown["box_may_be_changed"] and
      "job_id" in unknown)

if FAILS:
    print(f"\n{len(FAILS)} FAILED: {', '.join(FAILS)}")
    raise SystemExit(1)
print("remote_job: ALL GREEN")
