"""Test-only stdin/stdout pair that answers @@CONFIRM the way the Go supervisor does.

It deliberately does NOT stub `_request_confirm`. The id matching, the JSON shape and the
fail-closed branches are the part worth testing, and a stub would assert nothing about them: the
reply id is read back out of what the harness actually wrote, so a harness that emitted a malformed
request or forgot to flush fails here the same way it would fail against the real parent.
"""
import io
import json
import re
import sys

_CONFIRM_RE = re.compile(r"^@@CONFIRM (.+)$", re.M)


class ConfirmingIO:
    """Captures harness stdout; on readline() answers the LAST @@CONFIRM seen."""

    def __init__(self, decide, corrupt=None):
        self.out = io.StringIO()
        self.decide = decide          # callable(command) -> bool | (bool, terminal_reason)
        self.corrupt = corrupt        # callable(reply_dict) -> str, to forge a bad reply
        self.requests = []

    # --- stdout half ---
    def write(self, s):
        return self.out.write(s)

    def flush(self):
        pass

    # --- stdin half ---
    def readline(self):
        matches = _CONFIRM_RE.findall(self.out.getvalue())
        if not matches:
            return ""                 # nothing pending -> EOF, which must read as a denial
        req = json.loads(matches[-1])
        self.requests.append(req)
        decision = self.decide(req["command"])
        if isinstance(decision, tuple):
            approved, terminal_reason = decision
        else:
            approved = bool(decision)
            # A false response from this test double models an explicit user
            # choice. EOF/malformed replies below remain the ambiguous fallback.
            terminal_reason = "" if approved else "user_declined"
        reply = {"id": req["id"], "approved": bool(approved)}
        if terminal_reason:
            reply["terminal_reason"] = terminal_reason
        if self.corrupt is not None:
            return self.corrupt(reply)
        return json.dumps(reply) + "\n"


class _Swap:
    def __init__(self, io_obj):
        self.io = io_obj

    def __enter__(self):
        self._o, self._i = sys.stdout, sys.stdin
        sys.stdout, sys.stdin = self.io, self.io
        return self.io

    def __exit__(self, *a):
        sys.stdout, sys.stdin = self._o, self._i
        return False


def approving(decide=lambda cmd: True, corrupt=None):
    return _Swap(ConfirmingIO(decide, corrupt))
