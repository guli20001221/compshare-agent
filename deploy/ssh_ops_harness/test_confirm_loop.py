"""Offline gate for the per-write confirmation loop (no network / no SSH / no SDK).

Run:  python test_confirm_loop.py   ->  exits non-zero on ANY failure.

Why this loop exists at all: the lane-level card the user clicks authorizes ENTERING the box. It
never names what will change, so it cannot be the consent for `kill 6934`. A live paired run showed
the gap concretely — the model repaired four of four faults, and among the commands it ran were
`kill 6934`, `sed -i` on a user's source file and `systemctl restart`. The guardrail cannot tell a
squatting process from a training job three days in. The operator can. Measured cost of asking:
1-3 requests per repair, because 20-45 of the commands in a run are reads.

The property under test is therefore not "it asks" but "it cannot proceed unless a matching,
affirmative answer came back" — every other outcome must read as a refusal.
"""
import sys

import confirm_stub
import guardrails
import harness
import ssh_transport

FAILS = []


def check(name, cond):
    if not cond:
        FAILS.append(name)
        print(f"XX  {name}")


_REAL = ssh_transport.run_ssh
ssh_transport.run_ssh = lambda conn, cmd, secrets=None: {
    "exit_code": 0, "stdout": "fake", "stderr": "", "truncated": False}


def dispatch(command, allow_writes=True, **kw):
    harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw",
                      "allow_writes": allow_writes})
    del harness.AUDIT[:]
    with confirm_stub.approving(**kw) as io_obj:
        res = harness.run_command(command)
    return res, harness.AUDIT[-1], io_obj


WRITE = "systemctl restart ollama"
READ = "nvidia-smi"
DESTRUCTIVE = "rm -rf /workspace"

# --- approved -> runs, and the request carried the LITERAL command ------------------------------
# If the string on the card could differ from the string that executes, the approval would describe
# a command that never ran while the one that ran was never approved.
res, entry, io_obj = dispatch(WRITE)
check("approved-runs", res["executed"] is True and entry["disposition"] == "ran_mutating")
check("request-carries-literal-command",
      len(io_obj.requests) == 1 and io_obj.requests[0]["command"] == WRITE)

# --- declined -> does NOT run, and settles as refused on the wire --------------------------------
res, entry, _ = dispatch(WRITE, decide=lambda c: False)
check("declined-does-not-run", res["executed"] is False)
check("declined-disposition", entry["disposition"] == "refused_not_approved")
check("declined-wire", harness._wire_disposition(entry["disposition"]) == "refused")
# The agent must not go hunting for an equivalent command; a decline is an answer, not an obstacle.
check("declined-tells-agent-not-to-retry", "Do not" in res["text"] and "retry" in res["text"])

# --- every ambiguity is a denial ----------------------------------------------------------------
# These are the cases where "assume yes" would be catastrophic and silent: the parent died, the
# reply was garbage, or a stale reply arrived for a different request.
for name, corrupt in [
    ("eof", lambda r: ""),
    ("malformed-json", lambda r: "not json\n"),
    ("missing-approved", lambda r: '{"id": "%s"}\n' % r["id"]),
    ("approved-not-true", lambda r: '{"id": "%s", "approved": "yes"}\n' % r["id"]),
    ("wrong-id", lambda r: '{"id": "some-other-request", "approved": true}\n'),
]:
    res, entry, _ = dispatch(WRITE, corrupt=corrupt)
    check(f"ambiguity-denies::{name}",
          res["executed"] is False and entry["disposition"] == "refused_not_approved")

# --- reads are never gated ----------------------------------------------------------------------
# 20-45 of the commands in a real run are reads. Asking about those would make the feature unusable
# and would train the operator to click through without reading.
res, entry, io_obj = dispatch(READ)
check("reads-not-gated", res["executed"] is True and len(io_obj.requests) == 0)

# --- destructive is refused WITHOUT asking ------------------------------------------------------
# Offering a card for `rm -rf /workspace` would imply approving it is possible. It is not, and a
# card that cannot be honoured is worse than no card.
res, entry, io_obj = dispatch(DESTRUCTIVE)
check("destructive-refused", entry["disposition"] == "refused_destructive")
check("destructive-never-asks", len(io_obj.requests) == 0)

# --- read-only mode does not ask either ---------------------------------------------------------
# With writes off the answer is already no; asking would be theatre.
res, entry, io_obj = dispatch(WRITE, allow_writes=False)
check("readonly-refuses-without-asking",
      entry["disposition"] == "refused_mutating_phase1" and len(io_obj.requests) == 0)

# --- ids are per-request, so a reply cannot be replayed onto the next command --------------------
harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw", "allow_writes": True})
with confirm_stub.approving() as io_obj:
    harness.run_command(WRITE)
    harness.run_command("chmod 644 /tmp/f")
check("ids-are-unique-per-request",
      len(io_obj.requests) == 2 and io_obj.requests[0]["id"] != io_obj.requests[1]["id"])

ssh_transport.run_ssh = _REAL
print(f"\n{'FAILED: ' + ', '.join(FAILS) if FAILS else 'all confirm-loop checks passed'}")
sys.exit(1 if FAILS else 0)
