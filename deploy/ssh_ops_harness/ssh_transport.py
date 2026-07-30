"""Production SSH transport for the harness wrapper.

Credentials come from a passed-in `conn` dict (NEVER os.environ — the Agent SDK passes the
wrapper's full environment into the spawned `claude` CLI, so a credential in env would leak there).
Each call dials a FRESH connection and closes it on return — no module-global client/singleton,
so the credential's lifetime is bounded to the call and nothing survives across tasks. Output is
capped and secret-scrubbed (incl. the literal credential) before it can reach the model.
"""
import shlex
import warnings

import guardrails

_MAX_OUTPUT = 16000
_DIAL_TIMEOUT = 15
_EXEC_TIMEOUT = 30
# The bound the REMOTE enforces on itself, deliberately shorter than the local one so the box kills
# the process before we stop waiting for it. See _bounded for why this exists at all.
_REMOTE_TIMEOUT = 25
_REMOTE_KILL_GRACE = 5


def _bounded(command: str) -> str:
    """Wrap `command` so the BOX enforces a time bound on it, not just us.

    _pump's timeout path calls chan.close() with the comment "stop the remote command from running
    on". It does not stop it. Closing an SSH channel closes the channel; without a pty sshd does not
    reliably signal the child, so the command keeps running with nowhere to write. Measured A/B on a
    live box with `grep -rn <pat> / | head -20`: local-close-only left 2 processes alive (the grep
    AND the head) still running 12s later, while the same command under `timeout` left 0.

    That leak is not merely untidy. A `grep -rn` over /model abandoned at 30s was still scanning 1.5
    HOURS later, and the next diagnosis of that same instance saw it, read it as concurrent
    modification, and stopped to ask the operator instead of performing the repair. We also cannot
    reclaim it: the channel it was born on is gone.

    Shape notes:
      * `bash`, not `sh`. Live runs use bash-isms (`${PIPESTATUS[0]}` appeared in a real run), and
        dash would silently mis-execute them.
      * GNU timeout puts the child in its own process group and signals the group, which is why the
        grandchild (`head`) dies too — verified, not assumed.
      * If `timeout` is absent the `if` does not exec and the ORIGINAL command runs verbatim after
        it, so behaviour on such a box is byte-identical to before this wrapper existed.
      * The wrapper is applied HERE, at the transport, and never to the string that was classified,
        audited, or shown on the consent card — the operator approves the model's own text and that
        exact text is what runs inside the wrapper.
      * Exit 124 is the bound firing. That is strictly more informative than the local timeout it
        replaces, which surfaced exit_code=None with no way to tell a hang from a crash.
    """
    quoted = shlex.quote(command)
    return (f"if command -v timeout >/dev/null 2>&1; then "
            f"exec timeout -k {_REMOTE_KILL_GRACE} {_REMOTE_TIMEOUT} bash -c {quoted}; fi; "
            f"{command}")


def _clip(text: str) -> str:
    """Cap oversized output while keeping BOTH ends.

    A head-only clip (`text[:_MAX_OUTPUT]`) is actively misleading on the single most valuable
    diagnostic artifact there is — a log file. `cat` of a 1.2MB service log delivered a window whose
    newest visible line was 9 months old, and three separate live runs then reported, correctly for
    what they were shown, "this service last ran in October" — which read as model fabrication but
    was ours. Logs are appended, so the TAIL carries the crash and the most recent timestamp; other
    output (config dumps, listings) is front-loaded. Keeping both ends serves both, and the elision
    is stated inline so the agent knows material is missing rather than inferring from a false end.
    """
    if len(text) <= _MAX_OUTPUT:
        return text
    half = _MAX_OUTPUT // 2
    dropped = len(text) - 2 * half
    return (f"{text[:half]}\n"
            f"...[{dropped} bytes elided — showing the first and last {half} bytes]...\n"
            f"{text[-half:]}")


def _dec(b: bytearray) -> str:
    return bytes(b).decode("utf-8", "replace")


def _pump(chan, deadline_s: float = None):
    """Drain a channel until the command exits, or `_EXEC_TIMEOUT` elapses.

    Reads WHILE waiting rather than waiting then reading: an un-drained channel fills its SSH
    window, which stalls the remote side and would turn a large-output read into a false timeout.
    Returns (stdout, stderr, timed_out).
    """
    import time

    limit = _EXEC_TIMEOUT if deadline_s is None else deadline_s
    deadline = time.monotonic() + limit
    out, err = bytearray(), bytearray()

    def drain():
        moved = False
        while chan.recv_ready():
            out.extend(chan.recv(65536))                   # extend, not `+=`: no rebinding in a closure
            moved = True
        while chan.recv_stderr_ready():
            err.extend(chan.recv_stderr(65536))
            moved = True
        return moved

    while True:
        moved = drain()
        if chan.exit_status_ready() and not chan.recv_ready() and not chan.recv_stderr_ready():
            drain()                                       # final sweep for bytes that raced the exit
            return out, err, False
        if time.monotonic() >= deadline:
            try:
                chan.close()                              # stop the remote command from running on
            except Exception:                             # noqa: BLE001
                pass
            return out, err, True
        if not moved:
            time.sleep(0.05)


def run_ssh(conn: dict, command: str, secrets=()) -> dict:
    """Run ONE command on `conn`. Returns a dict with redacted stdout/stderr, or {"error": class}.
    The credential is used only for the paramiko auth and is never logged or returned."""
    warnings.filterwarnings("ignore")
    import paramiko                                       # lazy: keeps this module importable offline

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    kwargs = dict(
        hostname=conn["host"], port=int(conn.get("port", 22)), username=conn["user"],
        timeout=_DIAL_TIMEOUT, banner_timeout=20, auth_timeout=20,
        look_for_keys=False, allow_agent=False,
    )
    if conn.get("key"):
        kwargs["key_filename"] = conn["key"]
    else:
        kwargs["password"] = conn.get("password")
    try:
        try:
            client.connect(**kwargs)
        except paramiko.AuthenticationException:
            return {"error": "auth_failed"}              # stale credential — NEVER echo it
        except Exception as e:                            # noqa: BLE001 — class name only, no detail/cred
            return {"error": "connect_failed", "detail": type(e).__name__}
        _, so, se = client.exec_command(_bounded(command), timeout=_EXEC_TIMEOUT)
        out_b, err_b, timed_out = _pump(so.channel)
        if timed_out:
            # The classifier refuses the KNOWN blockers (tail -f, top, watch...), but it cannot
            # prove a command terminates — a stdin-reading command, a hung mount, a wedged NFS
            # path all block forever. paramiko's exec_command(timeout=) does NOT bound
            # recv_exit_status(), so this used to hang until the supervisor's whole-run wall clock
            # fired: 3 of 9 live runs died at 12m having executed only 6-8 commands, with the
            # blocking command itself invisible (a step is emitted only after it returns).
            # Partial output is kept — a command that printed and then hung is still evidence.
            return {"error": "exec_timeout", "detail": f"{_EXEC_TIMEOUT}s",
                    "partial": guardrails.scrub_output(_clip(_dec(out_b)), secrets)}
        out, err = _dec(out_b), _dec(err_b)
        truncated = len(out) > _MAX_OUTPUT or len(err) > _MAX_OUTPUT
        exit_code = so.channel.recv_exit_status()
        if exit_code == 124:
            # 124 is what `timeout` returns when the bound fires. Say so: without this the model sees
            # a bare non-zero exit on a command that printed partial output and reads it as the
            # command having FAILED, then re-runs the same unbounded search. (A command can return
            # 124 on its own, which is why this is worded as the likely cause rather than a fact.)
            err = (err.rstrip() + "\n" if err.strip() else "") + (
                f"[exit 124 —— 通常是远端 {_REMOTE_TIMEOUT}s 上限触发的超时，不是命令本身报错。"
                f"上面的输出是超时前已产生的部分结果；请缩小范围（限定目录、加 -maxdepth）后重试]")
        return {
            "exit_code": exit_code,
            "stdout": guardrails.scrub_output(_clip(out), secrets),
            "stderr": guardrails.scrub_output(_clip(err), secrets),
            "truncated": truncated,
        }
    finally:
        try:
            client.close()
        except Exception:                                 # noqa: BLE001
            pass
