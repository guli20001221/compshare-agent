# Production design — consent-gated in-instance SSH ops/diagnosis

**Status:** design proposal (grounded against `F:\compshare-agent` + upstream `uhost-compshare-api-master`, adversarially verified).
**Supersedes the deferral note in memory `project_ssh_diagnostics_deferred.md`** — the spike answered feasibility; this is the build plan.

---

## 0. Fixed premise + the real decisions

**FIXED PREMISE (not a decision to be optimized away):** production uses the **Claude Agent SDK harness** — Claude Code's agentic loop, tool orchestration, and permission model — as a **sub-agent**, driven by a **third-party model** (ds-v4-flash now, pro later). This is the *goal*. Harness ≠ model: we pay $0 to Anthropic (no Claude model is called; the SDK drives the system `claude` CLI through a local claude-code-router gateway to ds-v4-flash @ ModelVerse). The harness's token overhead (~30k input/turn, measured `README.md:99-102`) and its extra processes are **costs to manage**, not reasons to abandon the harness.

With the premise fixed, the production decisions are:

| Question | Decision |
|---|---|
| How the Go server invokes the harness | **Spawn the harness sub-agent per consented ops-task** (not a long-lived sidecar) → credential lifetime = task lifetime; per-task isolation. The ccr gateway is the only shared long-lived piece (stateless translator, holds the ModelVerse key only). |
| How the credential crosses Go→harness | `ssh_exec` MCP tool takes **`instance_id`, not a credential**; its handler resolves host/user/password inside the tool and returns only scrubbed output. The decoded password crosses via a **one-shot stdin handshake** at spawn into a **Python module variable (NEVER `os.environ`)** — never env, never argv, never a file. *(SDK-confirmed: `query()` does not read the host process's stdin; `create_sdk_mcp_server` tools run in-process as closures.)* |
| Where the credential is fetched | Go server reads it out-of-band off `ExternalExecutor.Execute` **RawResult**, `base64.StdEncoding.DecodeString`, before any redaction view — every redactor is key-name based, so never-serialize is the only sound design (§2). |
| Guardrails | The spike's `guardrails.py` is in the **right place** (the tool handler the harness calls) but **leaks** — it must be rebuilt path-aware before any auto-run ships (§4). This is the real work, and it survives regardless of harness vs anything. |
| Built-in tools | `disallowed_tools=[Bash,…]` + `setting_sources=[]` are **load-bearing safety**, re-asserted every spawn + test-asserted "only `ssh_exec` exposed" (§5). |
| Auth | Phase 1 password-auth (best-effort, stale-prone). Key-auth is the clean long-term fix but needs a provisioning workstream. |
| Rollout | **Dedicated default-off boot flag `COMPSHARE_SSH_OPS`** (NOT the mutating flag — that ships `=1` in the deploy template). Phase 1 = read-only tier only. Phase 2 adds the mutating-confirm tier after scoped consent + a real confirm channel exist. |

---

## 1. Architecture

```
  user turn (NL: "我的实例 X 跑不动了，进去看看")
     │
     ▼  Go HTTP server (internal/engine, internal/httpapi)  — owns identity + consent + credential fetch + audit
  consent gate (§3): scoped grant {instance, TTL, mode}?  ── Phase 1: read-only auto / mutating refused
     │  yes → for this consented task only:
     ├─ fetchSSHCredential(ctx, instanceId)  ── OUT-OF-BAND, never LLM-visible  (§2)
     │     └─ ExternalExecutor.Execute("DescribeCompShareInstance") → RawResult.Password (base64) → decode
     │
     ▼  spawn Claude Agent SDK harness SUB-AGENT (per task; dies → credential gone)
  agent.py wrapper: read {host,user,port,password} from stdin handshake → module local (never env/argv)
  ClaudeAgentOptions(disallowed_tools=[Bash,…], setting_sources=[], allowed_tools=[mcp__ssh_ops__ssh_exec])
     │  Anthropic /v1/messages → ccr gateway (:3456, stateless) → ds-v4-flash @ ModelVerse
     ▼  the model plans; the harness calls the ONE guarded tool:
  ssh_exec(instance_id, command)   ── handler:
     ├─ classify(command)  ── reasoning-blind, command-string only  (§4)
     ├─ resolve creds from the stdin-loaded local (NOT from the model)  → paramiko ssh.connect
     └─ output: cap(16000) + value-scrub (incl. literal credential) → tool result  (§4.3)
     │
     ▼  harness final verdict + AUDIT list
  Go server: scrub → SSE stream / messages DB ; AUDIT → durable ssh_ops_audit (§5)
```

The model passes `instance_id` (it was authorized for that instance via consent) but never sees the credential.

---

## 2. Credential lane (the hard invariant)

**Invariant:** the raw credential (base64 form, decoded plaintext, internal-call `UHostPassWord`) reaches **none** of: LLM prompt/context, trace JSONL / `agent_traces`, `messages` DB, SSE reply, process argv/env, the harness transcript, or any log.

### Why redaction can't be the primary defense
Every redactor in the repo is **key-name based** (`secret_boundary.go:94-120 isSecretKey`; `sanitizer`). A base64/plaintext password as a **free-text value** matches none of the in-string regexes (`secret_boundary.go:161-168` only catch Bearer / `token=` / `UCloud-CompShare-`). **So the only sound design is one where the plaintext never enters any serialized map/struct/log/transcript at all.**

### Mechanism (harness variant)
- **Go fetches off RawResult, not the LLM/Trace views.** `fetchSSHCredential` calls `ExternalExecutor.Execute(ctx, "DescribeCompShareInstance", {UHostId})` and reads `UHostSet[i].Password` + `SshLoginCommand` directly off the returned `map[string]any` (`external.go:181-193`), **before** any `SafeToolResult` view. It is a *separate trusted consumer*, like the snapshot parser / fact writer (`safe_executor.go:168,220`). The normal LLM-callable Describe path is untouched and still strips both fields via `isSecretKey` (`secret_boundary.go:96,119`, recursion verified `:39-92`).
- **Cross the boundary by stdin handshake, into a module variable — not env/argv/file.** Go spawns the harness and writes `{host,user,port,password}` (decoded) as the first stdin frame; the `agent.py` wrapper reads it once into a **plain Python module/closure variable** before starting the SDK query, never echoes it. *(SDK-verified: `query()` communicates with the spawned `claude` CLI over pipes it creates; the wrapper's own stdin is free.)* → credential lives only in the spawned process heap, dies with the task. Avoids `/proc/<pid>/environ` (same-uid readable) and world-visible argv.
  - **⚠️ NEVER `os.environ`.** The SDK passes the wrapper's **full `os.environ` into the spawned `claude` CLI** (`process_env = {**os.environ, **options.env, …}`, issue #573). So a credential in `os.environ` leaks into the CLI subprocess env. The spike's `load_env`→`os.environ.setdefault` (`agent.py:37`) + `ssh_tool.py` `os.environ.get("SSH_PASSWORD")` path is **spike-only** and must NOT be ported — use the module variable. `ClaudeAgentOptions.env` is for external-MCP tokens and is **also unsafe** for the secret (same subprocess-env exposure).
  - **Scrub the spawn environment.** Go must spawn the wrapper with a **minimal env** (only `ANTHROPIC_BASE_URL`, dummy `ANTHROPIC_API_KEY`, `NO_PROXY`, `PATH`) so the prod server's own `AK/SK`, `MYSQL_DSN`, `LLM_API_KEY` don't bleed into the `claude` CLI subprocess via the full-env inheritance above.
- **The model never carries it.** `ssh_exec` declares `(instance_id, command)` in its schema — **no** credential/host param. The handler (an in-process closure) resolves the connection from the module variable. The model can *trigger* SSH to its consented instance but never sees the secret.
- **Lifetime = the task.** No cross-command credential cache (per-command re-resolve from the local; mid-task `reset-password` is then picked up if the lane ever re-fetches). Never written as a `ToolFact` — the fact cache (`engine.go:3592-3608`) is a closed allowlist `{name,state,gpu,gpu_type,cpu,memory,zone}`; a credential fact would persist to `sessions.context` **and** re-inject into the next-turn prompt.
- **Type-enforce on the Go side** (verifier ask): wrap the decoded credential in a non-serializable type whose `String()/MarshalJSON()` returns `[REDACTED]` so an accidental Go-side serialize cannot leak it before the stdin write.

### Do NOT touch `HandleResourceInfo`
The existing "describe my instance" direct-dispatch (`handler.go:191`) hard-drops the creds — its `InstanceSnapshot` (`snapshot.go:11-27`) has no Password/SshLoginCommand fields. Build the SSH lane as a **separate** route.

### Host/user/port parsing (parse `SshLoginCommand`; it unifies both backends)
| Backend | `SshLoginCommand` | user | port | host |
|---|---|---|---|---|
| ucloud VM | `ssh ubuntu@<ip>` | ubuntu | 22 | non-Private `IPSet[].IP` |
| ucloud container | `ssh -p 23 root@<ip>` | root | 23 | public IP |
| pod / k3s | `ssh -p <ExternalPort> root@<podId>.podtcp.compshare.cn` | root | **dynamic** NAT | **DNS, NOT an IP** |
| Windows | *(empty)* | — | — | **refuse SSH** |

Evidence: `pkg/api/describe_compshare_instance.go:101-103`; ucloud `:1275-1282`; pod `:984-997`. Don't hardcode `root@22`; don't reconstruct pod host from IPSet.

### `Password` = base64 of *plaintext* — must decode
`describe_compshare_instance.go:90` "base64(明文) 直接透传"; decrypt `compshare_instance_pwd.go:73-99`. ⚠️ `DescribeAgentInstance` is a *different* Action returning Password already **decoded** — the prod lane uses `DescribeCompShareInstance` (base64-encoded), so the decode step is required.

### Staleness
Stored password is **last-known-set** — written only on create/reset/reinstall, **no OS→Mongo sync**; read path doesn't filter `Removed==1`. In-guest `passwd` → auth fails. **Handling:** fail-fast, single attempt, no retry-loop; surface a credential-free message ("密码认证失败，实例存储密码可能已过期；可重置实例密码以重新同步，或使用 SSH 密钥"). Error carries only `{instanceId, errorClass:auth_failed, latency}`. **Long-term:** key-auth (`paramiko` key / upstream `ssh.PublicKeys`) sidesteps staleness; needs a key-provisioning workstream.

---

## 3. Consent model — Go server owns it; it gates whether to spawn the harness

Today's consent is one boolean `ConfirmFunc(action, args) bool` (`engine.go:128`) with **no scope/instance/TTL/mode**. HTTP `denyConfirm` (`rehydrate.go:15`) **rejects all mutating** — no operator-confirm channel over HTTP. So:

- **Phase 1 (HTTP): read-only tier only.** Mutating/destructive hard-refused. The consent gate just decides whether to spawn the harness for instance X at all (boot flag + recommended per-turn client `Features:["ssh_ops_v1"]` opt-in).
- **Phase 2: scoped `ConsentGrant`** = `{owner:(top_org,org,project), instanceId, expiresAt, mode:read_only|allow_confirmed_mutations, grantId}`:
  - **Scope = single instance**, no wildcard. The Go server resolves the SSH target — and which credential to fetch — **from the grant**, not from an LLM arg → the model cannot redirect SSH to a non-consented box.
  - **Captured via `ConfirmEditsFunc`/`ConfirmForm`** (`workflow/types.go:135-221`): select-only form `{InstanceId display-only, Duration select, Mode select}`, returned as `ConfirmResolution.Overrides`, tamper-validated by `ValidateOverrides` (`:190-208`).
  - **Owner-bound, per-session state** (keyed by `UserFrom(ctx)`, `external.go:111-121`), recorded at broker `Resolve` (`confirm_broker.go`, `ErrConfirmationOwner` precedent), **never** on shared `SharedDeps.ExternalExecutor`. Zeroed on eviction/rehydrate.
  - **Every harness spawn re-checks** the grant before fetching the credential: `target==grant.instanceId` ∧ `now<expiresAt` ∧ `ctx.owner==grant.owner` (the 60s/120s frame timeout `handlers_chat.go:87-92` is a reply-wait, not a multi-command grant — genuinely new state).
  - **Per-command confirm** for the mutating tier rides the same `ConfirmEditsFunc`, showing the literal command (re-classified each round) so the user re-affirms against current box state.
  - **Fail-closed:** CLI (no Form) and non-opted-in HTTP → refuse, never an unscoped boolean yes (`safe_executor.go:202`, `orchestrator/hitl.go:65`).

The credential is **never** in any confirm-card arg (`sanitizeConfirmArgs` `handlers_chat.go:601-603` is a name *denylist*, not an allowlist; args become the user-visible Summary + the trace).

---

## 4. ⚠️ The spike guardrails LEAK — rebuild before any auto-run (survives harness/Go alike)

The credential *fetch* is sound. But the adversarial pass (reproduced against the actual `guardrails.py` with python3) proved the **classifier + output redactor leak secrets**. The reasoning-blind *decision* is correct (box output can't upgrade a command's tier), but **the allowlist/denylist/redactor it gates on are broken**. These live in the harness's tool handler — fix them there.

### 4.1 `read_only` allowlist auto-runs secret exfil — **CRITICAL**
Auto-runs (no confirm): `cat /root/.ssh/id_rsa`, `cat /var/run/secrets/.../token`, `cat /proc/self/environ`, `env`, `printenv`, `systemctl show vllm` (dumps `Environment=`), `cat ~/.aws/credentials`, `cat /home/*/.bash_history` (`guardrails.py:24,26,32,33`). The classifier blocks tier-escalation, not read-only exfil — an XPIA from box output need only **propose** an allowlisted exfil command.
**Fix:** read_only = **path-and-binary allowlist**. Reject `cat/head/tail/stat/ls/du/lsof` whose target is under `/root /home /etc/ssl/private /proc/*/environ /var/run/secrets ~/.ssh ~/.aws /etc/kubernetes`, `*.key/*.pem/*.bash_history`. Reject bare `env`/`printenv`, `systemctl show/cat` outright. **Better:** drop free-form file readers from auto-run; keep only structured diagnostics (`nvidia-smi -q`, `df -h`, `free -h`, `uptime`, `ps`, `ss`, `lscpu`).

### 4.2 Metachar denylist misses `$VAR` and glob — **HIGH**
`_DANGEROUS_META` (`guardrails.py:54`) blocks `$(...)` but not `$VAR`, `${VAR}`, `*`/`?`. Auto-runs `cat $SECRET_FILE`, `cat /home/*/.bash_history`.
**Fix:** add `$ { } * ? [ ~` to the auto-run disqualifier set.

### 4.3 Output redactor leaks every real secret shape — **CRITICAL** (and the SSE stream is unscrubbed)
`redact()` (`guardrails.py:76-91`) returns **unchanged**: `LLM_API_KEY=sk-proj-abc123` (rule breaks on `-` in `proj`), `MYSQL_DSN=user:hunter2@tcp(...)`, `AWS_SECRET_ACCESS_KEY=…`, `HF_TOKEN=hf_…`, `DATABASE_URL=…:p4ss@…`. The Go SSE token stream emits raw (`handlers_chat.go:425-431`); done-frame Content is raw reply (`:513-514`). So `env` (auto-run, §4.1) → secrets streamed verbatim. **Also how the SSH credential re-enters**: injected into a live shell, any command that prints it returns it in stdout.
**Fix:** (a) primary — remove secret-dumping commands from auto-run (§4.1); (b) defense — the tool handler must, before returning, literal-`Replace` the just-used decoded password **and its base64 form** with `[REDACTED]`, plus suffix-match `WORD=VALUE`/`WORD: VALUE` where WORD ∈ `/(KEY|TOKEN|SECRET|PASS|PWD|DSN|CRED|URL)$/i`, plus high-entropy ≥20-char tokens, plus PEM blocks. And the Go side must scrub harness output before SSE/DB too (don't rely on the key-name `RedactOutputLeak`).

### 4.4 Destructive denylist case-sensitive + misses brick/wipe — **HIGH**
`RM -rf /` → `mutating` (no `re.IGNORECASE`, `guardrails.py:42-52`). Also `mutating`: `find /data -delete`, `cp /dev/null /boot/vmlinuz`, `sed -i … /etc/fstab`, `ln -sf /dev/null /etc/resolv.conf`.
**Fix:** compile destructive `re.IGNORECASE`; add `find … -delete/-exec`, `ln -s … (/etc/|/boot/)`, `cp/mv/tee/sed -i/>` targeting `/boot //etc/fstab //etc/resolv.conf //etc/sudoers.d`. **Deny-by-effect, not deny-by-binary-name.**

### 4.5 Follow/stream flags hang the channel — **MEDIUM**
`tail -f`, `journalctl -f` classify read_only and never return. **Fix:** disqualify `-f/--follow/-F/watch`; the spike's `_TIMEOUT_S=30` per-command cap must be hard, non-optional.

### 4.6 The shell-string classifier is a *weaker* boundary than `safe_executor` — name it honestly
`safe_executor.go:183-188` tiers a **closed, curated** action-name set; the shell classifier tiers an **open, adversary-chosen** string. Openness is exactly where §4.1-4.5 come from. **Mandatory:** deny-by-default, path-aware+expansion-free read_only, and an **independent adversarial corpus** (exfil/recon/brick/obfuscation) as a **binary CI gate** before the lane is enabled.

---

## 5. Operational gaps (harness-specific) — release-blocking for Phase 1

| Gap | Note | Fix |
|---|---|---|
| **Built-in tools must be stripped, every spawn.** Harness ships `Bash` that runs on the **local control-plane host** — on first spike run it diagnosed the operator's laptop, 0 commands hit SSH guardrails (`README.md:90-97`, `agent.py:122-137`). | the harness's #1 failure mode | A wrapper that ALWAYS sets `disallowed_tools=[Bash,BashOutput,KillShell,Read,Write,Edit,…]` + `setting_sources=[]`, with a **boot test that asserts the only exposed tool is `mcp__ssh_ops__ssh_exec`**. Treat any other exposed tool as a hard failure. |
| **Scrub the spawn environment.** SDK passes the wrapper's **full `os.environ` into the `claude` CLI** (`{**os.environ, …}`, issue #573). | prod server's `AK/SK`/`MYSQL_DSN`/`LLM_API_KEY` would bleed into the CLI subprocess; a cred in env would too | Go spawns the wrapper with a **minimal env** (`ANTHROPIC_BASE_URL`, dummy `ANTHROPIC_API_KEY`, `NO_PROXY`, `PATH`). Credential → module variable from the stdin handshake, never `os.environ`, never `options.env`. |
| **Spawn lifecycle = credential lifecycle.** Per-task spawn → process exit drops the heap-resident credential. | the security property we want | Go supervises the child: bounded wall-clock, kill on completion. Do NOT port the spike's module-global singleton `_client` (`ssh_tool.py:15`) across tasks — fresh paramiko connect per task, close on exit. |
| **Abort = kill the subprocess.** `streamCtx` cancels on disconnect (`handlers_chat.go:337-340`) but nothing kills a running harness. | hung task can't be stopped | On `ctx.Done()` the Go supervisor SIGKILLs the harness child (and paramiko dies with it). Per-command `_TIMEOUT_S` inside the tool bounds a single hung command. The default-off boot flag is the global kill-switch. |
| **ccr gateway is a shared dependency.** Stateless translator, holds the ModelVerse key (same as today's LLM path). | one extra long-lived process | Supervise it like any sidecar; it carries no per-tenant state and never sees the SSH credential (that's paramiko↔instance, not gateway↔model). |
| **No SSH/egress rate-limit class.** `ratelimit.go:24-35` has only LLM/Mutating/ReadExpensive/UserTurn. | runaway/XPIA loop floods a tenant box | Add `ClassSSHExec` (QPS + daily, keyed by `(top_org,org)`) + a per-task command cap + per-`(owner,instance)` daily ceiling. |
| **Audit bridge.** The harness already builds an `AUDIT` list (command→tier→disposition, `agent.py:46,84-85`). | best-effort trace ≠ accountability | Bridge `AUDIT` into a dedicated `ssh_ops_audit` table, **synchronous insert before dispatch, fail-closed** (refuse if the audit write errors). Output excerpt best-effort; the *what-ran* record must not drop. Specify operator read path. |
| **Deploy-template flag trap.** Deploy template ships `COMPSHARE_ENABLE_MUTATING_TOOLS=1` (`.env.example`+`invite.sh`). | gating on the mutating flag = default-on in prod | **dedicated `COMPSHARE_SSH_OPS`**, default-off, NOT in `.env.example`/`invite.sh`. (Memory: don't touch those without coordination.) |
| **Concurrency (Phase 2).** Two sessions, same owner→same box, concurrent mutations (`pool.go:44-47` entryKey is owner+session). | cross-session race | per-`(owner,instance)` advisory lock; serialize mutating per instance. Read-only Phase 1 makes this Phase-2-blocking only. |
| **Pod egress.** Pod host = wildcard DNS `*.podtcp.compshare.cn`, dynamic NAT ports. | IP allowlist can't cover it | deploy prereq: server reaches ucloud public IPs **and** `*.podtcp.compshare.cn:<dyn>`. Dial/DNS failure → `connect_failed` (distinct from `auth_failed`), single attempt, credential-free. |

---

## 6. Invariants (binary, CI-gated)

- **INV-1** Raw credential never enters any LLM message or the harness transcript (arg, result, confirm-card arg, prompt, fact, history).
- **INV-2** Never reaches any persistence/display sink (trace JSONL, `agent_traces`, `messages.content`, SSE token/done frames, logs, `sessions.context`).
- **INV-3** Never in process argv or environment — Go→harness handoff is **stdin-only** into a module variable; the credential is never placed in `os.environ` (the SDK inherits the wrapper's full env into the `claude` CLI), and the wrapper is spawned with a minimal scrubbed env.
- **INV-4** Decoded plaintext lives only in (a) the Go fetch call-stack until the stdin write, wrapped in a non-serializable type, and (b) the spawned harness process heap; never a cached/shared struct field; discarded when the task process exits.
- **INV-5** Every command classified reasoning-blind from the literal string alone; **destructive unconditionally hard-refused before consent** — no flag/origin/grant/confirm bypass (mirror `safe_executor.go:183-185`).
- **INV-6** Tiering fails closed: anything not positively matching the read_only allowlist is ≥mutating, never auto-run.
- **INV-7** Every harness spawn re-checks the live grant (target==grant.instanceId from the grant; `now<expiresAt`; `ctx.owner==grant.owner`).
- **INV-8** Grant lives only in per-session state keyed by owner; never on shared structs; zeroed on eviction/rehydrate.
- **INV-9** The harness exposes **exactly one** tool (`ssh_exec`); any other exposed/built-in tool is a hard boot failure.
- **INV-10** Nil/unavailable consent gate declines (CLI + non-opted-in HTTP → refuse, never unscoped-yes).
- **INV-11** Auth failure fails fast (no retry); error carries only `{instanceId, errorClass, latency}`.
- **INV-12** Whole lane gated by a boot-only default-off flag; off → no spawn.
- **INV-13** Every executed command **and** every refusal → exactly one durable audit record; never the credential, never raw uncapped output.

---

## 7. Phasing

**Phase 1 (shippable; read-only avoids the HITL gap):**
- Go server: consent gate (flag + opt-in), out-of-band credential fetch (§2), spawn-per-task supervisor (stdin handshake, kill-on-abort, bounded), `ssh_ops_audit` (synchronous, fail-closed), `ClassSSHExec` rate limit, dedicated default-off flag.
- Harness wrapper: **always-strip built-in tools** (+ INV-9 boot test), `ssh_exec(instance_id, command)` taking creds from the stdin local, **rebuilt path-aware classifier** §4.1-4.2, destructive hard-refuse §4.4, no-follow §4.5, output scrub §4.3.
- **Binary CI gates:** the classifier adversarial corpus (port `test_guardrails.py` + the exfil/brick/obfuscation cases) in CI; a test that `DescribeCompShareInstance` Password/SshLoginCommand never survive into LLMResult/TraceResult and are read only off RawResult; a test that the Go credential wrapper marshals to `[REDACTED]`; the INV-9 single-tool boot test.

**Phase 2 (separate, gated PR):**
- Scoped `ConsentGrant` state §3 + per-turn confirm frame (reuse `ConfirmEditsFunc`/`ConfirmForm`).
- Mutating-needs-confirm tier (destructive still hard-refused).
- Per-`(owner,instance)` concurrency lock.

**Parallel workstream:** SSH key provisioning → switch transport to key-auth, retire password-auth staleness.

---

## 8. Standing risks (accepted / flagged)
- `InsecureIgnoreHostKey`/`AutoAddPolicy` (spike `ssh_tool.py:27`, upstream `run_agent_command.go:138`) permits MITM → an impersonating host **receives the plaintext password** during auth. Accepted for ephemeral IP/DNS-reached boxes; gate behind egress allowlist + dial timeout; TODO known-hosts pinning (capture key at create) before any untrusted route.
- Harness path's costs (managed, not blockers): ~30k input tok/turn × ReAct rounds; 3 long-lived/spawned processes (claude CLI + ccr gateway + per-task harness); the credential crosses one process boundary (stdin) — kept off env/argv/disk.
- Phase 1 is password-auth-only with documented staleness UX; quantify the in-guest-`passwd` rate before committing to value.

---

*Grounded by workflow `ssh-ops-credential-lane-design` (12 agents, 6 subsystem maps + 3 design + 3 adversarial passes, all file:line-cited). Rotate the chat-shared spike SSH password.*
