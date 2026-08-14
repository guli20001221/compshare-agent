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

# The check above is 24 characters long, so it held while the payload was built from
# `command[:400]` — the assertion was correct and simply could not fail. Until 2026-07-30 a longer
# command was silently trimmed to fit the card: the operator approved a PREFIX while the suffix (the
# end of a sed expression, the target of a redirect) executed unread. Consent to a prefix is not
# consent, so the case that exercises it has to be longer than the old cap.
LONG_WRITE = "sed -i 's/" + "a" * 450 + "/b/' /workspace/app.conf"
check("long-command-is-classified-approvable", guardrails.classify(LONG_WRITE) == "mutating")
res, entry, io_obj = dispatch(LONG_WRITE)
check("request-is-not-truncated",
      len(io_obj.requests) == 1 and io_obj.requests[0]["command"] == LONG_WRITE)
check("approved-long-command-runs", res["executed"] is True)

# Past the bound the command is REFUSED rather than trimmed to fit, and no card is offered — a card
# the operator cannot fully read is not consent, and shortening it to look presentable is the bug.
TOO_LONG = "sed -i 's/" + "a" * (harness._MAX_CONFIRMABLE_COMMAND + 100) + "/b/' /workspace/app.conf"
res, entry, io_obj = dispatch(TOO_LONG)
check("unconfirmable-does-not-run", res["executed"] is False)
check("unconfirmable-never-asks", len(io_obj.requests) == 0)
check("unconfirmable-disposition", entry["disposition"] == "refused_unconfirmable")
check("unconfirmable-settles-as-refused-on-the-wire",
      harness._wire_disposition(entry["disposition"]) == "refused")
check("unconfirmable-says-it-is-not-a-permissions-problem", "not\na permissions problem"
      in res["text"] or "not a permissions problem" in res["text"])

# --- declined -> does NOT run, and settles as refused on the wire --------------------------------
res, entry, _ = dispatch(WRITE, decide=lambda c: False)
check("declined-does-not-run", res["executed"] is False)
check("declined-disposition", entry["disposition"] == "refused_user_declined")
check("declined-wire", harness._wire_disposition(entry["disposition"]) == "refused")
# The agent must not go hunting for an equivalent command; a decline is an answer, not an obstacle.
check("declined-tells-agent-not-to-retry", "Do not" in res["text"] and "retry" in res["text"])

# A timeout is also a denial, but it is not a decline. Preserve that fact all
# the way to the step disposition so the activity stream can say what happened.
res, entry, _ = dispatch(WRITE, decide=lambda c: (False, "timeout"))
check("timeout-does-not-run", res["executed"] is False)
check("timeout-disposition", entry["disposition"] == "refused_confirmation_timeout")
check("timeout-wire", harness._wire_disposition(entry["disposition"]) == "refused")
check("timeout-is-not-worded-as-decline",
      "timed out" in res["text"] and "declined" not in res["text"])

# A rolling upgrade can pair this harness with an older Go supervisor that only
# sends {id, approved}. It must stay fail-closed and honestly generic rather
# than inventing a user decision.
res, entry, _ = dispatch(WRITE, decide=lambda c: (False, ""))
check("legacy-no-reason-does-not-run", res["executed"] is False)
check("legacy-no-reason-stays-generic", entry["disposition"] == "refused_not_approved")
check("legacy-no-reason-does-not-claim-decline", "no explicit approval" in res["text"])

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
