"""Offline gate for the per-write confirmation loop (no network / no SSH / no SDK).

Run:  python test_confirm_loop.py   ->  exits non-zero on ANY failure.

The lane-level card authorizes entering the box, not a later state change. Each write therefore
requires approval of its exact command.

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


def dispatch(command, **kw):
    harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw"})
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

# Exercise a command beyond the historical display cap: approval text must never be truncated.
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

# --- every transport reason survives the crossing, and none of them lands as "failed" ------------
# The Go side computes this closed set once per card (observability.ConfirmationReason*) and it has
# to arrive intact: the two tests above cover the two ends of the range, and these cover the middle,
# which is where a table entry gets forgotten. `_DISPOSITION_MAP` is checked per reason because a
# disposition missing from it does not error — `_wire_disposition` defaults to "failed", which would
# show the user a command that broke rather than one that was never approved.
for _reason, _disposition, _needle in [
    ("user_declined", "refused_user_declined", "the user declined"),
    ("timeout", "refused_confirmation_timeout", "confirmation timed out"),
    ("client_disconnect", "refused_client_disconnect", "client connection ended"),
    ("delivery_failed", "refused_confirmation_delivery_failed", "could not be delivered"),
    ("broker_cancelled", "refused_confirmation_broker_cancelled", "was cancelled"),
]:
    res, entry, _ = dispatch(WRITE, decide=lambda c, _r=_reason: (False, _r))
    check(f"reason-crosses-intact::{_reason}",
          res["executed"] is False and entry["disposition"] == _disposition)
    check(f"reason-is-refused-not-failed::{_reason}",
          harness._wire_disposition(entry["disposition"]) == "refused")
    check(f"reason-names-itself-to-the-model::{_reason}", _needle in res["text"])
    check(f"reason-says-nothing-ran::{_reason}", "NOT EXECUTED" in res["text"])
# An unknown reason is a newer server talking to this harness. It must degrade to the generic
# sentence, never guess the nearest known cause.
res, entry, _ = dispatch(WRITE, decide=lambda c: (False, "some_future_reason"))
check("unknown-reason-degrades-generic", entry["disposition"] == "refused_not_approved")
check("unknown-reason-does-not-guess", "declined" not in res["text"] and "timed out" not in res["text"])

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

# --- ids are per-request, so a reply cannot be replayed onto the next command --------------------
harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw"})
with confirm_stub.approving() as io_obj:
    harness.run_command(WRITE)
    harness.run_command("chmod 644 /tmp/f")
check("ids-are-unique-per-request",
      len(io_obj.requests) == 2 and io_obj.requests[0]["id"] != io_obj.requests[1]["id"])

ssh_transport.run_ssh = _REAL
print(f"\n{'FAILED: ' + ', '.join(FAILS) if FAILS else 'all confirm-loop checks passed'}")
sys.exit(1 if FAILS else 0)
