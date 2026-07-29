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
import os
import shutil
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

# --- the description must not forbid what the executor allows ------------------------------------
# The clause "no chaining/pipes/redirection" predates the 2026-07-23 deny-by-EFFECT gate and is now
# false: measured, `nohup ... > /root/x.log 2>&1 &` classifies `mutating` (approve -> run) and
# `ps aux | grep x` classifies `read_only`. It is not a cosmetic staleness — redirection is exactly
# what starting a service requires, so the sentence forbade the repair itself. These two checks are
# the gate and the text asserted TOGETHER, so the text cannot drift from the gate again silently.
check("gate-actually-allows-redirect",
      guardrails.classify("nohup python3 /root/app/start.py > /root/app.log 2>&1 &") == "mutating"
      and guardrails.is_form_violation("nohup python3 /root/app/start.py > /root/app.log 2>&1 &")
      is False)
check("gate-actually-allows-pipe", guardrails.classify("ps aux | grep -i comfy") == "read_only")
check("write-tool-desc-drops-false-shape-clause",
      "no chaining/pipes/redirection" not in harness.tool_description(True))
# The BrokenPipe death: a live repair started the service, the exec returned, the pipe closed and the
# process died seconds later. `nohup` alone does not fix it (nohup only diverts to nohup.out when
# stdout is a TTY; over an ssh exec it is a pipe — verified on a real box: no nohup.out is created).
# So the description must name the REDIRECT, not just backgrounding.
check("write-tool-desc-teaches-detach", "> /path/to/log 2>&1 &" in harness.tool_description(True))
check("write-tool-desc-says-nohup-insufficient", "`nohup` alone does not" in harness.tool_description(True))

# --- the second skill: write mode gets a repair playbook, read-only mode must not -----------------
# instance-triage is read-only THROUGHOUT (H1, opening job definition, and its section-4 verdict
# template), so one OVERRIDE clause in the system prompt does not settle it — the observed failure
# was 「请授权我执行安全修复」, i.e. that template executed faithfully AFTER repair was authorized.
check("readonly-skills-unchanged", harness.skills_for(False) == ["instance-triage"])
check("write-skills-add-repair", harness.skills_for(True) == ["instance-triage", "instance-repair"])
check("write-prompt-names-repair-skill", "`instance-repair`" in harness.system_prompt(True))
check("write-prompt-says-which-skill-wins", "instance-repair` wins" in harness.system_prompt(True))
check("readonly-prompt-has-no-repair-skill", "instance-repair" not in harness.system_prompt(False))

# --- the three load-bearing rules live in the SYSTEM PROMPT, not in a skill ----------------------
# Measured 2026-07-29: an instrumented live run logged 21 tool_use blocks — 21 ssh_exec, 0 Skill.
# The model never loads a playbook, so a rule that only exists in SKILL.md does not exist. These are
# the three behaviours it provably gets wrong unaided, each with a real failure behind it.
_wp = harness.system_prompt(True)
check("prompt-rule-prefer-image-launcher", "prefer the image's OWN launcher" in _wp)
check("prompt-rule-names-the-bypass-it-makes", "main.py" in _wp)   # the exact entrypoint it reaches for
check("prompt-rule-port-inventory-diff",
      "list the listening ports" in _wp and "is not up now" in _wp)
check("prompt-rule-verdict-sections-inline", "已执行的修复 / 验证 / 未处理" in _wp)
check("prompt-rule-own-failed-commands",
      "INCLUDING any that failed" in _wp and "never fold a command of yours" in _wp)
# The verdict shape must NOT be delegated to a skill the model never opens.
check("prompt-does-not-delegate-verdict-to-skill",
      "in the form `instance-repair` specifies" not in _wp)
# argv ceiling: a prior length probe (N=3/size) initialized cleanly through 6000 chars and died with
# an instant exit-1 near 12000. Stay inside the VERIFIED band, not merely under the cliff.
check("prompt-within-verified-length-band", len(_wp) < 6000)
check("readonly-prompt-untouched-by-repair-rules", "prefer the image's OWN launcher" not in harness.system_prompt(False))

_repair = os.path.join(os.path.dirname(os.path.abspath(harness.__file__)),
                       "skills", "instance-repair", "SKILL.md")
_repair_text = open(_repair, encoding="utf-8").read()
# The three things this skill exists to fix. Asserted on content, not on the file existing: an empty
# skill would load fine and change nothing.
check("repair-skill-replaces-section-4", "replaces `instance-triage` section 4" in _repair_text)
check("repair-skill-has-executed-section", "已执行的修复" in _repair_text)
check("repair-skill-teaches-redirect", "> /path/to/some.log 2>&1 &" in _repair_text)
check("repair-skill-says-nohup-insufficient", "`nohup` on its own is **not enough**" in _repair_text)
check("repair-skill-owns-failed-attempts",
      "report it as your own failed attempt" in _repair_text)

# Staging is the real boundary, not the `skills=` list: the CLI discovers skills by walking up from
# cwd, so a repair playbook left on disk is reachable by a read-only run — a card that says 只读排查
# over an agent that has been handed a repair procedure.
_cwd = os.getcwd()
try:
    for allow, want in ((False, ["instance-triage"]),
                        (True, ["instance-repair", "instance-triage"])):
        root = harness.stage_skills(allow)
        staged = sorted(os.listdir(os.path.join(root, ".claude", "skills")))
        check(f"staged-skills-match-mode::allow={allow}", staged == want)
        shutil.rmtree(root, ignore_errors=True)
finally:
    os.chdir(_cwd)

ssh_transport.run_ssh = _REAL_RUN_SSH

print(f"\n{'FAILED: ' + ', '.join(FAILS) if FAILS else 'all write-mode checks passed'}")
sys.exit(1 if FAILS else 0)
