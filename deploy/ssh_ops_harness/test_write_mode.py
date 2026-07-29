"""Offline gate for the write-enabled mode (no network / no SSH / no SDK).

Run:  python test_write_mode.py   ->  exits non-zero on ANY failure.

The point of this file is the boundary BETWEEN the two modes. Three properties have to hold, and
the third is the one that is easy to lose:

  1. allow_writes off  -> byte-for-byte the lane that was measured read-only. Nothing executes.
  2. allow_writes on   -> the mutating tier executes, so the agent can actually repair.
  3. allow_writes on   -> the DESTRUCTIVE tier and the SHAPE gate are unchanged.

(3) matters because it is tempting to read the gate as "writes are allowed now, so stop refusing".
The shape gate is not part of the read-only policy: classify() only ever sees the literal command
string, so `$(printf '\\x72\\x6d') -rf /` is destructive in effect and harmless in text. Refusing
substitution outright is what keeps the destructive tier meaningful at all.
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


_REAL_RUN_SSH = ssh_transport.run_ssh
ssh_transport.run_ssh = lambda conn, cmd, secrets=None: {
    "exit_code": 0, "stdout": "fake", "stderr": "", "truncated": False}


def dispatch(command, allow_writes):
    """Run one command through the real classify-dispatch with the gate set as given.

    Writes now also need a per-command approval, so this stands in for the operator saying yes to
    everything. That is the RIGHT default here: this file tests the config gate (may the mutating
    tier execute at all), and test_confirm_loop.py tests the human gate. Keeping them separate is
    what makes a failure point at one mechanism instead of two.
    """
    harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw",
                      "allow_writes": allow_writes})
    del harness.AUDIT[:]
    with confirm_stub.approving():
        res = harness.run_command(command)
    return res, harness.AUDIT[-1]


# --- the gate arrives on the handshake and nowhere else -----------------------------------------
# If the Go side ever stops sending the field, .get() yields None and this must read as OFF, not as
# "unset means allow". A missing gate defaulting open is the failure mode that has no symptom.
harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw"})
check("absent-gate-means-read-only", harness._ALLOW_WRITES is False)
harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw", "allow_writes": True})
check("gate-latches-from-handshake", harness._ALLOW_WRITES is True)

# --- (1) read-only mode is unchanged -------------------------------------------------------------
for cmd in ["systemctl restart ollama", "pip install torch", "kill -9 4321",
            "echo x > /tmp/f", "mkdir /tmp/d", "chmod 644 /tmp/f"]:
    res, entry = dispatch(cmd, allow_writes=False)
    check(f"readonly-refuses::{cmd}", res["executed"] is False and entry["disposition"] ==
          "refused_mutating_phase1")

# --- (2) write mode executes the mutating tier ---------------------------------------------------
for cmd in ["systemctl restart ollama", "pip install torch", "kill -9 4321",
            "echo x > /tmp/f", "mkdir /tmp/d", "chmod 644 /tmp/f"]:
    res, entry = dispatch(cmd, allow_writes=True)
    check(f"write-executes::{cmd}", res["executed"] is True and entry["disposition"] == "ran_mutating")
    # The wire disposition is what the user's activity stream and the audit row show. A write that
    # settles as anything other than "ran" is a change to someone's machine with no visible record.
    check(f"write-wire-disposition::{cmd}", harness._wire_disposition(entry["disposition"]) == "ran")
    check(f"write-keeps-tier::{cmd}", entry["tier"] == "mutating")

# --- (3a) destructive stays refused WITH writes on -----------------------------------------------
# These are exactly the "除了 rm 等部分高危操作" carve-out. They must not become reachable by
# turning writes on, because no consent card in this design distinguishes them from a restart.
for cmd in ["rm -rf /workspace", "mkfs.ext4 /dev/vdb", "dd if=/dev/zero of=/dev/vda bs=1M",
            "reboot", "shutdown -h now", "passwd root", "userdel ubuntu",
            "systemctl disable sshd", "iptables -F", "chmod -R 777 /",
            "docker system prune", "crontab -r", "swapoff -a"]:
    res, entry = dispatch(cmd, allow_writes=True)
    check(f"write-still-refuses-destructive::{cmd}",
          res["executed"] is False and entry["disposition"] == "refused_destructive")
    check(f"destructive-tier-intact::{cmd}", guardrails.classify(cmd) == "destructive")

# --- (3b) the shape gate stays refused WITH writes on --------------------------------------------
# Command substitution hides the command text from classify(), so allowing it in write mode would
# turn the destructive list into decoration. Multi-line input is refused for the same reason:
# classify() only ever reasons about a single line.
for cmd in ["cat $(which python3)", "echo `whoami`", "systemctl restart $(cat /tmp/svc)",
            "echo one\nrm -rf /"]:
    res, entry = dispatch(cmd, allow_writes=True)
    check(f"write-still-refuses-shape::{cmd[:28]!r}",
          res["executed"] is False and entry["disposition"] in ("refused_form", "refused_destructive"))

# A refusal the model cannot act on wastes the turn. In write mode the read-only wording ("this
# changes the box") is actively wrong: it sends the agent looking for a permission it already has.
res, _ = dispatch("cat $(which python3)", allow_writes=True)
check("write-shape-refusal-explains-form", "FORM rejected" in res["text"])
check("write-shape-refusal-not-readonly-wording", "read-only" not in res["text"])

# --- reads are untouched in both modes -----------------------------------------------------------
for allow in (False, True):
    res, entry = dispatch("nvidia-smi", allow_writes=allow)
    check(f"read-runs::allow={allow}", res["executed"] is True and entry["disposition"] == "ran_read_only")

# --- the two system prompts are distinct and each states its own contract ------------------------
check("readonly-prompt-says-readonly", "Stay read-only" in harness.system_prompt(False))
check("write-prompt-authorizes-repair", "authorized repair" in harness.system_prompt(True))
# The skill is shared byte-identically between modes and tells the agent it is read-only. If the
# write prompt does not name that contradiction, the agent has to guess which instruction wins —
# and a correct diagnosis followed by a refusal to act looks like a model limitation, not a bug.
check("write-prompt-overrides-the-skill", "OVERRIDE" in harness.system_prompt(True))
check("write-prompt-names-hard-limits", "hard-refused" in harness.system_prompt(True))

# --- and so does the TOOL DESCRIPTION, which is the one the model actually believes --------------
# A live write-enabled run diagnosed the box correctly, produced the right fix command, and then
# reported 「当前 SSH 诊断接口仅允许只读命令，无法直接执行启动/修改操作」 — a direct restatement of
# the tool description, which still said "Read-only commands only" while the system prompt said
# repair was authorized. The model was not being cautious; it was believing its tool. Whatever the
# system prompt says, a tool whose own description forbids writing will not be used to write.
check("write-tool-desc-does-not-forbid-writes",
      "Read-only commands only" not in harness.tool_description(True))
check("write-tool-desc-says-changes-run",
      "CHANGES the box" in harness.tool_description(True))
# The specific failure was describe-instead-of-do, so the description says so in those words.
check("write-tool-desc-says-send-not-describe",
      "do not describe it and stop" in harness.tool_description(True))
# Hard limits stay stated so the agent does not plan around commands the executor will reject.
check("write-tool-desc-keeps-hard-limits",
      "refused outright" in harness.tool_description(True))
# Read-only mode must be BYTE-IDENTICAL to what was measured — this is the description that was in
# the decorator literal before it became mode-dependent.
check("readonly-tool-desc-byte-identical",
      harness.tool_description(False) == (
          "Run ONE read-only diagnostic shell command on the remote GPU instance over SSH and return "
          "its output. Read-only commands only; one command per call; no chaining/pipes/redirection."))
check("tool-desc-differs-by-mode",
      harness.tool_description(True) != harness.tool_description(False))

ssh_transport.run_ssh = _REAL_RUN_SSH

print(f"\n{'FAILED: ' + ', '.join(FAILS) if FAILS else 'all write-mode checks passed'}")
sys.exit(1 if FAILS else 0)
