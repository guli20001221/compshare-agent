"""paramiko-backed SSH executor for the spike.

Reads credentials from env (populated from the gitignored .env.local). Applies an
output cap + redaction: the box's output is untrusted, so it is bounded and
secret-scrubbed before it can ever reach the model or a log.
"""
import os
import warnings
warnings.filterwarnings("ignore")
import paramiko
from guardrails import redact

_MAX_OUTPUT = 16000   # cap captured output (blast radius / OOM / injection surface)
_TIMEOUT_S = 30
_client = None


def _connect():
    global _client
    if _client is not None:
        t = _client.get_transport()
        if t is not None and t.is_active():
            return _client
    host = os.environ.get("SSH_HOST", "117.50.185.80")
    user = os.environ.get("SSH_USER", "ubuntu")
    c = paramiko.SSHClient()
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    kwargs = dict(hostname=host, port=int(os.environ.get("SSH_PORT", "22")),
                  username=user, timeout=15, banner_timeout=20, auth_timeout=20,
                  look_for_keys=False, allow_agent=False)
    key_path = os.environ.get("SSH_KEY_PATH")
    if key_path:
        kwargs["key_filename"] = key_path
    else:
        kwargs["password"] = os.environ.get("SSH_PASSWORD")
    c.connect(**kwargs)
    _client = c
    return c


def run(command: str) -> dict:
    """Execute one command on the box. Guardrail classification is the caller's job."""
    c = _connect()
    _, so, se = c.exec_command(command, timeout=_TIMEOUT_S)
    rc = so.channel.recv_exit_status()
    out = so.read().decode("utf-8", "replace")
    err = se.read().decode("utf-8", "replace")
    truncated = len(out) > _MAX_OUTPUT or len(err) > _MAX_OUTPUT
    return {
        "exit_code": rc,
        "stdout": redact(out[:_MAX_OUTPUT]),
        "stderr": redact(err[:_MAX_OUTPUT]),
        "truncated": truncated,
    }


def close():
    global _client
    if _client is not None:
        try:
            _client.close()
        finally:
            _client = None
