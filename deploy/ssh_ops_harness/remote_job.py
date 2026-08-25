"""Structured start/poll protocol for remote work that exceeds one SSH call's time bound."""
import re
import shlex
import time
import uuid

import guardrails
import ssh_transport

_JOB_ROOT = "/tmp/compshare-ops-jobs"
_JOB_ID = re.compile(r"^job-[0-9a-f]{32}$")
_RETURN_LOG_BYTES = 16000
_SHELLS = {"sh", "bash", "dash", "zsh", "ksh"}

POLL_DESCRIPTION = (
    "Read the state and bounded stdout/stderr tail of a background job previously returned by "
    "ssh_exec with run_in_background=true. This is read-only. It accepts only the currently active "
    "opaque job-NNN ID and cannot read "
    "an arbitrary path. Set wait_seconds (normally 15-30) so the tool waits before checking instead "
    "of burning model turns in a tight loop. Read-only diagnosis and separately approved foreground "
    "changes remain available while it runs, but a second background job is refused until a poll "
    "observes a terminal state. After six running polls, stop and report partial repair, the job_id, "
    "progress and pending "
    "verification; a later turn resumes rather than restarts it. "
    "Repeated calls return only new log bytes plus total byte counts and log_progress. `running` means the recorded PID still exists; `succeeded` means exit "
    "code zero; `failed` includes the non-zero exit code; `interrupted` means no completion record "
    "and no process (for example after a restart).")


def poll_schema():
    return {
        "type": "object",
        "properties": {
            "job_id": {"type": "string", "pattern": "^job-[0-9a-f]{32}$"},
            "wait_seconds": {"type": "integer", "minimum": 0, "maximum": 30,
                             "default": 15,
                             "description": "Bounded wait before checking; use 15-30 for ongoing work."},
        },
        "required": ["job_id"],
        "additionalProperties": False,
    }


def confirmation_display(command, purpose):
    purpose = " ".join(str(purpose or "").split())[:200]
    return "ssh_exec run_in_background=true purpose=%s command=%s" % (purpose, command)


def command_is_self_backgrounding(command):
    """Reject a payload that tries to detach itself inside the managed job wrapper.

    If the payload backgrounds its real work, the wrapper records the shell's early success while
    the useful process is still running and `poll_background_job` lies.  Foreground pipes and
    chains remain valid.  Shell `-c` payloads are checked recursively because quoting them does not
    make their ampersand literal to the inner shell.
    """
    try:
        lexer = shlex.shlex(str(command or ""), posix=True, punctuation_chars=";&|<>")
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return True
    if "&" in tokens:
        return True
    if tokens and tokens[0].rsplit("/", 1)[-1] in _SHELLS:
        for i, token in enumerate(tokens[:-1]):
            if token == "-c" and command_is_self_backgrounding(tokens[i + 1]):
                return True
    return False


def new_job_id():
    return "job-" + uuid.uuid4().hex


def start(conn, command, purpose, secrets=(), runner=ssh_transport.run_ssh, job_id=None):
    command = str(command or "").strip()
    purpose = " ".join(str(purpose or "").split())[:200]
    if not command or len(command) > 1500 or not purpose or command_is_self_backgrounding(command):
        return {"ok": False, "error_class": "invalid_arguments", "box_may_be_changed": False}
    if job_id is None:
        job_id = new_job_id()
    if not isinstance(job_id, str) or not _JOB_ID.fullmatch(job_id):
        return {"ok": False, "error_class": "invalid_job_id", "box_may_be_changed": False}
    directory = _JOB_ROOT + "/" + job_id
    status_tmp, status = directory + "/status.tmp", directory + "/status"
    # The original command runs in a subshell so an `exit` inside it cannot skip the completion
    # record. The trusted wrapper is generated locally after policy/confirmation; it is never model
    # input and is not what the audit displays. stdout/stderr are detached from the SSH session.
    #
    # Do NOT impose RLIMIT_FSIZE here. A limit inherited by the payload applies to every file it
    # writes, not merely stdout/stderr: large wheels, model shards and compiler outputs are cut off
    # with SIGXFSZ and left partial. poll() bounds what crosses the model boundary without changing
    # the approved job's filesystem semantics.
    body = ("( %s ); rc=$?; printf '%%s\\n' \"$rc\" > %s; mv %s %s" %
            (command, shlex.quote(status_tmp), shlex.quote(status_tmp), shlex.quote(status)))
    launcher = (
        "umask 077; mkdir -p %s || exit $?; "
        "nohup env COMPSHARE_OPS_JOB_ID=%s bash -c %s </dev/null >%s 2>%s & job_pid=$!; "
        "printf '%%s\\n' \"$job_pid\" >%s" %
        (shlex.quote(directory), shlex.quote(job_id), shlex.quote(body), shlex.quote(directory + "/stdout.log"),
         shlex.quote(directory + "/stderr.log"), shlex.quote(directory + "/pid")))
    try:
        result = runner(conn, launcher, secrets=secrets)
    except Exception as exc:  # noqa: BLE001 — after approval, a lost result has unknown outcome
        return {"ok": False, "job_id": job_id, "error_class": "launcher_outcome_unknown",
                "detail": type(exc).__name__, "box_may_be_changed": True,
                "poll_with": "poll_background_job"}
    if result.get("error"):
        may_have_started = result["error"] == "exec_timeout"
        response = {"ok": False, "error_class": result["error"],
                    "detail": result.get("detail", ""), "box_may_be_changed": may_have_started}
        if may_have_started:
            response.update({"job_id": job_id, "poll_with": "poll_background_job"})
        return response
    if result.get("exit_code") != 0:
        return {"ok": False, "job_id": job_id, "error_class": "launcher_failed",
                "exit_code": result.get("exit_code"), "box_may_be_changed": True,
                "poll_with": "poll_background_job"}
    return {"ok": True, "job_id": job_id, "state": "started", "purpose": purpose,
            "poll_with": "poll_background_job"}


def _read(sftp, path, limit, tail=False):
    size = None
    if tail:
        try:
            size = int(sftp.lstat(path).st_size)
        except Exception:  # noqa: BLE001 — fall back to a bounded prefix on unusual SFTP servers
            size = None
    with sftp.file(path, "rb") as handle:
        if size is not None and size > limit:
            handle.seek(size - limit)
            data = handle.read(limit)
            return data, True
        data = handle.read(limit + 1)
    return data[:limit], len(data) > limit


def _read_optional(sftp, path, limit, tail=False):
    try:
        return _read(sftp, path, limit, tail=tail)
    except Exception:  # noqa: BLE001 — absent/unreadable is represented without remote detail
        return b"", False


def _read_optional_since(sftp, path, limit, offset):
    """Read a bounded tail on first observation, then only bytes appended after ``offset``."""
    try:
        size = int(sftp.lstat(path).st_size)
        if offset is None or not isinstance(offset, int) or offset < 0 or offset > size:
            data, cut = _read(sftp, path, limit, tail=True)
            return data, cut, size
        start = offset
        cut = size - start > limit
        if cut:
            start = size - limit
        with sftp.file(path, "rb") as handle:
            handle.seek(start)
            return handle.read(limit), cut, size
    except Exception:  # noqa: BLE001 — absent/unreadable is represented without remote detail
        return b"", False, 0


def _bounded_text(data, secrets):
    text = data.decode("utf-8", "replace")
    return guardrails.scrub_output(text, secrets)


def poll(conn, job_id, secrets=(), wait_seconds=0, offsets=None,
         opener=ssh_transport.open_client, sleeper=time.sleep):
    if not isinstance(job_id, str) or not _JOB_ID.fullmatch(job_id):
        return {"ok": False, "error_class": "invalid_job_id"}
    if (not isinstance(wait_seconds, int) or isinstance(wait_seconds, bool)
            or wait_seconds < 0 or wait_seconds > 30):
        return {"ok": False, "job_id": job_id, "error_class": "invalid_wait_seconds"}
    if wait_seconds:
        sleeper(wait_seconds)
    client, connect_error = opener(conn)
    if connect_error:
        return {"ok": False, "job_id": job_id, "error_class": connect_error.get("error", "connect_failed"),
                "detail": connect_error.get("detail", "")}
    sftp = None
    try:
        sftp = client.open_sftp()
        directory = _JOB_ROOT + "/" + job_id
        try:
            attrs = sftp.lstat(directory)
            if not attrs:
                raise FileNotFoundError()
        except Exception:  # noqa: BLE001
            return {"ok": False, "job_id": job_id, "error_class": "job_not_found"}

        status_b, _ = _read_optional(sftp, directory + "/status", 32)
        pid_b, _ = _read_optional(sftp, directory + "/pid", 32)
        status_text = status_b.decode("ascii", "ignore").strip()
        pid_text = pid_b.decode("ascii", "ignore").strip()
        exit_code = None
        if re.fullmatch(r"-?\d+", status_text):
            exit_code = int(status_text)
            state = "succeeded" if exit_code == 0 else "failed"
        elif re.fullmatch(r"\d+", pid_text):
            try:
                sftp.lstat("/proc/%s/stat" % pid_text)
                environ_b, _ = _read_optional(sftp, "/proc/%s/environ" % pid_text, 4096)
                marker = ("COMPSHARE_OPS_JOB_ID=" + job_id).encode("ascii")
                state = "running" if marker in environ_b.split(b"\0") else "interrupted"
            except Exception:  # noqa: BLE001
                state = "interrupted"
        else:
            state = "interrupted"

        # Split the bounded response evenly between both streams; logs themselves remain on-box.
        offsets = offsets if isinstance(offsets, dict) else {}
        stdout_b, stdout_cut, stdout_total = _read_optional_since(
            sftp, directory + "/stdout.log", _RETURN_LOG_BYTES // 2, offsets.get("stdout"))
        stderr_b, stderr_cut, stderr_total = _read_optional_since(
            sftp, directory + "/stderr.log", _RETURN_LOG_BYTES // 2, offsets.get("stderr"))
        log_progress = (stdout_total != offsets.get("stdout") or
                        stderr_total != offsets.get("stderr"))
        result = {"ok": True, "job_id": job_id, "state": state,
                  "stdout": _bounded_text(stdout_b, secrets),
                  "stderr": _bounded_text(stderr_b, secrets),
                  "stdout_truncated": stdout_cut, "stderr_truncated": stderr_cut,
                  "stdout_bytes_total": stdout_total, "stderr_bytes_total": stderr_total,
                  "log_progress": log_progress}
        if exit_code is not None:
            result["exit_code"] = exit_code
        return result
    except Exception as exc:  # noqa: BLE001
        return {"ok": False, "job_id": job_id, "error_class": "job_status_failed",
                "detail": type(exc).__name__}
    finally:
        if sftp is not None:
            try:
                sftp.close()
            except Exception:  # noqa: BLE001
                pass
        client.close()
