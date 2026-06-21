# Spike: Claude Agent SDK harness + third-party model (ds-v4-flash) for in-instance SSH ops/diagnosis

**Branch:** `spike/agent-sdk-ssh-ops` · exploration only, not wired into the Go binary.

## What this proves

Whether we can use the **Claude Agent SDK *harness*** — its agent loop, tool
orchestration, permission model — while driving it with our **own third-party model
(`ds-v4-flash`)** instead of a Claude model, to SSH into a user's GPU instance and run
**consented** ops/diagnosis behind real guardrails.

Key idea (from the design discussion): *harness* and *model* are two orthogonal levers.
Using Claude Code's harness does **not** require Claude the model — we pay $0 to Anthropic
and keep our flash token costs.

## Architecture

```
  task (NL)
     │
     ▼
  claude-agent-sdk  (agent.py)            ← the HARNESS (Anthropic, MIT wrapper). NOT Anthropic-billed.
     │  Anthropic /v1/messages
     ▼
  claude-code-router  (localhost:3456)    ← gateway: Anthropic ⇄ OpenAI translation
     │  OpenAI /v1/chat/completions
     ▼
  ds-v4-flash @ api.modelverse.cn         ← the MODEL (third-party, our existing key)
     ▲
     │  the model decides WHAT to run; the harness calls our tool:
  ssh_exec  (ssh_tool.py, paramiko)       ← guardrails in guardrails.py
     │  SSH (password/key, creds from gitignored .env.local)
     ▼
  ubuntu@<instance>  (RTX 4090 box)
```

## Guardrails (reasoning-blind — `guardrails.py`)

The run/refuse/confirm decision uses **only the user intent + the literal command
string**, never anything the box emitted (XPIA firewall). Three tiers mirror
`internal/tools/safe_executor.go`:

| tier | examples | action |
|---|---|---|
| `read_only`  | `nvidia-smi -q`, `df -h /`, `journalctl -u ...`, `systemctl status` | auto-run (T1 allowlist) |
| `mutating`   | `systemctl restart`, `pip install`, anything with `|`/`>`/`;`, unknown | **not executed** — surfaced as an optional fix needing operator confirm (T3) |
| `destructive`| `rm -rf`, `dd`, `mkfs`, `reboot`, `chmod -R`, `ufw disable` | **hard-refused**, checked first (L2) |

Allowlist (not denylist): only known-safe `command + arg shape` auto-runs; everything else
is deny-first. Box output is capped + secret-redacted before it reaches the model or a log.

## Files

| file | role |
|---|---|
| `guardrails.py` | reasoning-blind classifier (`classify`) + output `redact` |
| `ssh_tool.py`   | paramiko SSH executor; output cap + redaction; creds from env |
| `agent.py`      | Agent SDK wiring: `ssh_exec` MCP tool, system prompt, audit, token metrics |
| `test_guardrails.py` | offline proof the tiers classify correctly (21 cases) |
| `.env.example`  | template; copy to `.env.local` (gitignored) and fill |

## Run

```powershell
# 1) deps (one-time): claude-agent-sdk (pip), claude-code-router (npm), paramiko
# 2) config: copy .env.example -> .env.local, fill SSH creds + LLM_API_KEY
# 3) gateway: ccr restart   (config at ~/.claude-code-router/config.json -> ModelVerse)
# 4) run:
python test_guardrails.py        # offline guardrail proof
python agent.py                  # GPU health-check diagnosis (read-only)
python agent.py "<your task>"
```

## Validation status — ALL GREEN (2026-06-21)

- [x] gateway → ds-v4-flash: Anthropic `/v1/messages` translated, flash replies, usage returned
- [x] SSH into the box: `ubuntu@117.50.185.80` (RTX 4090, Ubuntu 5.15) read-only probes OK
- [x] guardrails: 21/21 classification cases pass; redaction works
- [x] **end-to-end**: Agent SDK harness, driven by **ds-v4-flash**, SSHed into the box and produced
      a correct GPU health verdict (RTX 4090 / driver 570.153.02 / 24 GB / 58 GB disk / 62 GiB RAM)
- [x] guardrails enforced live: read-only commands auto-ran; commands with pipes/redirection were
      classified `mutating` and **not executed** (surfaced as needs-confirmation in the AUDIT)

## Findings (the spike's actual answers)

1. **Feasible, $0 to Anthropic.** The Claude Agent SDK *harness* runs on our own `ds-v4-flash`
   through the `claude-code-router` gateway. No Claude model is called → no Anthropic billing.
   SDK installs as a tiny pure-python sdist that drives the existing `claude` CLI.

2. **🔴 Critical safety finding — built-in tools bypass the guardrails.** Out of the box the harness
   exposes a built-in `Bash` tool that runs on the **LOCAL control-plane host**. On the first run the
   model used it and "diagnosed" the operator's laptop, never touching the remote box — **0 commands
   hit the SSH guardrails**. Fix (load-bearing): `disallowed_tools=[Bash, Read, Write, …]` so the
   guarded `ssh_exec` is the agent's ONLY capability. Without this, adopting the harness is unsafe.

3. **Must isolate from host config.** `setting_sources=[]` is required, else the spawned CLI loads
   the operator's `~/.claude` tools/plugins and the in-process `ssh_exec` MCP tool never registers.

4. **💰 Harness is token-heavy (the cost lever, measured).** Even a one-word reply cost **~30,148
   input tokens** of system-prompt/tooling overhead; the full 7-call diagnosis ran ~30 k input /
   ~1.4 k output. On flash's cheap rate this is acceptable, but it is a real multiplier vs a lean
   hand-rolled loop — this is the "harness vs model" cost tradeoff, now with numbers.

5. **flash tool-calling is adequate here.** It issued correctly-formatted multi-step tool calls and
   self-corrected; one prompt tweak removed an initial tool-name fumble. Good enough for bounded,
   gated ops — long unbounded autonomy on flash would need more evaluation.

### Reproduce the headline run
```powershell
ccr status                       # gateway up on :3456
$env:NO_PROXY='127.0.0.1,localhost'
python agent.py                  # see the AUDIT + METRICS block at the end
```

### Install gotcha (this environment)
The published wheels are ~70 MB (they bundle the `claude` binary + pywin32, whose DLL is locked by a
running process → `WinError 5`). Install the pure-python source instead, reusing the system CLI:
`pip install claude-agent-sdk --no-binary claude-agent-sdk --no-deps --index-url https://pypi.org/simple/`
(domestic mirrors 403/hang through the local proxy; official PyPI works through it).

## Security / credential hygiene

- Real SSH password + `LLM_API_KEY` live only in `.env.local` (gitignored). Never committed.
- The instance password was shared in chat for this test — **rotate it after**.
- Mutating commands are never auto-run; destructive are hard-refused regardless.
