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
        rc = so.channel.recv_exit_status()
        out = so.read().decode("utf-8", "replace")
        err = se.read().decode("utf-8", "replace")
        truncated = len(out) > _MAX_OUTPUT or len(err) > _MAX_OUTPUT
        return {
            "exit_code": rc,
            "stdout": guardrails.scrub_output(out[:_MAX_OUTPUT], secrets),
            "stderr": guardrails.scrub_output(err[:_MAX_OUTPUT], secrets),
            "truncated": truncated,
        }
    finally:
        try:
            client.close()
        except Exception:                                 # noqa: BLE001
            pass
