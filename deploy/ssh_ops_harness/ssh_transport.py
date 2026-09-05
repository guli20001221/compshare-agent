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
# Capture is bounded independently of the smaller model-visible display. Keep
# enough context to redact complete lines before selecting their first/last bytes.
_MAX_CAPTURE_BYTES = 256 * 1024
_DIAL_TIMEOUT = 15
_EXEC_TIMEOUT = 30
# The bound the REMOTE enforces on itself, deliberately shorter than the local one so the box kills
# the process before we stop waiting for it. See _bounded for why this exists at all.
_REMOTE_TIMEOUT = 25
_REMOTE_KILL_GRACE = 5


def _bounded(command: str) -> str:
    """Wrap `command` so the BOX enforces a time bound on it, not just us.

    Closing an SSH channel does not reliably signal a non-pty child. GNU timeout bounds the remote
    process group so descendants cannot outlive the channel.

    Shape notes:
      * `bash`, not `sh`, because generated commands may use bash syntax.
      * GNU timeout puts the child in its own process group and signals the group, which is why the
        grandchild (`head`) dies too — verified, not assumed.
      * If `timeout` is absent the `if` does not exec and the ORIGINAL command runs verbatim after
        it, so behaviour on such a box is byte-identical to before this wrapper existed.
      * The wrapper is applied at transport, not to the classified/audited command.
        The model's exact command text runs inside the task-scoped wrapper.
      * Exit 124 is the bound firing. That is strictly more informative than the local timeout it
        replaces, which surfaced exit_code=None with no way to tell a hang from a crash.
    """
    quoted = shlex.quote(command)
    return (f"if command -v timeout >/dev/null 2>&1; then "
            f"exec timeout -k {_REMOTE_KILL_GRACE} {_REMOTE_TIMEOUT} bash -c {quoted}; fi; "
            f"{command}")


def _clip(text: str) -> str:
    """Cap oversized output while keeping BOTH ends.

    Logs are tail-heavy while configuration and listings are often head-heavy. Keep both ends and
    mark the elision so the agent does not infer a false beginning or end.
    """
    if len(text) <= _MAX_OUTPUT:
        return text
    half = _MAX_OUTPUT // 2
    dropped = len(text) - 2 * half
    return (f"{text[:half]}\n"
            f"...[{dropped} bytes elided — showing the first and last {half} bytes]...\n"
            f"{text[-half:]}")


def _scrub_and_clip(text: str, secrets=()) -> str:
    """Remove complete known secrets before a bounded view can split them."""
    return _clip(guardrails.scrub_output(text, secrets))


class _BoundedOutput:
    """Drain every byte without retaining an unbounded log in the shared runner."""

    def __init__(self):
        self.head = bytearray()
        self.tail = bytearray()
        self.total = 0

    def append(self, data):
        self.total += len(data)
        split = min(len(data), _MAX_CAPTURE_BYTES // 2 - len(self.head))
        self.head.extend(data[:split])
        self.tail.extend(data[split:])
        del self.tail[:max(0, len(self.tail) - _MAX_CAPTURE_BYTES // 2)]

    @property
    def truncated(self):
        return self.total > _MAX_CAPTURE_BYTES

    def text(self, secrets=()):
        head = self.head.decode("utf-8", "replace")
        tail = self.tail.decode("utf-8", "replace")
        if not self.truncated:
            # Decode once so a UTF-8 character crossing the capture split survives.
            return _scrub_and_clip(bytes(self.head + self.tail).decode("utf-8", "replace"), secrets)
        head, tail = guardrails.scrub_output_fragments(head, tail, secrets)
        return _clip(head + f"\n...[output exceeded {_MAX_CAPTURE_BYTES} byte capture; "
                     f"{self.total} bytes received; middle and cut lines omitted]...\n" + tail)


def _pump(chan, deadline_s: float = None):
    """Drain a channel until the command exits, or `_EXEC_TIMEOUT` elapses.

    Reads WHILE waiting rather than waiting then reading: an un-drained channel fills its SSH
    window, which stalls the remote side and would turn a large-output read into a false timeout.
    Returns (stdout, stderr, timed_out).
    """
    import time

    limit = _EXEC_TIMEOUT if deadline_s is None else deadline_s
    deadline = time.monotonic() + limit
    out, err = _BoundedOutput(), _BoundedOutput()

    while True:
        if time.monotonic() >= deadline:
            try:
                chan.close()                              # stop the remote command from running on
            except Exception:                             # noqa: BLE001
                pass
            return out, err, True
        # One batch per stream per iteration: continuous stdout cannot starve
        # stderr or hide the deadline inside an unbounded recv_ready loop.
        moved = False
        if chan.recv_ready():
            out.append(chan.recv(65536))
            moved = True
        if chan.recv_stderr_ready():
            err.append(chan.recv_stderr(65536))
            moved = True
        # An SSH server can send exit-status before the last data/EOF packets.
        # An empty ready buffer is only a temporary gap, not stream completion.
        if (chan.exit_status_ready() and (chan.eof_received or chan.closed)
                and not chan.recv_ready() and not chan.recv_stderr_ready()):
            return out, err, False
        if not moved:
            time.sleep(0.05)


def open_client(conn: dict):
    """Open one credential-bounded SSH client, returning ``(client, error_dict)``.

    Structured SFTP operations share this exact authentication boundary with shell execution.  The
    error contains only a stable class; destination and credential are never returned or logged.
    The caller owns and must close a successful client.
    """
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
        client.connect(**kwargs)
        return client, None
    except paramiko.AuthenticationException:
        client.close()
        return None, {"error": "auth_failed"}             # stale credential — NEVER echo it
    except Exception as e:                                # noqa: BLE001 — class name only, no detail/cred
        client.close()
        return None, {"error": "connect_failed", "detail": type(e).__name__}


def run_ssh(conn: dict, command: str, secrets=()) -> dict:
    """Run ONE command on `conn`. Returns a dict with redacted stdout/stderr, or {"error": class}.
    The credential is used only for the paramiko auth and is never logged or returned."""
    client, connect_error = open_client(conn)
    if connect_error:
        return connect_error
    try:
        _, so, se = client.exec_command(_bounded(command), timeout=_EXEC_TIMEOUT)
        out_b, err_b, timed_out = _pump(so.channel)
        if timed_out:
            # The classifier refuses the KNOWN blockers (tail -f, top, watch...), but it cannot
            # prove a command terminates — a stdin-reading command, a hung mount, a wedged NFS
            # path all block forever. paramiko's exec_command(timeout=) does not bound
            # recv_exit_status(), so the transport must enforce this deadline.
            # Partial output is kept — a command that printed and then hung is still evidence.
            return {"error": "exec_timeout", "detail": f"{_EXEC_TIMEOUT}s",
                    "partial": out_b.text(secrets), "partial_stderr": err_b.text(secrets),
                    "truncated": out_b.truncated or err_b.truncated}
        out, err = out_b.text(secrets), err_b.text(secrets)
        truncated = (out_b.truncated or err_b.truncated or
                     out_b.total > _MAX_OUTPUT or err_b.total > _MAX_OUTPUT)
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
            "stdout": out,
            "stderr": err,
            "truncated": truncated,
        }
    finally:
        try:
            client.close()
        except Exception:                                 # noqa: BLE001
            pass
