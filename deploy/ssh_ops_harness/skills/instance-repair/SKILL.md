---
name: instance-repair
description: Repair playbook for a remote compute instance when the operator has authorized writes. Load it together with instance-triage — triage tells you how to FIND the fault, this tells you how to fix it, how to start a service that survives the SSH session, and how to write a verdict that admits what you actually did. It REPLACES instance-triage section 4.
---

# In-instance repair (write-authorized)

`instance-triage` is the method for finding the fault, and everything in its sections 0-3 still
applies. But that skill is written for a read-only lane: it says you cannot change anything, and its
section 4 tells you to hand the operator commands and never imply you ran them. **In this session
that is wrong.** The operator authorized repair before you were started. Wherever this file and
`instance-triage` disagree, this file wins, and **section 4 of `instance-triage` is replaced by
section 4 of this file.**

You still diagnose first. Authorization to change the box is not permission to guess at it.

## 0. What the executor actually does

Read this before planning a fix — a fix that the executor rejects costs a round trip, and a fix
based on a wrong idea of the executor costs the repair.

- **Read-only commands run immediately.** A command that changes the box also runs, but the operator
  is shown that exact command and must approve it. Expect 1-3 approvals in a repair.
- **If a command is declined, it is declined.** Do not retry it, and do not look for a different
  command that makes the same change — that is working around the human, not around a bug. Report it
  under 未处理.
- **Pipes, globs, and `;` / `&&` chaining are accepted.** So are `2>/dev/null` and `2>&1`.
- **Refused for form, in both modes:** command substitution (`$(...)`, backticks) and multi-line
  scripts. A form refusal is not a permissions problem — resend as plain single-line commands.
- **Refused outright, whatever you do:** deleting data, wiping or partitioning disks, reboot or
  power off, passwords and accounts, disabling ssh or networking. Do not plan around these; when the
  only real fix is one of them, say so and stop.
- **Every call is its own SSH session.** It opens, runs your one command line, and closes. Nothing
  carries over: not a `cd`, not an environment variable, not a background job's terminal.

## 1. Before you change anything

**Justify the change with something you observed.** Name the command whose output proves this fix
addresses this cause. If you cannot, you are not ready to fix it — go back to triage.

**Smallest fix that addresses the cause.** Restart the one dead service, not the box. Fix the one
wrong path, not the whole config.

**Prefer the image's own launcher over your own invocation.** These images start their app through a
supervisor unit or a shipped script (`/start.d/*.sh`, `/entrypoint.sh`, a `start.py`). That launcher
often brings up **more than one thing** — a real run started `main.py` directly, restored the port
it was asked about, and silently left a second UI port dead that the launcher would have started.
If you deliberately bypass the launcher, say so in the verdict and check what else it would have
started.

## 2. Starting something that must outlive your command

This is the single most common way a repair silently fails here, so it has its own section.

Your command runs in an SSH session that **ends when the command returns**. A service still holding
that session's stdout dies on its next write — you will see it in the log as
`BrokenPipeError: [Errno 32] Broken pipe`, seconds after a start that looked successful.

To start a long-lived service, background it **and** send its output to a file:

```
nohup <the image's own launcher> > /path/to/some.log 2>&1 &
```

`nohup` on its own is **not enough**. It only diverts output to `nohup.out` when stdout is a
terminal, and here it is a pipe — so without the explicit `> file 2>&1` the process still writes
into the closing pipe and dies. The redirect is the part that matters; `nohup` (or `setsid`) only
adds protection against the hangup signal.

Then **wait and re-check** — startup is not instant, and a process that exists one second after
launch may already be gone five seconds later:

```
sleep 5
ss -tlnp | grep <port>
tail -30 /path/to/some.log
```

A service is only started when the port is listening **and** the log shows no fatal error after your
start time. `ps` showing a PID is not enough.

## 3. Own what you did

Everything you ran is part of the story you tell. In particular:

- **If a command you ran failed, report it as your own failed attempt** — never as background
  evidence about the machine. Writing "最近一次启动在 17:52 因 BrokenPipeError 退出" about *your own*
  start makes the operator think the service crashed by itself. Write "我在 17:52 尝试启动，失败了，
  原因是……" instead.
- **Never claim a fix you did not verify**, and never describe an unexecuted command as if it ran.
- **Do not upgrade a guess.** `instance-triage` section 0 still governs: a cause you did not prove is
  **推测** or **无法确认**, even when the fix worked. A fix that happens to work does not retroactively
  prove why it broke.

## 4. Writing the verdict — replaces `instance-triage` section 4

Chinese, concise, in this order. Sections 4-6 are what this mode adds; do not fold them into 1-3.

1. **结论** — one line: what was broken and whether it is fixed now.
2. **证据** — the concrete values you observed, each tied to the command that produced it, with the
   timestamp/age of any log you cite.
3. **确证 vs 推测** — visually separate. Anything unproven goes under **无法确认**, never silently
   upgraded, even if the repair succeeded.
4. **已执行的修复** — every command you actually ran that changed the box, verbatim, in order, each
   with its result. **Include the ones that failed**, labelled as your own attempts. If you bypassed
   the image's own launcher, say so here.
5. **验证** — the read-only command(s) proving it works now, with their output. If you could not
   verify, say that plainly; do not present the fix as confirmed.
6. **未处理 / 需要操作员** — commands that were refused or declined, and anything that still needs a
   human. Keep this strictly separate from section 4: a refused command is not a completed repair.

If you changed nothing — because the cause needed a refused command, or because the box is healthy
and the fault is elsewhere — say so in 结论 and leave 已执行的修复 empty. An honest "I did not
change anything, and here is why" is a good outcome. A repair you cannot prove is not.

Missing data is **无法确认**. Never report an unmeasured value as 0% or as healthy.
