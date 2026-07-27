"""Production SSH transport for the harness wrapper.

Credentials come from a passed-in `conn` dict (NEVER os.environ — the Agent SDK passes the
wrapper's full environment into the spawned `claude` CLI, so a credential in env would leak there).
Each call dials a FRESH connection and closes it on return — no module-global client/singleton,
so the credential's lifetime is bounded to the call and nothing survives across tasks. Output is
capped and secret-scrubbed (incl. the literal credential) before it can reach the model.
"""
import warnings

import guardrails

_MAX_OUTPUT = 16000
_DIAL_TIMEOUT = 15
_EXEC_TIMEOUT = 30


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
        _, so, se = client.exec_command(command, timeout=_EXEC_TIMEOUT)
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
        return {
            "exit_code": so.channel.recv_exit_status(),
            "stdout": guardrails.scrub_output(_clip(out), secrets),
            "stderr": guardrails.scrub_output(_clip(err), secrets),
            "truncated": truncated,
        }
    finally:
        try:
            client.close()
        except Exception:                                 # noqa: BLE001
            pass
