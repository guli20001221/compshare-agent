"""Offline gate for the single confirmation-gated repair mode (no network / no SSH / no SDK).

Run:  python test_write_mode.py   ->  exits non-zero on ANY failure.

The product-level read-only/write split is deliberately gone. Three properties have to hold:

  1. commands positively proven read-only execute immediately;
  2. every other reversible guest-local effect reaches an exact confirmation card;
  3. destructive/boundary effects and unsupported command shapes remain refused.

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


def dispatch(command):
    """Run one command through the real classifier with an approving exact confirmer.

    test_confirm_loop.py covers denial, timeout, stale replies and missing confirmation channels;
    this file checks which operations may reach that card and which are hard-refused.
    """
    harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw"})
    del harness.AUDIT[:]
    with confirm_stub.approving():
        res = harness.run_command(command)
    return res, harness.AUDIT[-1]


# --- reversible guest-local effects execute only after the exact approval ------------------------
for cmd in ["systemctl restart ollama", "pip install torch", "kill -9 4321",
            "echo x > /tmp/f", "mkdir /tmp/d", "chmod 644 /tmp/f",
            "chmod 777 /workspace/app", "chattr +i /workspace/model.bin",
            "systemctl disable vllm", "swapoff -a",
            "install -m 440 /tmp/x /etc/sudoers.d/90-x"]:
    res, entry = dispatch(cmd)
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
    res, entry = dispatch(cmd)
    check(f"write-still-refuses-destructive::{cmd}",
          res["executed"] is False and entry["disposition"] == "refused_destructive")
    check(f"destructive-tier-intact::{cmd}", guardrails.classify(cmd) == "destructive")

# --- (3b) substitution stays refused; multi-line scripts use the ordinary exact card -------------
# Command substitution hides the command text from classify(), so allowing it in write mode would
# turn the destructive list into decoration. A literal multi-line script does not: destructive
# scanning is per line, and an otherwise reversible script is shown in full on one approval card.
for cmd in ["cat $(which python3)", "echo `whoami`", "systemctl restart $(cat /tmp/svc)"]:
    res, entry = dispatch(cmd)
    check(f"write-still-refuses-shape::{cmd[:28]!r}",
          res["executed"] is False and entry["disposition"] == "refused_form")

res, entry = dispatch("printf one\nprintf two")
check("multiline-proven-read-script-runs-without-card",
      res["executed"] is True and entry["disposition"] == "ran_read_only")
res, entry = dispatch("mkdir /tmp/multiline-canary\nprintf two")
check("multiline-reversible-write-uses-exact-card",
      res["executed"] is True and entry["disposition"] == "ran_mutating")
res, entry = dispatch("echo one\nrm -rf /")
check("multiline-destructive-script-still-refused",
      res["executed"] is False and entry["disposition"] == "refused_destructive")

# A refusal the model cannot act on wastes the turn. In write mode the read-only wording ("this
# changes the box") is actively wrong: it sends the agent looking for a permission it already has.
res, _ = dispatch("cat $(which python3)")
check("write-shape-refusal-explains-form", "FORM rejected" in res["text"])
check("write-shape-refusal-not-readonly-wording", "read-only" not in res["text"])

# --- proven reads still run immediately -----------------------------------------------------------
res, entry = dispatch("nvidia-smi")
check("proven-read-runs-without-a-write-tier", res["executed"] is True and
      entry["disposition"] == "ran_read_only")

# --- one prompt and one tool contract: diagnose, confirm exact repairs, then verify --------------
_WRITE_PROMPT = harness.SYSTEM_PROMPT
_WRITE_PROMPT_FLAT = flat(_WRITE_PROMPT)
_WRITE_DESC = flat(harness.TOOL_DESC)
check("prompt-authorizes-confirmation-gated-repair",
      "authorized the repair workflow" in _WRITE_PROMPT_FLAT and
      "requires approval of that exact effect" in _WRITE_PROMPT_FLAT)
check("prompt-names-hard-limits",
      "Hard refusal is only" in _WRITE_PROMPT and
      "tenant/control-plane boundary" in _WRITE_PROMPT)
check("prompt-contract::shared-core", _WRITE_PROMPT.startswith(harness._SYSTEM_PROMPT_CORE))
check("prompt-contract::structured",
      all(section in _WRITE_PROMPT for section in ("## Evidence model", "## Diagnostic loop",
                                                    "## Authorization:", "## Final response")))
check("prompt-contract::layered-evidence",
      all(layer in _WRITE_PROMPT for layer in ("Control-plane metadata", "guest state",
                                                "application state", "external reachability")))
check("prompt-contract::outcome-driven",
      "observable success criterion" in _WRITE_PROMPT and "original success criterion" in _WRITE_PROMPT)
check("prompt-contract::actual-runtime",
      "application's real" in _WRITE_PROMPT and "virtualenv or Conda" in _WRITE_PROMPT)
check("prompt-contract::managed-ownership-invariant",
      "controller's ownership state" in _WRITE_PROMPT and "drift rather than proof" in _WRITE_PROMPT)
check("prompt-contract::reconciles-owner-before-start-and-polls-terminal-state",
      "Reconcile an existing ownership conflict" in _WRITE_PROMPT and
      "bounded-poll transitional manager states" in _WRITE_PROMPT and "terminal result" in _WRITE_PROMPT)
check("prompt-contract::single-mode-only",
      "Authorization: diagnose, repair, verify" in _WRITE_PROMPT and
      "Authorization: diagnosis only" not in _WRITE_PROMPT)
check("prompt-contract::no-incident-specific-patches",
      all(token not in _WRITE_PROMPT.lower()
          for token in ("filebrowser", "main.py", "/start.d/", "8188", "comfyui")))
check("prompt-contract::within-argv-safe-band", len(_WRITE_PROMPT) < 5000)
check("prompt-does-not-arbitrate-skills", "OVERRIDE" not in _WRITE_PROMPT)
check("prompt-names-no-skill", "skill" not in _WRITE_PROMPT.lower())

check("tool-desc-does-not-forbid-writes", "Read-only commands only" not in _WRITE_DESC)
check("tool-desc-says-changes-run",
      "For repair" in _WRITE_DESC and "approves that exact command" in _WRITE_DESC)
# The contract tells the model to submit an evidence-backed action rather than stopping at prose.
check("tool-desc-says-send-not-describe", "send the smallest concrete command" in _WRITE_DESC)
# Hard limits stay stated so the agent does not plan around commands the executor will reject.
check("tool-desc-keeps-hard-limits",
      "irreversible" in _WRITE_DESC.lower() and "control-plane crossings" in _WRITE_DESC and
      "are refused" in _WRITE_DESC)
check("tool-desc-forbids-invented-platform-entrypoints",
      "platform-facing port" in _WRITE_DESC and "substitute service" in _WRITE_DESC)
# Tool descriptions are behavioral contracts, not a growing list of executable-path exceptions.
check("tool-desc-states-execution-semantics",
      all(needle in _WRITE_DESC.lower() for needle in
          ("fresh, non-interactive ssh session", "25 seconds", "positively proven",
           "exit status")))
check("tool-description-prefers-the-actual-runtime",
      "application's actual interpreter" in _WRITE_DESC)
check("tool-description-requires-managed-ownership",
      "surviving" in _WRITE_DESC and "outside that ownership" in _WRITE_DESC)
check("tool-description-reconciles-owner-and-polls-transition",
      "reconcile any surviving child outside that ownership" in _WRITE_DESC and
      "transitional manager states" in _WRITE_DESC and "terminal result" in _WRITE_DESC)
check("tool-desc-contract-stays-general",
      all(token not in _WRITE_DESC.lower()
          for token in ("filebrowser", "main.py", "/start.d/", "8188", "python -c")))
check("tool-desc-contract-stays-focused", len(_WRITE_DESC) < 2000)

# --- the description must not forbid what the executor allows ------------------------------------
# The description must match executable policy: supported chaining, pipes and redirection cannot be
# described as forbidden.
check("gate-actually-allows-redirect",
      guardrails.classify("nohup python3 /root/app/start.py > /root/app.log 2>&1 &") == "mutating"
      and guardrails.is_form_violation("nohup python3 /root/app/start.py > /root/app.log 2>&1 &")
      is False)
check("gate-actually-allows-pipe", guardrails.classify("ps aux | grep -i comfy") == "read_only")
check("tool-desc-drops-false-shape-clause",
      "no chaining/pipes/redirection" not in _WRITE_DESC)
# Backgrounding is one execution mode of ssh_exec, not a parallel shell tool. The description owns
# the lifecycle contract and must not teach a hand-rolled protocol that can drift from it.
check("tool-desc-routes-long-work-to-ssh-exec-background-mode",
      "run_in_background=true" in flat(_WRITE_DESC) and "do not hand-roll" in _WRITE_DESC.lower() and
      "At most one background job may be active" in flat(_WRITE_DESC))
check("surface-has-one-shell-tool-plus-a-read-only-poll",
      "mcp__ssh_ops__ssh_exec" in harness.ALLOWED_TOOLS and
      "mcp__ssh_ops__poll_background_job" in harness.ALLOWED_TOOLS and
      all("start_background_job" not in name for name in harness.ALLOWED_TOOLS))

# The lane has no skills or built-in Skill tool; policy lives in the prompt and executable tools.
check("skills-are-gone-from-the-module", not hasattr(harness, "skills_for") and not hasattr(harness, "SKILLS"))
check("skills-directory-is-gone",
      not os.path.exists(os.path.join(os.path.dirname(os.path.abspath(harness.__file__)), "skills")))
check("prompt-has-no-repair-skill", "instance-repair" not in _WRITE_PROMPT)
# Neither prompt may tell the model to load a playbook that no longer exists — an instruction to open
# a missing document is worse than the one it replaced, since it invites answering as though one had.
check("prompt-does-not-claim-skill-load", "skill" not in _WRITE_PROMPT.lower())
# The Skill TOOL goes with them. It was the single permitted built-in, justified only by "skills are
# how the playbook reaches the model"; with no playbook that justification is empty, and INV-9 is
# back to its original posture — no built-in exists on the control-plane host.
check("skill-tool-no-longer-exists", harness.TOOLS_BASE == [])
check("atomic-file-tool-exists-in-single-repair-surface",
      "mcp__ssh_ops__atomic_text_edit" in harness.ALLOWED_TOOLS)
check("remote-text-tool-exists-in-single-repair-surface",
      "mcp__ssh_ops__read_text_file" in harness.ALLOWED_TOOLS)
check("remote-search-tool-exists-in-single-repair-surface",
      "mcp__ssh_ops__search_text_tree" in harness.ALLOWED_TOOLS)
check("remote-glob-tool-exists-in-single-repair-surface",
      "mcp__ssh_ops__find_paths" in harness.ALLOWED_TOOLS)
check("process-environment-tool-exists-in-single-repair-surface",
      "mcp__ssh_ops__read_process_environment" in harness.ALLOWED_TOOLS)
check("endpoint-probe-exists-in-single-repair-surface",
      "mcp__ssh_ops__endpoint_probe" in harness.ALLOWED_TOOLS)
check("guest-endpoint-probe-exists-in-single-repair-surface",
      "mcp__ssh_ops__guest_endpoint_probe" in harness.ALLOWED_TOOLS)
check("atomic-file-tool-is-hash-bound-and-backed-up",
      all(term in harness.atomic_file.TOOL_DESCRIPTION
          for term in ("SHA-256", "same-directory temporary", "recoverable backup",
                       "Create never overwrites")))
check("remote-text-tool-is-bounded-read-only-and-hash-bearing",
      all(term in harness.remote_text.TOOL_DESCRIPTION
          for term in ("read-only", "32 KiB", "whole-file SHA-256", "follows no symlink")))
check("remote-text-tool-routes-known-files-away-from-shell-readers",
      "instead of cat/head/sed" in harness.remote_text.TOOL_DESCRIPTION)
check("remote-search-tool-is-bounded-and-routes-to-exact-read",
      all(term in harness.remote_search.TOOL_DESCRIPTION
          for term in ("literal text fragment", "never follows symlinks", "8 MiB",
                       "read_text_file", "not recursive grep through ssh_exec",
                       "validates every descendant")))
check("prompt-routes-recursive-content-search-to-structured-tool",
      "search_text_tree (not recursive shell grep)" in harness.SYSTEM_PROMPT
      and "use search_text_tree, not" in flat(harness.TOOL_DESC))
check("remote-glob-tool-is-bounded-and-does-not-run-a-shell",
      all(term in harness.remote_search.FIND_DESCRIPTION
          for term in ("basename glob", "invokes no remote shell", "never follows symlinks",
                       "100 paths")))
check("process-environment-tool-is-selected-and-secret-bounded",
      all(term in harness.process_env.TOOL_DESCRIPTION
          for term in ("caller-selected", "schema allowlist", "credentials")))

# The lane repairs only the assigned fault; broader application replacement or shutdown requires a
# separate user decision.
for _name, _needle in [
    ("states the scope", "Repair the diagnosed fault only"),
    ("names redeploying an app", "Re-downloading an app"),
    ("names taking a service down", "disabling an unrelated service"),
    ("routes it to the user rather than forbidding it",
     "needs explicit intent in an available user report"),
    ("keeps prior user intent bounded to continuation", "prior reports only continue unfinished requests"),
]:
    check(f"tool-desc-bounds-scope::{_name}", _needle in _WRITE_DESC)

# A form refusal is recoverable by changing syntax, unlike a denied/expired
# confirmation. The model must receive that distinction directly from its two
# authoritative prompt surfaces, and neither should call the user an "operator".
check("write-tool-desc::reformats-form-refusals",
      "Rewrite only a rejected form" in _WRITE_DESC)
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
check("write-prompts::distinguish-service-restart-from-instance-reboot",
      "A process or service restart is not an instance reboot" in _WRITE_PROMPT_FLAT and
      "A process or service restart is not an instance reboot" in _WRITE_DESC and
      "guest-local restart cannot recover" in _WRITE_PROMPT_FLAT and
      "guest-local restart cannot recover" in _WRITE_DESC)
check("write-tool-desc::separates-independent-probes",
      "Each call is one effect" in _WRITE_DESC and
      "split independent probes" in _WRITE_DESC)
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
_wp = _WRITE_PROMPT
_td = _WRITE_DESC
# The launcher rule was stated in the system prompt and IGNORED (the run went straight to main.py and
# never opened /start.d — its log mtime never moved). It lives in the TOOL DESCRIPTION now: that is
# the only channel with evidence of landing (detach protocol adopted verbatim 2/2, system prompt
# rules 0/3). Asserted in BOTH directions so it cannot quietly drift back to the dead channel.
check("tooldesc-rule-prefer-image-launcher",
      "use its existing supervisor/launcher" in _td and "not an inner binary" in _td)
check("tooldesc-rule-does-not-invent-manager-ownership",
      "Do not create a new unit merely because a manager exists" in _td and
      "only a launcher exists" in _td and "report the durability gap" in _td)
check("prompt-and-tool-do-not-invent-traceback-semantics",
      "A traceback proves a failure site, not intended" in _wp and
      "A traceback proves failure site, not intended semantics" in _td and
      "local test" in _wp and "version contract" in _wp and
      "reversible rollback/disable within scope" in _wp and
      "reversible rollback/disable within scope" in _td)
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
      all(token in _wp for token in ("`已修复`", "`部分修复`", "`未修复`", "`无需修复`")) and
      "never describe a read-only check itself as a repair" in _wp)
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
# The four rules the deleted instance-repair skill carried now have to live where the model actually
# reads them, or deleting it lost something. Each is asserted against the live prompt/description
# rather than against a file, which is the whole point of the move.
check("repair-rule-executed-section-survives", "In `已完成`" in _wp)
check("repair-rule-long-job-lifecycle-survives",
      "run_in_background=true" in _td and "timed-out foreground command" in _td and
      "terminal poll frees the slot" in _td)
check("repair-rule-own-failed-attempts-survives",
      "attempts that ran but failed" in _wp and "label it as your action" in _wp)

# The working root is the real boundary, and it outlived the skills. The CLI discovers content by
# walking UP from cwd, so anything left on disk near it is reachable by the operations agent. Nothing
# is staged now, and the single mode must leave the root empty.
_cwd = os.getcwd()
try:
    root = harness.stage_clean_workdir()
    check("working-root-empty-in-single-mode", os.listdir(root) == [])
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
    ("sort-without-output-flag", "sort /etc/os-release"),
    ("base64-to-stdout", "base64 /etc/os-release"),
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

# A partial run lists confirmed commands without claiming that each one changed the instance.
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
check("partial-note::says-confirmed-commands-may-affect-state", "其中可能包含影响实例状态的操作" in _note)
check("partial-note::does-not-assert-the-write-happened", "已经被改动过" not in _note)
check("partial-note::does-not-claim-read-only", "只执行了已证明为只读的命令" not in _note)
check("partial-note::lists-confirmed-commands-that-ran",
      "apt-get install -y python3-tomli" in _note and "systemctl restart infersvc" in _note)
check("partial-note::omits-the-refused-ones",
      "rm -rf /" not in _note and "chmod 777 /etc" not in _note)
check("partial-note::keeps-the-error", "500 fetch failed" in _note)

# With nothing but proven-read-only commands executed, the note may state that positively: this is
# the one direction the guardrail's verdict actually licenses.
del harness.AUDIT[:]
harness.AUDIT.extend([
    {"command": "systemctl status infersvc", "tier": "read_only", "disposition": "ran_read_only"},
    {"command": "systemctl restart infersvc", "tier": "mutating", "disposition": "refused_mutating_phase1"},
])
_note = harness._partial_note("timed out")
check("partial-note::read-only-run-still-says-read-only", "只执行了已证明为只读的命令" in _note)
check("partial-note::read-only-run-claims-no-change", "改动过这台实例" not in _note)
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
_res, _entry = dispatch("echo x > /tmp/f")
check("remote-bound::audit-records-the-models-text", _entry["command"] == "echo x > /tmp/f")
check("remote-bound::audit-has-no-wrapper", "timeout -k" not in _entry["command"])
with confirm_stub.approving() as _io:
    harness.set_conn({"host": "h", "user": "u", "port": 22, "password": "pw"})
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

# Standalone syntax fragments are not positively proven reads. They therefore reach confirmation
# instead of inheriting the old unsafe "unknown means read-only" default.
for _name, _cmd in [("fi", "fi"), ("done", "done"), ("esac", "esac"), ("while-true", "while true")]:
    check(f"bare-keyword-is-not-auto-run::{_name}", guardrails.classify(_cmd) == "mutating")

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
    ("cat-cuda-version", "cat /usr/local/cuda/version.txt"),
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
    _res, _entry = dispatch(_cmd)
    check(f"chain-reaches-the-card::{_name}",
          _res["executed"] is True and _entry["disposition"] == "ran_mutating")

# Per-command scanning re-anchors `^` and `$` to the command, which CLOSES two card-buying bypasses
# a whole-string scan had: both of these were `mutating` before, i.e. one approval away from a write
# the tier refuses outright.
for _name, _cmd in [
    ("borrowed-the-zero-size-exemption", "truncate -s 10G /workspace/big; echo -s 0"),
    ("evaded-the-destination-anchor", "cp /tmp/x /var/lib/mysql/ibdata1; ls /tmp"),
]:
    _res, _entry = dispatch(_cmd)
    check(f"chain-cannot-buy-a-card::{_name}",
          _res["executed"] is False and _entry["disposition"] == "refused_destructive")

ssh_transport.run_ssh = _REAL_RUN_SSH

print(f"\n{'FAILED: ' + ', '.join(FAILS) if FAILS else 'all write-mode checks passed'}")
sys.exit(1 if FAILS else 0)
