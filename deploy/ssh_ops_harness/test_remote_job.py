"""Offline protocol tests for structured long jobs."""
import json
import re
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
check("trusted-wrapper-records-exit-and-caps-logs",
      "ulimit -f" in captured[0] and "status.tmp" in captured[0] and "pip install vllm" in captured[0])
check("trusted-wrapper-marks-process-identity",
      "COMPSHARE_OPS_JOB_ID=" + started["job_id"] in captured[0])
check("result-never-returns-generated-shell", "pip install" not in json.dumps(started))
check("self-backgrounding-payload-is-refused",
      remote_job.start({}, "pip install vllm &", "bad detach", runner=_runner)["error_class"] ==
      "invalid_arguments")
check("nested-self-backgrounding-payload-is-refused",
      remote_job.command_is_self_backgrounding("bash -c 'sleep 2 &'") is True)
check("foreground-chain-remains-supported",
      remote_job.command_is_self_backgrounding("apt-get update && apt-get install -y jq") is False)


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
check("poll-reports-completed-exit", done["ok"] and done["state"] == "completed" and done["exit_code"] == 0)
check("poll-returns-bounded-logs", done["stdout"] == "installed\n")
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
