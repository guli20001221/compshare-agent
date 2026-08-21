"""Offline gate for the write-enabled mode (no network / no SSH / no SDK).

Run:  python test_write_mode.py   ->  exits non-zero on ANY failure.

The point of this file is the boundary BETWEEN the two modes. Three properties have to hold, and
the third is the one that is easy to lose:

  1. allow_writes off  -> only commands proven read-only execute; the instance is not changed.
  2. allow_writes on   -> the mutating tier executes, so the agent can actually repair.
  3. allow_writes on   -> the DESTRUCTIVE tier and the SHAPE gate are unchanged.

(3) matters because it is tempting to read the gate as "writes are allowed now, so stop refusing".
The shape gate is not part of the read-only policy: classify() only ever sees the literal command
string, so `$(printf '\\x72\\x6d') -rf /` is destructive in effect and harmless in text. Refusing
substitution outright is what keeps the destructive tier meaningful at all.
"""
import os
import shlex
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


def flat(text):
    """Compare prose semantics without coupling tests to source line wrapping."""
    return " ".join(text.split())


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
            "echo x > /tmp/f", "mkdir /tmp/d", "chmod 644 /tmp/f",
            "chmod 777 /workspace/app", "chattr +i /workspace/model.bin",
            "systemctl disable vllm", "swapoff -a",
            "install -m 440 /tmp/x /etc/sudoers.d/90-x"]:
    res, entry = dispatch(cmd, allow_writes=False)
    check(f"readonly-refuses::{cmd}", res["executed"] is False and entry["disposition"] ==
          "refused_mutating_phase1")

# --- (2) write mode executes the mutating tier ---------------------------------------------------
for cmd in ["systemctl restart ollama", "pip install torch", "kill -9 4321",
            "echo x > /tmp/f", "mkdir /tmp/d", "chmod 644 /tmp/f",
            "chmod 777 /workspace/app", "chattr +i /workspace/model.bin",
            "systemctl disable vllm", "swapoff -a",
            "install -m 440 /tmp/x /etc/sudoers.d/90-x"]:
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
            "systemctl disable sshd", "systemctl --now mask NetworkManager",
            "iptables -F", "chmod -R 777 /",
            "docker system prune", "crontab -r"]:
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

# --- platform-facing FileBrowser is not a guest-binary repair -----------------------------------
# The production failure found a standalone binary, guessed 8080 + /workspace + --noauth, and used
# loopback HTTP 200 as proof that the CONSOLE File Browser was fixed. It was not: the platform route,
# authenticated entrypoint and real root had never been established. Direct launch must be refused
# even after the user approves it, while an image-owned supervisor remains repairable.
unsafe_filebrowser = (
    "nohup /model/other/filebrowser/filebrowser --address 0.0.0.0 --port 8080 "
    "--root /workspace --noauth --database /tmp/filebrowser.db > /tmp/filebrowser.log 2>&1 &"
)
res, entry = dispatch(unsafe_filebrowser, allow_writes=True)
check("filebrowser-direct-launch-is-refused",
      res["executed"] is False and entry["disposition"] == "refused_unmanaged_platform_service")
check("filebrowser-refusal-explains-platform-contract",
      "platform-managed entry" in res["text"] and "external route" in res["text"])
check("filebrowser-refusal-keeps-wire-shape",
      harness._wire_disposition(entry["disposition"]) == "refused")
check("filebrowser-guard-recognizes-nohup-wrapper",
      guardrails.is_unmanaged_platform_service_launch(unsafe_filebrowser) is True)
check("filebrowser-guard-recognizes-shell-c-wrapper",
      guardrails.is_unmanaged_platform_service_launch(
          "bash -c 'nohup /model/other/filebrowser/filebrowser --port 8080 --noauth &'") is True)
check("filebrowser-guard-recognizes-quoted-executable",
      guardrails.is_unmanaged_platform_service_launch(
          "exec '/model/other/filebrowser/filebrowser' --port 8080") is True)
check("filebrowser-help-remains-diagnostic",
      guardrails.is_unmanaged_platform_service_launch("/model/other/filebrowser/filebrowser --help") is False)
check("filebrowser-command-v-remains-diagnostic",
      guardrails.is_unmanaged_platform_service_launch("command -v filebrowser") is False)
check("filebrowser-image-supervisor-through-shell-remains-repairable",
      guardrails.is_unmanaged_platform_service_launch("bash -c 'supervisorctl start filebrowser'") is False)
res, entry = dispatch("supervisorctl start filebrowser", allow_writes=True)
check("filebrowser-image-supervisor-remains-repairable",
      res["executed"] is True and entry["disposition"] == "ran_mutating")

# --- reads are untouched in both modes -----------------------------------------------------------
for allow in (False, True):
    res, entry = dispatch("nvidia-smi", allow_writes=allow)
    check(f"read-runs::allow={allow}", res["executed"] is True and entry["disposition"] == "ran_read_only")

# --- the two system prompts share one diagnostic protocol and state distinct authorization -------
check("readonly-prompt-says-readonly",
      "Authorization: diagnosis only" in harness.system_prompt(False) and
      "Do not change the instance" in harness.system_prompt(False))
check("write-prompt-authorizes-repair",
      "authorized the repair workflow" in harness.system_prompt(True) and
      "requires approval of that exact effect" in harness.system_prompt(True))
check("write-prompt-names-hard-limits",
      "Hard refusal is only" in harness.system_prompt(True) and
      "tenant/control-plane boundary" in harness.system_prompt(True))

# The prompt is one stable diagnostic protocol plus a mode policy, not two independently patched
# playbooks. Both modes must preserve the same evidence model and diagnostic loop, then select one
# and only one authorization contract.
_READ_PROMPT = harness.system_prompt(False)
_WRITE_PROMPT = harness.system_prompt(True)
for _mode, _prompt in (("read", _READ_PROMPT), ("write", _WRITE_PROMPT)):
    check(f"prompt-contract::{_mode}::shared-core", _prompt.startswith(harness._SYSTEM_PROMPT_CORE))
    check(f"prompt-contract::{_mode}::structured",
          all(section in _prompt for section in ("## Evidence model", "## Diagnostic loop",
                                                  "## Authorization:", "## Final response")))
    check(f"prompt-contract::{_mode}::layered-evidence",
          all(layer in _prompt for layer in ("Control-plane metadata", "guest state",
                                              "application state", "external reachability")))
    check(f"prompt-contract::{_mode}::outcome-driven",
          "observable success criterion" in _prompt and "original success criterion" in _prompt)
    check(f"prompt-contract::{_mode}::actual-runtime",
          "application's real" in _prompt and "virtualenv or Conda" in _prompt)
    check(f"prompt-contract::{_mode}::managed-ownership-invariant",
          "controller's ownership state" in _prompt and
          "drift rather than proof" in _prompt)
check("prompt-contract::write-reconciles-owner-before-start-and-polls-terminal-state",
      "Reconcile an existing ownership conflict" in _WRITE_PROMPT and
      "bounded-poll transitional manager states" in _WRITE_PROMPT and
      "terminal result" in _WRITE_PROMPT)
check("prompt-contract::mode-policy-is-exclusive",
      "Authorization: diagnosis only" in _READ_PROMPT and
      "Authorization: diagnose, repair, verify" not in _READ_PROMPT and
      "Authorization: diagnose, repair, verify" in _WRITE_PROMPT and
      "Authorization: diagnosis only" not in _WRITE_PROMPT)
check("prompt-contract::no-incident-specific-patches",
      all(token not in (_READ_PROMPT + _WRITE_PROMPT).lower()
          for token in ("filebrowser", "main.py", "/start.d/", "8188", "comfyui")))
check("prompt-contract::within-argv-safe-band",
      len(_READ_PROMPT) < 5000 and len(_WRITE_PROMPT) < 5000)
# The prompt states authorization directly and does not arbitrate deleted playbooks.
check("write-prompt-does-not-arbitrate-skills", "OVERRIDE" not in harness.system_prompt(True))
check("write-prompt-names-no-skill", "skill" not in harness.system_prompt(True).lower())

# --- The tool description and system prompt must agree that confirmed repairs are available. ------
check("write-tool-desc-does-not-forbid-writes",
      "Read-only commands only" not in harness.tool_description(True))
check("write-tool-desc-says-changes-run",
      "state-changing repair" in harness.tool_description(True) and
      "approves that exact command" in harness.tool_description(True))
# The contract tells the model to submit an evidence-backed action rather than stopping at prose.
check("write-tool-desc-says-send-not-describe",
      "send the smallest concrete command" in harness.tool_description(True))
# Hard limits stay stated so the agent does not plan around commands the executor will reject.
check("write-tool-desc-keeps-hard-limits",
      "irreversible" in harness.tool_description(True) and
      "control-plane crossings" in harness.tool_description(True) and
      "are refused" in harness.tool_description(True))
check("write-tool-desc-forbids-invented-platform-entrypoints",
      "platform-facing port" in harness.tool_description(True) and
      "substitute service" in harness.tool_description(True))
# Tool descriptions are behavioral contracts, not a growing list of executable-path exceptions.
check("readonly-tool-desc-states-execution-semantics",
      all(needle in harness.tool_description(False) for needle in
          ("fresh, non-interactive SSH session", "25 seconds", "prove read-only", "exit status")))
check("both-tool-descriptions-prefer-the-actual-runtime",
      all("application's actual interpreter" in harness.tool_description(mode)
          for mode in (False, True)))
check("both-tool-descriptions-require-managed-ownership",
      all("surviving" in harness.tool_description(mode) and
          ("controller ownership" in harness.tool_description(mode) or
           "outside that ownership" in harness.tool_description(mode))
          for mode in (False, True)))
check("write-tool-description-reconciles-owner-and-polls-transition",
      "reconcile any surviving child outside that ownership" in harness.tool_description(True) and
      "transitional manager states" in harness.tool_description(True) and
      "terminal result" in harness.tool_description(True))
check("tool-desc-differs-by-mode",
      harness.tool_description(True) != harness.tool_description(False))
check("tool-desc-contracts-stay-general",
      all(token not in (harness.TOOL_DESC + harness.TOOL_DESC_WRITE).lower()
          for token in ("filebrowser", "main.py", "/start.d/", "8188", "python -c")))
check("tool-desc-contracts-stay-focused",
      len(harness.TOOL_DESC) < 1000 and len(harness.TOOL_DESC_WRITE) < 2000)

# --- the description must not forbid what the executor allows ------------------------------------
# The description must match executable policy: supported chaining, pipes and redirection cannot be
# described as forbidden.
check("gate-actually-allows-redirect",
      guardrails.classify("nohup python3 /root/app/start.py > /root/app.log 2>&1 &") == "mutating"
      and guardrails.is_form_violation("nohup python3 /root/app/start.py > /root/app.log 2>&1 &")
      is False)
check("gate-actually-allows-pipe", guardrails.classify("ps aux | grep -i comfy") == "read_only")
check("write-tool-desc-drops-false-shape-clause",
      "no chaining/pipes/redirection" not in harness.tool_description(True))
# The BrokenPipe/exit-124 lesson is now executable structure rather than a shell recipe the model has
# to reproduce. The SSH description must route long work to the two reviewed tools and must not keep
# teaching a parallel hand-rolled protocol that can drift from them.
check("write-tool-desc-routes-long-work-to-structured-job",
      "structured background-job tools" in harness.tool_description(True) and
      "do not hand-roll detachment" in harness.tool_description(True))
check("write-surface-has-start-and-poll-job-tools",
      all(name in harness.allowed_tools(True) for name in
          ("mcp__ssh_ops__start_background_job", "mcp__ssh_ops__poll_background_job")))

# The lane has no skills or built-in Skill tool; policy lives in the prompt and executable tools.
check("skills-are-gone-from-the-module", not hasattr(harness, "skills_for") and not hasattr(harness, "SKILLS"))
check("skills-directory-is-gone",
      not os.path.exists(os.path.join(os.path.dirname(os.path.abspath(harness.__file__)), "skills")))
check("readonly-prompt-has-no-repair-skill", "instance-repair" not in harness.system_prompt(False))
# Neither prompt may tell the model to load a playbook that no longer exists — an instruction to open
# a missing document is worse than the one it replaced, since it invites answering as though one had.
check("readonly-prompt-does-not-claim-skill-load",
      "skill" not in harness.system_prompt(False).lower())
# The Skill TOOL goes with them. It was the single permitted built-in, justified only by "skills are
# how the playbook reaches the model"; with no playbook that justification is empty, and INV-9 is
# back to its original posture — no built-in exists on the control-plane host.
check("skill-tool-no-longer-exists", harness.TOOLS_BASE == [])
check("atomic-file-tool-exists-only-in-write-mode",
      "mcp__ssh_ops__atomic_text_replace" not in harness.allowed_tools(False) and
      "mcp__ssh_ops__atomic_text_replace" in harness.allowed_tools(True))
check("endpoint-probe-exists-in-both-modes",
      "mcp__ssh_ops__endpoint_probe" in harness.allowed_tools(False) and
      "mcp__ssh_ops__endpoint_probe" in harness.allowed_tools(True))
check("atomic-file-tool-is-hash-bound-and-backed-up",
      all(term in harness.atomic_file.TOOL_DESCRIPTION
          for term in ("SHA-256", "same-directory backup", "atomically renames")))

# The lane repairs only the assigned fault; broader application replacement or shutdown requires a
# separate user decision.
for _name, _needle in [
    ("states the scope", "within the diagnosed fault"),
    ("names redeploying an app", "re-downloading an application"),
    ("names taking a service down", "disabling an unrelated service"),
    ("routes it to the user rather than forbidding it", "unless the task requests it"),
]:
    check(f"write-tool-desc-bounds-scope::{_name}", _needle in harness.tool_description(True))
# Read-only mode cannot overreach — it executes nothing — so its description stays unchanged.
check("readonly-tool-desc-unchanged-by-scope-rule",
      "Repair the fault" not in harness.tool_description(False))

# A form refusal is recoverable by changing syntax, unlike a denied/expired
# confirmation. The model must receive that distinction directly from its two
# authoritative prompt surfaces, and neither should call the user an "operator".
_WRITE_DESC = harness.tool_description(True)
_WRITE_PROMPT = harness.system_prompt(True)
check("write-tool-desc::reformats-form-refusals",
      "Rewrite only command-form rejections" in _WRITE_DESC)
check("write-system-prompt::reformats-form-refusals",
      "rejects only the command form" in _WRITE_PROMPT and "rewrite it into a supported plain command" in _WRITE_PROMPT)
check("write-system-prompt::does-not-manualize-unapproved-writes",
      "do not turn the command into a manual instruction" in _WRITE_PROMPT)
check("write-system-prompt::does-not-bypass-denied-effect",
      "Do not seek an equivalent fallback for a denied effect" in _WRITE_PROMPT)
check("write-system-prompt::labels-waiting-for-confirmation",
      "等待你确认" in _WRITE_PROMPT and "approval is pending or denied" in _WRITE_PROMPT)
check("write-prompts::name-the-user-not-an-operator",
      "operator" not in _WRITE_DESC and "operator" not in _WRITE_PROMPT)

# The FileBrowser lesson is "do not invent an app's platform entry", NOT "never mention a
# platform-level operation". Those were briefly the same rule, and the wider one had no way to be
# satisfied: nothing in the reference context describes a console control (the fact keys are
# instance/gpu/image/disks/port_hints/tcp_forwards/declared_software/listeners/catalog), so a
# repair that genuinely needs a reboot could only ever be reported as 未核实 — turning the one
# correct handoff into a dead end.
#
# Naming the boundary is necessary but not sufficient: this verdict ENDS the turn (the Go side
# returns it behind finalReplyPrefix), so a sentence that only states "this needs a platform-level
# restart" leaves the user holding a diagnosis with no next move. The rule therefore has to make the
# model ASK, which is a question the user can answer on the following turn and the outer agent can
# act on with RebootInstanceWorkflow. Both surfaces carry all four clauses; they are asserted
# together because a rule present in only one of them is a rule the model may not see.
check("write-tool-desc-platform-boundary::forbids-invention",
      "platform-facing port" in _WRITE_DESC and "substitute service" in _WRITE_DESC)
check("write-system-prompt-platform-boundary::forbids-invention",
      "invent a platform-facing" in _WRITE_PROMPT and "substitute service" in _WRITE_PROMPT)
check("write-tool-desc-platform-boundary::names-and-hands-off-restart",
      "Restarting the instance is unavailable" in _WRITE_DESC and
      "ask whether the user wants the instance restarted" in _WRITE_DESC and
      "guest-shell" in _WRITE_DESC)
check("write-system-prompt-platform-boundary::names-and-hands-off-restart",
      "需要重启实例才能继续" in _WRITE_PROMPT and
      "ask whether the user wants the instance restarted" in flat(_WRITE_PROMPT) and
      "Do not bypass those limits" in _WRITE_PROMPT)
# The verdict is Chinese, so the sentence the user actually reads is pinned too.
check("write-system-prompt::states-the-boundary-in-the-verdict-language",
      "需要重启实例才能继续" in _WRITE_PROMPT)
# WHICH layer performs the restart is a fact for the model (it is why the guest shell is off
# limits), not a phrase for the customer. "平台级重启" says nothing a user can act on, and an
# English "platform-level restart" in the prompt is an invitation for the model to translate it
# straight into the verdict. Neither spelling may come back on either surface.
for _surface, _text in [("desc", _WRITE_DESC), ("prompt", _WRITE_PROMPT)]:
    check(f"write-prompts::no-layer-jargon-in-the-handoff::{_surface}",
          "platform-level" not in _text and "平台级" not in _text)
# The over-broad phrasing, verbatim. It is cheaper to name the sentence we removed than to
# rediscover why reboots stopped being mentioned.
for _surface, _text in [("desc", _WRITE_DESC), ("prompt", _WRITE_PROMPT)]:
    check(f"write-prompts::no-facts-gate-on-console-controls::{_surface}",
          "platform facts establish" not in _text)

# Load-bearing rules live in the prompt or tool description, not in a deleted skill.
_wp = harness.system_prompt(True)
_td = harness.tool_description(True)
# The launcher rule was stated in the system prompt and IGNORED (the run went straight to main.py and
# never opened /start.d — its log mtime never moved). It lives in the TOOL DESCRIPTION now: that is
# the only channel with evidence of landing (detach protocol adopted verbatim 2/2, system prompt
# rules 0/3). Asserted in BOTH directions so it cannot quietly drift back to the dead channel.
check("tooldesc-rule-prefer-image-launcher",
      "use its existing supervisor" in _td and "rather than starting an inner binary" in _td)
check("prompt-and-tool-align-on-managed-service-ownership",
      "identify its existing supervisor" in _wp and "use its existing supervisor" in _td)
check("prompts-drop-product-specific-launcher-patches",
      all(token not in (_wp + _td).lower() for token in ("filebrowser", "main.py", "/start.d/")))
# Replaces the before/after diff, which could not fire: in the fault under test BOTH ports start
# down, so "was up before" is empty. This asks about the ports the LAUNCHER defines instead, which is
# exactly the 7860 case — a port the repair never restored rather than one it broke.
check("tooldesc-rule-verify-every-launcher-port",
      "Verify the full service contract owned by the launcher" in _td)
check("prompt-no-longer-carries-port-diff", "list the listening ports" not in _wp)
check("prompt-rule-verdict-starts-with-status",
      all(token in _wp for token in ("`已修复`", "`部分修复`", "`未修复`")))
check("prompt-rule-verdict-stays-compact",
      "Then include `已完成` and, only when needed, `下一步`" in _wp and
      "结论 / 证据 / 确证vs推测" not in _wp)
check("prompt-rule-own-failed-commands",
      "attempts that ran but failed" in _wp and "label it as your action" in _wp and
      "pending or denied operation as executed" in _wp)
# The verdict shape must NOT be delegated to a skill the model never opens.
check("prompt-does-not-delegate-verdict-to-skill",
      "in the form `instance-repair` specifies" not in _wp)
# argv ceiling: a prior length probe (N=3/size) initialized cleanly through 6000 chars and died with
# an instant exit-1 near 12000. Stay inside the VERIFIED band, not merely under the cliff.
check("prompt-within-verified-length-band", len(_wp) < 6000)
check("readonly-prompt-untouched-by-repair-rules", "start it the way the image starts it" not in harness.tool_description(False))

# The four rules the deleted instance-repair skill carried now have to live where the model actually
# reads them, or deleting it lost something. Each is asserted against the live prompt/description
# rather than against a file, which is the whole point of the move.
check("repair-rule-executed-section-survives", "In `已完成`" in _wp)
check("repair-rule-long-job-lifecycle-survives",
      "structured background-job tools" in _td and "timed-out foreground command" in _td)
check("repair-rule-own-failed-attempts-survives",
      "attempts that ran but failed" in _wp and "label it as your action" in _wp)

# The working root is the real boundary, and it outlived the skills. The CLI discovers content by
# walking UP from cwd, so anything left on disk near it is reachable by a read-only run — that is how
# a repair playbook could once end up under a card that says 只读排查. Nothing is staged now, and BOTH
# modes must leave the root empty: a mode-dependent file here would be exactly that bug again.
_cwd = os.getcwd()
try:
    for allow in (False, True):
        root = harness.stage_clean_workdir(allow)
        check(f"working-root-empty-in-both-modes::allow={allow}", os.listdir(root) == [])
        shutil.rmtree(root, ignore_errors=True)
finally:
    os.chdir(_cwd)

# Destructive versus confirmable writes.
# What matters is WHICH TIER, not merely "not refused": `destructive` is refused outright, while
# `mutating` still makes the operator approve that exact string on a consent card. A command that
# slipped to `read_only` would execute with NO approval — strictly worse than the over-refusal we
# are fixing — so every case asserts its tier by name.
_NVML = "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.570.153.02"
for _name, _cmd in [
    # The fault was injected by renaming this library aside. The agent found it, named it exactly,
    # and then could not rename it back, because cp/mv/install into /usr was destructive.
    ("restore-renamed-system-library", f"mv {_NVML}.disabled {_NVML}"),
    ("restore-then-refresh-linker", f"mv {_NVML}.disabled {_NVML} && ldconfig && nvidia-smi"),
    # A read-only probe (fetch a port, print the body) refused because it tidied its own scratch file.
    ("probe-that-cleans-its-own-tmp-file",
     "curl -sS http://127.0.0.1:8888/ -o /tmp/b; head -c 99 /tmp/b; rm -f /tmp/b"),
    ("delete-one-named-non-system-file", "rm -f /tmp/jupyter-http-body.1234"),
    # This image keeps its own service log at /usr/local/jupyterlab.log; writing beside it was refused.
    ("write-a-log-under-usr-local", "echo starting >/usr/local/mylog.log"),
    # Repairing a broken service means editing where the service's config lives.
    ("edit-a-broken-service-config", "sed -i 's/^c.ServerApp.certfile/#&/' /etc/jupyter/config.py"),
    ("back-up-a-system-file-before-editing", "cp /etc/hosts /etc/hosts.bak"),
    ("remove-an-empty-directory", "rmdir /root/emptydir"),
    ("install-a-removable-sudoers-drop-in", "install -m 440 /tmp/x /etc/sudoers.d/90-x"),
]:
    check(f"approvable-tier::{_name}", guardrails.classify(_cmd) == "mutating")

# The narrowing must not have widened. `/boot`, `/sys`, `/proc` and raw disks stay hard-refused
# because a bad write there is unrecoverable or not reasonable to judge from a card, and deletion
# stays refused whenever the blast radius is recursive, glob-shaped, or a system path.
for _name, _cmd in [
    ("recursive-delete-of-root", "rm -rf /"),
    ("recursive-delete-of-a-data-dir", "rm -rf /root/models"),
    ("glob-delete-radius-not-readable", "rm -f /tmp/*.log"),
    ("delete-under-a-system-path", "rm /etc/hosts"),
    ("long-form-recursive-flag", "rm --recursive /var/data"),
    ("flag-order-does-not-matter", "rm -Rf /opt/app"),
    ("boot-chain-copy", "cp /tmp/x /boot/vmlinuz"),
    ("boot-chain-redirect", "echo x >/boot/grub/grub.cfg"),
    ("boot-chain-inplace-edit", "sed -i 's/a/b/' /boot/cfg"),
    ("kernel-interface-redirect", "echo 1 >/proc/sys/kernel/panic"),
    ("raw-disk-write", "dd if=/dev/zero of=/dev/sda"),
    ("filesystem-format", "mkfs.ext4 /dev/sdb1"),
    ("power-off", "shutdown -h now"),
    ("account-lockout", "usermod -L root"),
    ("management-channel-lockout", "systemctl stop sshd"),
    ("firewall-flush", "iptables -F"),
]:
    check(f"still-refused::{_name}", guardrails.classify(_cmd) == "destructive")

# Wrappers must not hide the inner command from the consent gate.
for _name, _cmd in [
    ("nice", "nice touch /root/marker"),
    ("nice-with-value-flag", "nice -n 5 touch /root/marker"),
    ("nice-with-numeric-flag", "nice -5 rm -f /root/marker"),
    ("nohup", "nohup touch /root/marker"),
    ("setsid", "setsid touch /root/marker"),
    ("stdbuf-attached-flag-value", "stdbuf -oL touch /root/marker"),
    ("stdbuf-separate-flag-value", "stdbuf -o L touch /root/marker"),
    ("timeout-eats-one-positional", "timeout 5 touch /root/marker"),
    ("timeout-with-value-flags", "timeout -s KILL -k 10 5 rm -f /root/marker"),
    ("busybox-applet", "busybox rm -f /root/marker"),
    ("command-builtin", "command touch /root/marker"),
    ("nested-wrappers", "nice nohup setsid touch /root/marker"),
    # No inner command to judge => fail closed, rather than waved through as an empty read.
    ("wrapper-with-no-inner-command", "ionice -c 3 -p 1234"),
    # Wrappers we cannot parse (positional-or-flag argument grammar) fail closed by name.
    ("unparseable-wrapper-taskset", "taskset 0x3 touch /root/marker"),
    ("unparseable-wrapper-flock", "flock /tmp/l touch /root/marker"),
]:
    check(f"wrapper-cannot-skip-the-card::{_name}", guardrails.classify(_cmd) == "mutating")

# Unwrapping must inherit the INNER tier in both directions — including read_only, or the fix would
# pay for itself by refusing the reads. `timeout 5 curl …` is how the agent avoids hanging on a
# dead port, so breaking it would cost real diagnostic ability.
for _name, _cmd in [
    ("timeout-guards-a-loopback-probe", "timeout 5 curl -sS -I http://127.0.0.1:8188"),
    ("nice-on-a-plain-read", "nice cat /proc/meminfo"),
    ("stdbuf-on-a-plain-read", "stdbuf -oL nvidia-smi -q"),
    ("command-v-only-looks-up", "command -v python3"),
]:
    check(f"wrapper-keeps-reads-readable::{_name}", guardrails.classify(_cmd) == "read_only")

for _name, _cmd in [
    ("wrapped-recursive-delete", "nice rm -rf /root/models"),
    ("wrapped-power-off", "nohup shutdown -h now"),
]:
    check(f"wrapper-cannot-soften-destructive::{_name}", guardrails.classify(_cmd) == "destructive")

# --- writers whose write hides in a FLAG, not in the binary name ---------------------------------
# All of these classified read_only, i.e. they wrote a file with no consent card. The last two are
# the standard "put arbitrary bytes at an arbitrary path" idioms, so this was not a corner case.
for _name, _cmd in [
    ("openssl-out", "openssl rand -out /root/key.bin 4096"),
    ("openssl-keyout", "openssl req -x509 -keyout /root/k.pem -out /root/c.pem"),
    ("sort-output-flag", "sort -o /root/sorted /root/raw"),
    ("split-always-writes", "split -b 1M /root/big /root/part"),
    ("csplit-always-writes", "csplit /root/f 100"),
    ("base64-decode-to-a-path", "base64 -d -o /root/out /tmp/in"),
    ("xxd-revert-produces-bytes", "xxd -r /tmp/in /root/out"),
    ("mknod-creates-a-device", "mknod /root/n c 1 3"),
]:
    check(f"hidden-write-needs-the-card::{_name}", guardrails.classify(_cmd) == "mutating")

# A blanket "-o means output" rule would have been wrong: on these binaries -o selects the output
# FORMAT and they are core diagnostics. This pins that the scoping stayed per-binary.
for _name, _cmd in [
    ("ps-o-is-a-format", "ps -o pid,comm,etime"),
    ("lsblk-o-is-a-format", "lsblk -o NAME,SIZE,MOUNTPOINT"),
    ("sort-without-output-flag", "sort /root/raw"),
    ("base64-to-stdout", "base64 /root/f"),
]:
    check(f"format-flag-is-not-a-write::{_name}", guardrails.classify(_cmd) == "read_only")

# --- a path rule must survive being respelled ----------------------------------------------------
# The destructive tier contains PATH rules, and a regex over the raw string read `//etc/fstab` and
# `/tmp/../etc/fstab` as different paths from `/etc/fstab` while the kernel does not. Both were
# accepted as `mutating` — one consent card away from a write the tier refuses outright. Likewise
# brace and bracket expansion made one `rm` delete an unknown number of files while the
# system-path rule's own comment claimed such deletes stay refused.
#
# Every case here was chosen by checking that the OLD patterns miss the raw string — `/etc/./hosts`
# and `/var/lib/../lib/...` look like respellings but the pre-existing system-path rule already
# matched them verbatim, so asserting on those would have passed with this fix reverted.
for _name, _cmd in [
    ("double-slash", "rm -f //etc/hosts"),
    ("parent-dir-traversal", "rm -f /tmp/../etc/hosts"),
    ("dot-segment-hiding-the-prefix", "rm -f /var/./lib/mysql/ibdata1"),
    ("respelled-inplace-edit-of-fstab", "sed -i s/a/b/ //etc/fstab"),
    ("respelled-copy-onto-fstab", "cp /tmp/x //etc/fstab"),
    ("respelled-write-to-var-lib", "echo x > /var/./lib/mysql/ibdata1"),
    ("brace-expansion", "rm -f /tmp/{a,b}"),
    ("bracket-expansion", "rm -f /root/log[12].txt"),
]:
    check(f"respelling-cannot-dodge-a-path-rule::{_name}", guardrails.classify(_cmd) == "destructive")

# --- the carve-outs from the narrowing -----------------------------------------------------------
# /etc moved to `mutating` because that is where a broken service's config lives. These paths are
# not service config: a bad fstab means the box does not boot, the account and ssh files end the
# only channel we would have to fix our own mistake, and /var/lib is live database/container state.
for _name, _cmd in [
    ("fstab-truncating-tee", "sudo tee /etc/fstab"),
    ("fstab-inplace-edit", "sed -i s/a/b/ /etc/fstab"),
    ("fstab-moved-away-is-also-fatal", "mv /etc/fstab /tmp/fstab.bak"),
    ("shadow-overwrite", "cp /tmp/shadow /etc/shadow"),
    ("sshd-config-overwrite", "echo x > /etc/ssh/sshd_config"),
    ("live-database-state", "echo x > /var/lib/mysql/ibdata1"),
    ("blank-a-system-file-via-dev-null", "install -m 000 /dev/null /etc/resolv.conf"),
]:
    check(f"lockout-path-stays-refused::{_name}", guardrails.classify(_cmd) == "destructive")

# Reading a lockout path, and backing one up before editing, must stay allowed — refusing those is
# the exact over-strictness this re-tiering exists to remove. cp/install/ln write only their LAST
# argument, so the source position is not a write.
for _name, _cmd, _want in [
    ("read-fstab", "cat /etc/fstab", "read_only"),
    ("list-a-state-dir", "ls -la /var/lib/docker", "read_only"),
    ("size-a-state-dir", "du -sh /var/lib/docker", "read_only"),
    ("back-up-fstab-before-editing", "cp /etc/fstab /tmp/fstab.bak", "mutating"),
    ("copy-passwd-out-for-inspection", "cp /etc/passwd /tmp/p", "mutating"),
    ("blank-a-tmp-file-via-dev-null", "cp /dev/null /tmp/marker", "mutating"),
]:
    check(f"lockout-path-stays-readable::{_name}", guardrails.classify(_cmd) == _want)

# Zero-length truncation is a confirmable disk-space repair; growing a file remains destructive.
for _name, _cmd, _want in [
    ("zero-a-log-is-the-repair", "truncate -s 0 /var/log/bigblob.bin", "mutating"),
    ("zero-via-a-held-open-fd", "truncate -s 0 /proc/633/fd/3", "mutating"),
    ("long-form-zero", "truncate --size=0 /root/big.log", "mutating"),
    # Growing a file is not reclaiming space; it is allocating it. Still refused.
    ("grow-a-file-still-refused", "truncate -s 10G /root/big.log", "destructive"),
    # The exemption is the ZERO form only — it does not reach past the lockout/kernel rules.
    ("zero-a-lockout-path", "truncate -s 0 /etc/fstab", "destructive"),
    ("zero-a-kernel-interface", "truncate -s 0 /proc/sys/kernel/panic", "destructive"),
    ("write-verb-into-sysfs", "tee /proc/sys/vm/drop_caches", "destructive"),
    ("write-verb-into-dev", "cp /tmp/x /dev/watchdog", "destructive"),
]:
    check(f"space-reclaim-tiering::{_name}", guardrails.classify(_cmd) == _want)

# SSH configuration edits and daemon/network restarts remain destructive: a bad configuration can
# make the re-exec fail and permanently remove the only management path.
for _name, _cmd, _want in [
    ("restart-with-an-already-bad-config", "systemctl restart sshd", "destructive"),
    ("reload-re-execs-and-can-die", "systemctl reload ssh", "destructive"),
    ("sysvinit-form", "service ssh restart", "destructive"),
    ("networking-too", "systemctl restart networking", "destructive"),
    # `start` is the RECOVERY direction: it cannot lose a daemon that is already down.
    ("start-is-recovery-not-lockout", "systemctl start sshd", "mutating"),
    # Nothing here may spill onto ordinary services.
    ("ordinary-service-restart-unaffected", "systemctl restart jupyterlab", "mutating"),
    # Diagnosis of this category stays fully available — that IS the product here.
    ("read-the-config", "cat /etc/ssh/sshd_config", "read_only"),
    ("validate-without-applying", "/usr/sbin/sshd -t", "read_only"),
]:
    check(f"ssh-channel-tiering::{_name}", guardrails.classify(_cmd) == _want)

# A partial run that changed the box must list those changes.
del harness.AUDIT[:]
harness.AUDIT.extend([
    {"command": "systemctl status infersvc", "tier": "read_only", "disposition": "ran_read_only"},
    {"command": "apt-get install -y python3-tomli", "tier": "mutating", "disposition": "ran_mutating"},
    {"command": "systemctl restart infersvc", "tier": "mutating", "disposition": "ran_mutating"},
    # Refused and not-approved writes changed nothing, so they must NOT be listed as changes.
    {"command": "rm -rf /", "tier": "destructive", "disposition": "refused_destructive"},
    {"command": "chmod 777 /etc", "tier": "mutating", "disposition": "refused_not_approved"},
])
_note = harness._partial_note("500 fetch failed")
check("partial-note::says-the-box-was-changed", "已经被改动过" in _note)
check("partial-note::does-not-claim-read-only", "基于已执行只读命令" not in _note)
check("partial-note::lists-the-writes-that-ran",
      "apt-get install -y python3-tomli" in _note and "systemctl restart infersvc" in _note)
check("partial-note::omits-the-refused-ones",
      "rm -rf /" not in _note and "chmod 777 /etc" not in _note)
check("partial-note::keeps-the-error", "500 fetch failed" in _note)

# With no write having executed, the old wording is the correct wording — and it now says so
# positively ("没有执行任何写操作") instead of leaving the reader to infer it.
del harness.AUDIT[:]
harness.AUDIT.extend([
    {"command": "systemctl status infersvc", "tier": "read_only", "disposition": "ran_read_only"},
    {"command": "systemctl restart infersvc", "tier": "mutating", "disposition": "refused_mutating_phase1"},
])
_note = harness._partial_note("timed out")
check("partial-note::read-only-run-still-says-read-only", "基于已执行只读命令" in _note)
check("partial-note::read-only-run-states-no-writes", "没有执行任何写操作" in _note)
check("partial-note::read-only-run-claims-no-change", "已经被改动过" not in _note)
del harness.AUDIT[:]

# The remote process group, not only the local SSH channel, must enforce the time bound.
_CMD = "grep -rn 'x' / | head -20"
_W = ssh_transport._bounded(_CMD)
check("remote-bound::asks-the-box-to-enforce-it", f"timeout -k 5 {ssh_transport._REMOTE_TIMEOUT}" in _W)
check("remote-bound::bound-is-shorter-than-ours",
      ssh_transport._REMOTE_TIMEOUT < ssh_transport._EXEC_TIMEOUT)
# bash, not sh: generated commands may use bash syntax.
check("remote-bound::runs-under-bash", "bash -c" in _W and "sh -c" not in _W.replace("bash -c", ""))
# On a box without `timeout` the original must still run, byte-identical to pre-wrapper behaviour.
check("remote-bound::degrades-to-the-original", _W.rstrip().endswith(_CMD))
check("remote-bound::guards-on-timeout-existing", "command -v timeout" in _W)
# The model's own text must survive quoting intact — it is what the operator approved on the card.
_TRICKY = """awk '$4=="0A"' /proc/net/tcp | grep -c ':1FFC'"""
check("remote-bound::preserves-quotes-in-the-payload",
      shlex.split(ssh_transport._bounded(_TRICKY).split("bash -c ")[1].rsplit("; fi;", 1)[0])[0]
      == _TRICKY)
# The wrapper is a transport concern only. My first attempt asserted the wrapper would classify
# DIFFERENTLY from the original, on the theory that its `exec`/`bash -c` would be caught and so a
# reversed order would fail loudly. It does not: the wrapper classifies read_only too, because
# `exec` is not token 0 of its segment. Asserting the real invariant instead — what the audit
# records and what the consent card shows is the MODEL's string, never our wrapper. That is the
# property that matters: the operator approves what they were shown, and that is what runs.
_res, _entry = dispatch("echo x > /tmp/f", allow_writes=True)
check("remote-bound::audit-records-the-models-text", _entry["command"] == "echo x > /tmp/f")
check("remote-bound::audit-has-no-wrapper", "timeout -k" not in _entry["command"])
with confirm_stub.approving() as _io:
    harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw", "allow_writes": True})
    harness.run_command("echo x > /tmp/f")
check("remote-bound::card-shows-the-models-text",
      len(_io.requests) == 1 and _io.requests[0]["command"] == "echo x > /tmp/f")

# Shell keywords are wrappers too and must not hide the first executable.
for _name, _cmd in [
    ("if-then-delete", "if true; then rm -f /root/marker; fi"),
    ("if-then-write-to-etc", "if true; then touch /etc/x; fi"),
    ("for-do-delete", "for f in a b; do rm -f /root/$f; done"),
    ("while-do-write", "while true; do touch /root/m; done"),
    ("time-prefix", "time touch /root/marker"),
    ("negation-prefix", "! touch /root/marker"),
    ("bare-then", "then touch /root/marker"),
    ("bare-do", "do touch /root/marker"),
    ("bare-else", "else rm -f /root/marker"),
    ("brace-group", "{ touch /root/marker; }"),
    ("keyword-plus-sudo", "then sudo touch /root/marker"),
]:
    check(f"keyword-cannot-skip-the-card::{_name}", guardrails.classify(_cmd) == "mutating")

# Keywords that end a construct change nothing, so they must stay reads rather than fail closed —
# otherwise every `fi`/`done` segment in an accepted chain would demand an approval.
for _name, _cmd in [("fi", "fi"), ("done", "done"), ("esac", "esac"), ("while-true", "while true")]:
    check(f"bare-keyword-is-not-a-write::{_name}", guardrails.classify(_cmd) == "read_only")

# And the destructive tier is unaffected — the construct's own segment carries the dangerous verb,
# so per-command scanning (see _scan_destructive) still catches both of these.
for _name, _cmd in [("if-then-chmod", "if [ -f /x ]; then chmod 777 /etc/passwd; fi"),
                    ("for-do-rm-rf", "for d in /a /b; do rm -rf $d; done")]:
    check(f"keyword-cannot-soften-destructive::{_name}", guardrails.classify(_cmd) == "destructive")

# Fifth spelling of the same asymmetry: the subcommand-sensitive rules (_MUTATING_FORMS) are
# ^-anchored, so an ABSOLUTE PATH to the binary missed the anchor and the write fell through to
# read_only with no card. The table-driven writers were never exposed, because those are looked up
# by basename — which is what made this one survive four earlier passes over the same defect.
# Found by classifying the repair commands for a live fixture BEFORE running it: f1's fix is
# `pip install tomli`, and the model writes it as `/usr/bin/pip3 install tomli` about as often.
for _name, _cmd in [
    ("pip3-abs", "/usr/bin/pip3 install tomli"),
    ("pip-usr-local", "/usr/local/bin/pip install tomli"),
    ("systemctl-abs", "/usr/bin/systemctl restart infersvc"),
    ("supervisorctl-abs", "/usr/bin/supervisorctl restart ollama"),
    ("apt-get-abs", "/usr/bin/apt-get install -y tomli"),
    ("docker-abs", "/usr/bin/docker run -d img"),
    ("git-abs", "/usr/bin/git clone https://example/x"),
    ("sudo-plus-abs", "sudo /usr/bin/pip3 install tomli"),
]:
    check(f"abs-path-cannot-skip-the-card::{_name}", guardrails.classify(_cmd) == "mutating")

# The ^ anchor exists so a PATH containing the verb cannot trip these rules. Re-spelling token 0
# must not cost that: these are reads and have to stay reads, or core diagnostics start carding.
for _name, _cmd in [
    ("cat-installer-log", "cat /var/log/cuda-installer.log"),
    ("cat-pip-install-log", "cat /opt/pip-install.log"),
    ("grep-install-in-log", "grep -n install /opt/pip-install.log"),
    ("ls-the-binary", "ls -l /usr/bin/pip3"),
    ("systemctl-status-abs", "/usr/bin/systemctl status ollama"),
    ("pip-list-abs", "/usr/bin/pip3 list"),
    ("pip-version-abs", "/usr/bin/pip3 --version"),
]:
    check(f"abs-path-keeps-reads-readable::{_name}", guardrails.classify(_cmd) == "read_only")

# The destructive tier matches on \b, not ^, so it was never path-fragile. Pin that, so a future
# refactor of the respelling cannot quietly move these down a tier.
for _name, _cmd in [("systemctl-restart-sshd-abs", "/usr/bin/systemctl restart sshd"),
                    ("rm-rf-etc-ssh-abs", "/bin/rm -rf /etc/ssh")]:
    check(f"abs-path-cannot-soften-destructive::{_name}", guardrails.classify(_cmd) == "destructive")

# Separate commands must not combine into a false destructive match. Assert end-to-end which gate
# the user reaches, not only the classifier tier.
for _name, _cmd in [
    ("perms-then-read-proc", "chmod u+rx /root/models; awk '{print}' /proc/17146/status"),
    ("kill-squatter-then-check-ssh", "pkill -f squatter.py; systemctl status sshd"),
    ("perms-then-recursive-listing", "chmod 755 /workspace/app; ls -R /workspace"),
    ("tidy-then-look-at-etc", "rm /tmp/probe.txt; ls /etc"),
    ("back-up-then-read-proc", "cp /etc/nginx/nginx.conf /tmp/bak; cat /proc/cpuinfo"),
]:
    _res, _entry = dispatch(_cmd, allow_writes=True)
    check(f"chain-reaches-the-card::{_name}",
          _res["executed"] is True and _entry["disposition"] == "ran_mutating")
    # Refused with writes OFF, and refused as MUTATING — the fix must not have made a chain
    # containing a write auto-run, which would be the same bug pointing the other way.
    _res, _entry = dispatch(_cmd, allow_writes=False)
    check(f"chain-still-needs-the-gate::{_name}",
          _res["executed"] is False and _entry["disposition"] == "refused_mutating_phase1")

# Per-command scanning re-anchors `^` and `$` to the command, which CLOSES two card-buying bypasses
# a whole-string scan had: both of these were `mutating` before, i.e. one approval away from a write
# the tier refuses outright.
for _name, _cmd in [
    ("borrowed-the-zero-size-exemption", "truncate -s 10G /workspace/big; echo -s 0"),
    ("evaded-the-destination-anchor", "cp /tmp/x /var/lib/mysql/ibdata1; ls /tmp"),
]:
    _res, _entry = dispatch(_cmd, allow_writes=True)
    check(f"chain-cannot-buy-a-card::{_name}",
          _res["executed"] is False and _entry["disposition"] == "refused_destructive")

ssh_transport.run_ssh = _REAL_RUN_SSH

print(f"\n{'FAILED: ' + ', '.join(FAILS) if FAILS else 'all write-mode checks passed'}")
sys.exit(1 if FAILS else 0)
