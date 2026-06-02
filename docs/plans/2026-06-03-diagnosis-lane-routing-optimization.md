# Diagnosis Lane And Routing Optimization Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task.

**Goal:** Make the already-built read-only routing and diagnosis agent lane actually reachable and proven with real CLI runs, before expanding more tools or doing SKILL.md strict alignment.

**Architecture:** Keep the three runtime forms: `routing` for deterministic read-only dispatch, `terminal_rag` for cited pure knowledge answers, and `agent` for true body-read diagnosis skills plus saga workflows. This plan first fixes reachability (`network_accelerator_status`, symptom-to-diagnosis routing), then proves one diagnosis true-skill loop with live CLI, then expands evidence and only later considers default-on or standardization.

**Tech Stack:** Go, PowerShell, `agent.exe cli -c deploy/conf/agent.yaml`, JSONL traces, `eval/skill`, real CompShare API credentials from local untracked smoke config, project `org-cwy2qk`.

---

## Ground Rules

- Prefer branching in the existing `F:\compshare-agent` checkout instead of a separate worktree, because live smoke credentials and several smoke helpers are local/untracked there. The checkout may contain many untracked artifacts; stage commits by explicit file path only.
- If a separate worktree is required, copy local-only smoke prerequisites into it first and update smoke scripts to use the worktree root rather than hardcoded `F:\compshare-agent` paths.
- Do not commit secrets, live raw transcripts with credentials, or `.smoke_env.ps1`.
- Use local scripts/config to load credentials:
  - `eval/.smoke_env.ps1` contains local smoke credentials and must stay untracked.
  - `deploy/conf/agent.yaml` is the CLI config.
  - `COMPSHARE_PROJECT_ID=org-cwy2qk` is the test project used by prior live smokes.
- Real CLI tests are required wherever this plan says "Live CLI gate".
- If a live test needs an instance ID and no suitable instance exists, create any stocked instance through the agent with mutating tools enabled. Cost is not a blocker for this test account.
- Test-created instances do not need to be deleted afterward. Record their `UHostId` in local smoke notes, prefer reusing one created instance across tests, and keep those IDs out of committed reports unless redacted.
- Destructive/delete-class actions must remain hard-refused even when mutating tools are enabled.
- Do not do broad prompt optimization. Only change planner text/examples for the measured #123 misses.
- Do not expand new tools before already-built routes are reachable.
- Do not flip `USE_SKILL_EXECUTOR` globally until one or more diagnosis true-skill loops are proven.

## Current Verified Facts

- `main` baseline for this plan: `54ec27a`.
- `network_accelerator_status` has route metadata, required tool, and handler, but is not in the default cutover set.
- `network_accelerator_status` live smoke currently shows:
  - planned form: `routing`
  - actual form: empty/fallback
  - cutover: `fallback_ineligible`
  - tool calls: none
- #123 probe data exists at `C:\Users\23843\AppData\Local\Temp\diag123\diag123_report.json`.
- #123 measured miss set:
  - `为什么我开的端口在外面访问不了`: diagnosis 1/5, knowledge_qa 4/5.
  - `跑模型的时候说找不到GPU`: diagnosis 1/5, knowledge_qa 4/5.
  - `ssh连接超时一直进不去`: diagnosis 4/5, knowledge_qa 1/5.
- Control cases are clean:
  - `怎么加速github下载`: knowledge_qa 5/5.
  - `下载模型特别慢怎么办`: knowledge_qa 5/5.
- `diagnose-port-firewall` true-skill loop is implemented and gated:
  - global gate: `USE_SKILL_EXECUTOR`
  - per-skill gate: `USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS`
  - current safe evidence adapter injects `EvidenceLedger`, not raw `KBChunk.Content`.
  - `ValidateNoRawEvidenceLeak` rejects answers that leak raw retrieved content.
- The diagnosis true-skill loop has unit/process tests, but must be proven with real CLI.

---

## Phase 0: Branch-In-Place Baseline

**Purpose:** start from a known main commit while preserving local-only smoke credentials and helpers.

**Files:**
- No repo file changes.

**Step 1: Fetch remotes**

Run:

```powershell
git fetch --all --prune
```

Expected: no errors.

**Step 2: Verify tracked files are clean**

Run:

```powershell
git diff --quiet
git diff --cached --quiet
git rev-parse --short HEAD
git rev-parse --short origin/main
```

Expected:

- both `git diff` commands exit 0.
- local HEAD matches `origin/main`.
- untracked files may exist and are expected.

**Step 3: Create implementation branch in place**

Run:

```powershell
git switch -c codex/diagnosis-routing-optimization
git status --short
```

Expected:

- branch created.
- tracked files remain clean.
- untracked smoke artifacts may still be listed.

**Step 4: Baseline tests**

Run:

```powershell
$env:COMPSHARE_PROJECT_ID="org-cwy2qk"
go test ./cmd ./internal/intent ./internal/engine ./internal/knowledge ./eval/skill -count=1
go vet ./cmd ./internal/intent ./internal/engine ./internal/knowledge ./eval/skill
```

Expected: all green. If `cmd` fails only because `COMPSHARE_PROJECT_ID` is missing, set it and rerun before continuing.

---

## Phase 1A: Make `network_accelerator_status` Reachable

**Purpose:** fix an already-built read-only route that currently cannot dispatch by default.

**Risk:** low. This changes default runtime behavior but does not change planner prompt bytes.

**Files:**
- Modify: `cmd/trace.go`
- Modify: `cmd/trace_test.go`
- Possibly modify: `eval/skill/cases.jsonl` only if the existing case needs stronger assertion text.
- Create if needed: `eval/net_accelerator_smoke.ps1`

**Step 1: Write the failing test**

Update `TestIntentPlannerCutoverIntents_DefaultsWhenEnvUnset` in `cmd/trace_test.go` so the default list includes:

```text
network_accelerator_status
```

Add a test case to `TestIntentPlannerCutoverIntentsFromEnv` so explicit env aliases include one of:

```text
network_accelerator
network_accelerator_status
net_accelerator
```

The canonical intent value must be `network_accelerator_status`.

Run:

```powershell
go test ./cmd -run "IntentPlannerCutoverIntents" -count=1
```

Expected: fail because the default and aliases are not implemented yet.

**Step 2: Implement the default cutover fix**

In `cmd/trace.go`, add `intent.IntentNetAcceleratorStatus` to `defaultCutoverIntents()` near the other read-only routing intents.

Also add explicit env parsing aliases in `intentPlannerCutoverIntentsFromEnv`.

Expected default runtime line after this phase:

```text
planner_mode=dispatch cutover_intents=[resource,monitor,gpu_specs,stock,pricing_query,platform_image,custom_image,community_image,network_accelerator_status]
```

Exact order can follow the existing default list convention; update the test to pin the chosen order.

**Step 3: Unit tests**

Run:

```powershell
go test ./cmd ./internal/intent ./internal/engine ./eval/skill -count=1
```

Expected: all green.

**Step 4: Prompt byte-stability gate**

Run:

```powershell
go test ./internal/intent -run "Prompt|systemPrompt|RoutingPrompt|SkillRegistry" -count=1
```

Expected: all green and no `systemPromptSHA256Baseline` update needed.

If the prompt hash changes, stop. Phase 1A should not modify the planner prompt.

**Step 5: Build CLI**

Run:

```powershell
go build -o agent.exe ./cmd
```

Expected: `agent.exe` exists.

**Step 6: Live CLI gate**

Use real credentials from local smoke config. Do not print or commit the secret values.

Run:

```powershell
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8
. .\eval\.smoke_env.ps1
$env:COMPSHARE_PROJECT_ID="org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS=""
$env:COMPSHARE_TRACE_ENABLED="1"
$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$env:COMPSHARE_TRACE_DIR = Join-Path $env:TEMP "compshare-net-accelerator-$runId"
New-Item -ItemType Directory -Force $env:COMPSHARE_TRACE_DIR | Out-Null
"网络加速现在是什么状态`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml 2>&1 | Tee-Object -FilePath "$env:COMPSHARE_TRACE_DIR\transcript.txt"
```

Inspect the latest trace:

```powershell
Get-ChildItem $env:COMPSHARE_TRACE_DIR -Filter "*.jsonl" -Recurse |
  Sort-Object LastWriteTime -Descending |
  Select-Object -First 1 |
  ForEach-Object { Get-Content $_.FullName -Raw -Encoding UTF8 }
```

Expected:

- planner intent: `network_accelerator_status`
- planned runtime form: `routing`
- actual runtime form: `routing`
- cutover: `dispatched`
- tool call: `CheckCompShareNetOptimizer`
- no mutating tool calls
- user answer must not imply the agent can enable or modify network acceleration.

**Step 7: Commit Phase 1A**

Run:

```powershell
git add cmd/trace.go cmd/trace_test.go
git commit -m "fix(routing): enable network accelerator status by default"
```

If a smoke script/report is added and contains no secrets, include it in the commit. Do not commit raw environment files.

---

## Phase 1B: Fix #123 Diagnosis Recall Without Polluting How-To

**Purpose:** route symptom reports to `diagnosis` even when the user did not provide an instance ID, while keeping pure tutorials and configuration questions in `terminal_rag`.

**Risk:** medium. This changes planner prompt bytes and must be eval-gated.

**Files:**
- Modify: `internal/intent/planner.go`
- Modify: `internal/intent/planner_examples/diagnosis.md`
- Modify: `internal/intent/planner_examples_test.go`
- Create: `eval/diagnosis_routing_jitter_questions.json`
- Create: `eval/diagnosis_routing_jitter.ps1` or reuse/adapt `eval/planner_jitter.ps1`
- Optional: `eval/diagnosis_routing_jitter_report.md`

**Step 1: Record baseline from existing probe**

Copy the measured baseline into the new report or PR description:

```text
ssh_vague: diagnosis 5/5
ssh_timeout: diagnosis 4/5, knowledge_qa 1/5
port_external: diagnosis 5/5
port_why: diagnosis 1/5, knowledge_qa 4/5
gpu_notfound: diagnosis 1/5, knowledge_qa 4/5
init_stuck: diagnosis 5/5
slow_download: knowledge_qa 5/5
tutorial_github: knowledge_qa 5/5
```

Do not treat this old probe as final validation after code changes. It is only baseline.

**Step 2: Add eval questions**

Create `eval/diagnosis_routing_jitter_questions.json` with at least:

```json
[
  {"id":"port_why","expect":"diagnosis","question":"为什么我开的端口在外面访问不了"},
  {"id":"gpu_notfound","expect":"diagnosis","question":"跑模型的时候说找不到GPU"},
  {"id":"ssh_timeout","expect":"diagnosis","question":"ssh连接超时一直进不去"},
  {"id":"port_external","expect":"diagnosis","question":"我部署的服务外网打不开"},
  {"id":"init_stuck","expect":"diagnosis","question":"实例一直卡在初始化进不去"},
  {"id":"tutorial_github","expect":"knowledge_qa","question":"怎么加速github下载"},
  {"id":"slow_download","expect":"knowledge_qa","question":"下载模型特别慢怎么办"},
  {"id":"platform_howto","expect":"knowledge_qa","question":"怎么配置远程桌面音频"},
  {"id":"error_code_howto","expect":"knowledge_qa","question":"错误码226601是什么意思"}
]
```

**Step 3: Build/reuse jitter runner**

Use the same pattern as `eval/planner_jitter.ps1`:

- UTF-8 stdin/stdout.
- `Runs=5` minimum per question.
- `COMPSHARE_ENABLE_MUTATING_TOOLS=""`.
- trace enabled.
- project set to `org-cwy2qk`.
- parse latest JSONL trace for:
  - planner intent
  - planned runtime form
  - cutover status
  - retrieval/tool calls

Runner should output a compact JSON report:

```json
{
  "question_id": "port_why",
  "runs": 5,
  "diagnosis": 5,
  "knowledge_qa": 0,
  "pass": true
}
```

**Step 4: Run baseline on current code in branch before prompt edits**

Run:

```powershell
go build -o agent.exe ./cmd
powershell -ExecutionPolicy Bypass -File .\eval\diagnosis_routing_jitter.ps1 -Runs 5 -Tag baseline
```

Expected:

- reproduces old miss pattern for `port_why` and `gpu_notfound`.
- confirms the runner itself catches failures.

**Step 5: Write minimal planner change**

In `internal/intent/planner.go`, change the diagnosis-vs-knowledge boundary from target-first to shape-first:

- runtime symptom / broken-state / failure report -> `diagnosis`, even when `target_refs` is empty.
- pure how-to / configuration / error-code explanation -> `knowledge_qa`.
- the engine already asks "which instance?" for diagnosis without a target, so no target is safe.

Do not touch unrelated prompt sections.

**Step 6: Add diagnosis examples**

In `internal/intent/planner_examples/diagnosis.md`, add 2-3 no-target symptom examples:

- "为什么我开的端口在外面访问不了"
- "跑模型的时候说找不到GPU"
- "ssh连接超时一直进不去"

Each example must use:

```json
"intent":"diagnosis",
"target_refs":[]
```

Do not add broad examples that would capture tutorials like "怎么加速 GitHub 下载".

**Step 7: Update prompt hash with explicit justification**

Run:

```powershell
go test ./internal/intent -run "Prompt|systemPrompt|Planner" -count=1
```

Expected: fail only because `systemPromptSHA256Baseline` changed.

Update `internal/intent/planner_examples_test.go` `systemPromptSHA256Baseline` to the new hash. Add or update the in-file changelog/comment if the file has one.

Commit message must mention why the hash changed:

```text
planner prompt rebaseline: no-target symptom reports route to diagnosis; how-to controls remain knowledge_qa
```

**Step 8: Unit and offline tests**

Run:

```powershell
go test ./internal/intent ./internal/engine ./eval/skill -count=1
go vet ./internal/intent ./internal/engine ./eval/skill
```

Expected: all green.

**Step 9: Live CLI gate**

Run:

```powershell
powershell -ExecutionPolicy Bypass -File .\eval\diagnosis_routing_jitter.ps1 -Runs 5 -Tag after
```

Required pass criteria:

- `port_why`: diagnosis >= 4/5.
- `gpu_notfound`: diagnosis >= 4/5.
- `ssh_timeout`: diagnosis >= 4/5.
- existing strong diagnosis cases remain diagnosis 5/5 or near-stable.
- `tutorial_github`: knowledge_qa 5/5.
- `slow_download`: knowledge_qa 5/5 unless product explicitly wants this as a future diagnosis class.
- platform how-to/error-code controls remain knowledge_qa.

Failure rule:

- If how-to controls leak into diagnosis, revert or narrow the prompt/examples before merging.

**Step 10: Commit Phase 1B**

Run:

```powershell
git add internal/intent/planner.go internal/intent/planner_examples/diagnosis.md internal/intent/planner_examples_test.go eval/diagnosis_routing_jitter_questions.json eval/diagnosis_routing_jitter.ps1
git commit -m "fix(planner): route no-target symptoms to diagnosis"
```

---

## Phase 2: Prove `diagnose-port-firewall` True-Skill Loop With Live CLI

**Purpose:** prove the body-read diagnosis path is not just implemented, but works safely on live CLI.

**Risk:** medium. The path is read-only and gated, but this is the first hard proof of the true-skill executor in real CLI.

**Prerequisite:** Phase 1B should pass, otherwise no-target port symptoms may never reach diagnosis.

**Files:**
- Use local-only: `eval/diagnose_skill_smoke.ps1` if it exists in the current checkout.
- Create if committing automation: `eval/diagnosis_true_skill_live_smoke.ps1`
- Create: `eval/diagnosis_true_skill_live_cases.json`
- Create: `eval/diagnosis_true_skill_live_report.md`
- Possibly modify: `eval/skill/README.md`

**Step 1: Add live cases**

Create cases:

```json
[
  {
    "id": "port_no_target",
    "question": "为什么我开的端口在外面访问不了",
    "expect_intent": "diagnosis",
    "expect_skill": "diagnose-port-firewall",
    "expect_retrieval": true,
    "expect_tools": ["SearchKnowledge", "DescribeCompShareInstance"],
    "forbid_mutating": true
  },
  {
    "id": "port_strong_symptom",
    "question": "我部署的服务外网打不开",
    "expect_intent": "diagnosis",
    "expect_skill": "diagnose-port-firewall",
    "expect_retrieval": true,
    "expect_tools": ["SearchKnowledge", "DescribeCompShareInstance"],
    "forbid_mutating": true
  },
  {
    "id": "github_tutorial_control",
    "question": "怎么加速github下载",
    "expect_intent": "knowledge_qa",
    "expect_skill": "",
    "expect_retrieval": true,
    "expect_tools": ["SearchKnowledge"],
    "forbid_diagnosis": true
  }
]
```

**Step 2: Harden the smoke script**

`eval/diagnose_skill_smoke.ps1` is currently a local smoke helper and may be untracked. It can be used for manual validation in the existing checkout, but do not commit it if it still contains hardcoded `F:\compshare-agent` paths.

If this phase needs a committed runner, create `eval/diagnosis_true_skill_live_smoke.ps1` instead. It must derive paths from the repo root:

```powershell
$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$agentExe = Join-Path $repoRoot "agent.exe"
$config = Join-Path $repoRoot "deploy\conf\agent.yaml"
$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"
. $smokeEnv
```

The runner must support:

- case file input.
- `-SkillExec 1`.
- `USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS=diagnose-port-firewall`.
- project `org-cwy2qk`.
- trace directory per case.
- JSON summary output.

Required environment:

```powershell
. .\eval\.smoke_env.ps1
$env:COMPSHARE_PROJECT_ID="org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS=""
$env:USE_SKILL_EXECUTOR="1"
$env:USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS="diagnose-port-firewall"
$env:COMPSHARE_TRACE_ENABLED="1"
```

**Step 3: Live CLI gate**

Run:

```powershell
go build -o agent.exe ./cmd
powershell -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 -SkillExec 1 -Runs 3 -Tag port-firewall
```

Required:

- diagnosis planner intent for port cases.
- executor path actually runs the body-read skill.
- `SearchKnowledge` appears before or alongside `DescribeCompShareInstance`.
- `EvidenceLedger` is present in internal prompt only as safe summary, not raw KB body.
- `KBChunk.Content` or raw retrieved paragraph must not appear in final answer.
- no mutating tool calls.
- if the skill executor fails, trace must show fail-closed fallback to shipped Go diagnosis chain.

**Step 4: Add a raw-leak regression assertion**

If the smoke parser can inspect trace/prompt safely, assert:

```text
final answer does not contain any retrieved KBChunk.Content body text
```

If raw chunk bodies are not available in trace, keep the unit test as the hard deterministic guard and document that live smoke checks only the final answer/transcript.

**Step 5: Compare executor off vs on**

Run once with executor off:

```powershell
$env:USE_SKILL_EXECUTOR=""
powershell -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 -SkillExec "" -Runs 3 -Tag port-firewall-off
```

Then run with executor on:

```powershell
$env:USE_SKILL_EXECUTOR="1"
$env:USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS="diagnose-port-firewall"
powershell -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 -SkillExec 1 -Runs 3 -Tag port-firewall-on
```

Report:

- same question.
- intent.
- actual runtime form.
- tool calls.
- retrieval trace presence.
- answer quality summary.
- safety violations: must be zero.

**Step 6: Unit tests**

Run:

```powershell
go test ./internal/engine ./internal/knowledge ./eval/skill -count=1
go vet ./internal/engine ./internal/knowledge ./eval/skill
```

Expected: all green.

**Step 7: Commit Phase 2**

Run:

```powershell
git add eval/diagnosis_true_skill_live_smoke.ps1 eval/diagnosis_true_skill_live_cases.json eval/diagnosis_true_skill_live_report.md eval/skill/README.md
git commit -m "test(eval): add live diagnosis true-skill smoke"
```

Only commit the report if it is de-identified and contains no secrets or live resource identifiers that should remain private.

---

## Phase 3: Extend Safe RAG Evidence To `diagnose-gpu-not-detected`

**Purpose:** apply the proven evidence adapter to the second highest-value diagnosis symptom: GPU not detected.

**Risk:** medium. RAG evidence must stay safe and route-independent.

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/skill_executor_pilot_test.go`
- Modify: `eval/skill/diagnosis_process_eval_test.go`
- Modify: `eval/diagnosis_true_skill_live_cases.json`
- Possibly modify: `internal/skills/diagnose-gpu-not-detected/SKILL.md`

**Step 1: Write failing unit test**

Add a test that `diagnose-gpu-not-detected`:

- probes knowledge when skill executor is on and allowlisted.
- receives `EvidenceLedger`.
- does not receive raw chunk content.
- exposes only read-only tools.
- fails closed on raw evidence leak.

Run:

```powershell
go test ./internal/engine -run "SkillExecutor|Evidence|GPU" -count=1
```

Expected: fail because GPU diagnosis does not use evidence yet.

**Step 2: Implement minimal evidence routing**

Change:

```go
func diagnosisSkillUsesKnowledgeEvidence(skillName string) bool
```

so it returns true for:

```text
diagnose-port-firewall
diagnose-gpu-not-detected
```

Do not broaden it to all diagnosis skills yet.

**Step 3: Update process eval**

Add/adjust GPU case so it expects:

- `EvidenceLedger` available.
- no raw evidence body.
- no mutating tools.
- actionable next step.

Run:

```powershell
go test ./internal/engine ./eval/skill -run "Diagnosis|SkillExecutor|Evidence" -count=1
```

Expected: green.

**Step 4: Live CLI gate**

Use a real instance if available. If no suitable instance exists, create one:

1. Check stock:

```powershell
"4090 现在有库存吗`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

2. If there is stock, create any instance through the agent with mutating tools enabled:

```powershell
. .\eval\.smoke_env.ps1
$env:COMPSHARE_PROJECT_ID="org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS="1"
$env:COMPSHARE_TRACE_ENABLED="1"
$env:COMPSHARE_TRACE_DIR="F:\compshare-agent-diagnosis-routing\eval\traces_create_test_instance"
"帮我创建一台有库存的GPU实例用于测试`ny`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

3. Extract `UHostId` from the trace or reply. Keep the instance for later tests; no teardown is required.

Then run:

```powershell
$env:USE_SKILL_EXECUTOR="1"
$env:USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS="diagnose-gpu-not-detected"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS=""
"跑模型的时候说找不到GPU`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

Required:

- intent `diagnosis`.
- body-read executor path is used for GPU allowlist.
- retrieval evidence is probed.
- only read-only tools are called.
- no raw evidence leak.

**Step 5: Commit Phase 3**

Run:

```powershell
git add internal/engine/engine.go internal/engine/skill_executor_pilot_test.go eval/skill/diagnosis_process_eval_test.go eval/diagnosis_true_skill_live_cases.json
git commit -m "feat(diagnosis): add safe evidence to gpu diagnosis skill"
```

---

## Phase 4: Decide Whether #117 Can Become Default-On

**Purpose:** make the default-on decision from data, not from smoke anecdotes.

**Risk:** high. This changes the main diagnosis execution path.

**Prerequisite:** at least two diagnosis skills have passed live CLI true-skill gates.

**Files:**
- Create: `eval/diagnosis_executor_ab_cases.json`
- Create: `eval/diagnosis_executor_ab.ps1`
- Create: `eval/diagnosis_executor_ab_report.md`
- Modify only if approved: `cmd/trace.go` default env behavior or config docs.

**Step 1: Build A/B case set**

Include at least:

- SSH no-target symptom.
- SSH concrete target symptom.
- port no-target symptom.
- GPU not found symptom.
- init stuck symptom.
- image dependency symptom.
- controls that should remain `knowledge_qa`.

Each case runs with:

- executor off.
- executor on with one allowlisted skill.
- executor on with all currently proven allowlisted skills.

**Step 2: A/B live CLI run**

Run:

```powershell
go build -o agent.exe ./cmd
powershell -ExecutionPolicy Bypass -File .\eval\diagnosis_executor_ab.ps1 -Runs 5
```

Required report dimensions:

- intent hit rate.
- right diagnosis skill / right chain rate.
- raw evidence leak count.
- mutating tool call count.
- fail-closed count.
- answer quality lightweight check.
- latency/token comparison.

**Step 3: Decision rule**

Do not default on unless:

- executor-on has no safety regressions.
- executor-on has equal or better route/process success than executor-off.
- raw evidence leaks are zero.
- mutating calls are zero.
- how-to controls do not enter diagnosis.
- fail-closed fallback works when the body loop fails.

**Step 4: If approved, widen allowlist first**

First widen only the allowlist:

```text
USE_SKILL_EXECUTOR=1
USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS=diagnose-port-firewall,diagnose-gpu-not-detected,...
```

Do not flip global default until a second PR proves this in live CLI.

**Step 5: Commit Phase 4 report**

Run:

```powershell
git add eval/diagnosis_executor_ab_cases.json eval/diagnosis_executor_ab.ps1 eval/diagnosis_executor_ab_report.md
git commit -m "test(eval): add diagnosis executor ab gate"
```

---

## Phase 5: Targeted Context Management For Diagnosis Only

**Purpose:** improve diagnosis quality without broad prompt bloat.

**Risk:** medium. More context can improve answers but can also leak irrelevant information.

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/orchestrator/*` if the seed schema needs a typed struct.
- Modify: `internal/engine/skill_executor_pilot_test.go`
- Modify: diagnosis SKILL.md files only if needed.

**Design:** only add fields to the diagnosis true-skill seed when they are grounded:

```text
SymptomType
TargetInstanceSummary
EvidenceLedger
ReadOnlyToolResults
NextStepExpectation
```

Do not add chat transcript dumps.
Do not pass raw RAG content.
Do not pass mutating tool options into true-skill context.

**Step 1: Add unit tests**

Assert:

- seed includes target instance summary when target exists.
- seed omits it when target is unknown.
- seed never includes raw KB content.
- seed does not include mutating workflow names.

Run:

```powershell
go test ./internal/engine -run "SkillExecutor|Seed|Diagnosis" -count=1
```

Expected: fail before implementation.

**Step 2: Implement typed seed**

Prefer a small typed helper over ad hoc map growth:

```text
buildDiagnosisSkillSeed(skillName, userMsg, args, evidenceLedger, instanceFacts)
```

**Step 3: Verify**

Run:

```powershell
go test ./internal/engine ./eval/skill -count=1
go vet ./internal/engine ./eval/skill
```

Expected: green.

**Step 4: Live CLI spot check**

Run one port and one GPU diagnosis with executor on.

Required:

- answer mentions actual instance state only when it has read it.
- no raw evidence leak.
- no mutating tool call.

**Step 5: Commit Phase 5**

Run:

```powershell
git add internal/engine/engine.go internal/engine/skill_executor_pilot_test.go eval/skill/diagnosis_process_eval_test.go
git commit -m "feat(diagnosis): structure true-skill context"
```

---

## Phase 6: Read-Only Tool Expansion After Reachability Fixes

**Purpose:** add new user-visible read-only routes only after existing routes and diagnosis entry are stable.

**Risk:** low to medium per route. Each new route changes planner surface and needs live smoke.

**Candidate order:**

1. `DescribeCompShareImageTags` -> image tag catalog/filter route.
2. `DescribeModelRepositoryModels` + `DescribeModelRepositoryTags` -> model repository browse route.
3. `DescribeCompShareSharingImages` -> shared-to-me image route.

**Do not prioritize:**

- `GetSoftwareURL` because the interface is known problematic.
- `GetOpenClawModelList` because it is OpenClaw-specific and low demand here.
- `DescribeCompShareSupportZone` as user-facing route; keep it as internal helper.
- delete APIs.

**Per-route files:**

- Create: `internal/routing/<route_name>/route.yaml`
- Create or modify: `internal/intent/routing_<route_name>.go`
- Modify/generated: `internal/routing/registry_gen.go`
- Modify/generated: route digest files if present.
- Modify: `internal/intent/routing_registry_test.go`
- Modify: `eval/skill/cases.jsonl`
- Create: `eval/<route_name>_smoke.ps1` if no generic route smoke exists.

**Per-route gates:**

```powershell
go test ./internal/intent ./internal/routing ./eval/skill -count=1
go build -o agent.exe ./cmd
```

Live CLI gate:

- question routes to the new intent.
- cutover is `dispatched`.
- exactly expected read-only tool calls happen.
- no extra mutating tools.
- boundary how-to/control cases do not get stolen.

**Commit per route**

Example:

Stage only the files touched by the route. Do not run `git add eval` or directory-wide adds that can sweep live traces.

Example:

```powershell
git add internal/routing/image_tag_catalog/route.yaml internal/routing/registry_gen.go internal/intent/routing_image_tag_catalog.go internal/intent/routing_registry_test.go eval/skill/cases.jsonl eval/image_tag_catalog_smoke.ps1
git commit -m "feat(routing): add image tag catalog route"
```

---

## Phase 7: Saga Workflow Work After `user_email` Gateway Context

**Purpose:** continue write-operation support, but only through saga workflows and confirmation gates.

**Risk:** high. These are real mutating operations.

**Prerequisite:** gateway/user_email behavior for custom image create is clarified if touching custom image create.

**Candidate order:**

1. `UpdateCompShareImage` / update custom image metadata.
2. `PublishCompShareImage` / publish community image.
3. disk attach/resize workflows.

**Global write gates:**

- `COMPSHARE_ENABLE_MUTATING_TOOLS=1` only for write smoke.
- confirmation card must show concrete action and parameters.
- denial leg must call no mutating API.
- approval leg must call exactly the intended mutating API.
- delete-class APIs remain hard-refused with zero tool calls.
- if a test needs a UHostId, create any stocked instance with agent workflow and use that ID.
- test-created instances do not need to be deleted after the run; record and reuse them.

**Live instance setup if needed:**

```powershell
. .\eval\.smoke_env.ps1
$env:COMPSHARE_PROJECT_ID="org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS="1"
$env:COMPSHARE_TRACE_ENABLED="1"
$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$env:COMPSHARE_TRACE_DIR = Join-Path $env:TEMP "compshare-write-setup-$runId"
"帮我创建一台有库存的GPU实例用于测试`ny`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

Extract the returned `UHostId` from trace/reply.
Keep the instance for later tests; no teardown is required.

**Custom image create follow-up after user_email**

Run deny leg:

```powershell
$env:COMPSHARE_ENABLE_MUTATING_TOOLS="1"
"把 <UHostId> 保存成自定义镜像，名字叫 claude-smoke-image`nN`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

Required:

- confirms before create.
- no `CreateCompShareCustomImage` after `N`.

Run approve leg:

```powershell
"把 <UHostId> 保存成自定义镜像，名字叫 claude-smoke-image`ny`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

Required:

- reaches `CreateCompShareCustomImage`.
- no `RetCode=210 Missing params [user_email]`.
- if created, progress check uses `GetCompShareImageCreateProgress` if implemented.
- follow-up check lists custom images and confirms the expected state.

Run destructive control:

```powershell
"销毁 <UHostId>`ny`nexit" | .\agent.exe cli -c .\deploy\conf\agent.yaml
```

Required:

- direct refusal.
- no API call.
- no confirmation prompt that could accidentally execute destructive action.

**Commit per workflow**

Example:

Stage only the files touched by the workflow. Do not run directory-wide adds that can sweep live traces.

Example:

```powershell
git add internal/workflow/custom_image_publish.go internal/workflow/custom_image_publish_test.go internal/engine/engine.go internal/tools/registry.go eval/custom_image_publish_smoke.ps1
git commit -m "feat(workflow): add custom image publish saga"
```

---

## Phase 8: SKILL.md Strict Alignment And Description-Driven Activation

**Purpose:** standardize true skills after the true-skill lane is actually reachable and useful.

**Risk:** medium to high if done too early, because naming and loader changes can churn registry/digests without improving behavior.

**Prerequisite:**

- at least one diagnosis true skill has passed live CLI.
- route/workflow terminology is stable.
- route manifests and true skill bundles are clearly separated.

**Scope:**

- true skills only.
- do not move deterministic routing workflows back into SKILL.md.
- `allowed-tools` is advisory/preauthorization metadata only; code-level tool gates stay authoritative.
- preserve progressive disclosure:
  - L1: name + description.
  - L2: body read only when skill executes.
  - L3: referenced files loaded only when needed.

**Potential files:**

- `internal/skills/loader.go`
- `internal/skills/*/SKILL.md`
- `internal/skills/registry_gen.go`
- skill digest/pin files.
- `internal/skills/loader_test.go`
- `eval/skill`

**Gate:**

```powershell
go test ./internal/skills ./internal/engine ./eval/skill -count=1
go vet ./internal/skills ./internal/engine ./eval/skill
```

Live CLI spot check:

- one diagnosis true skill still executes.
- no raw RAG leak.
- no mutating tools.

**Description-driven activation**

Do not implement until:

- true skill count or overlap is high enough to justify it.
- eval shows fixed mapping is failing on overlapping skill groups.
- N>=5 jitter data proves description-driven selection improves success without hurting safety.

---

## Phase 9: Frontend/Console Integration Gate

**Purpose:** validate production console behavior only after CLI confirms backend behavior.

**When to run:**

- after Phase 1A if showing network acceleration status in UI matters.
- after Phase 2 if diagnosis true-skill responses are going to console users.
- after Phase 7 for any write saga with confirmation cards.

**Gate:**

- frontend sends the same prompt.
- backend trace matches CLI trace shape.
- confirmation UI cannot skip deny/approve gate.
- streaming/SSE displays intermediate steps without exposing raw evidence.
- mutating-on deployment still hard-refuses delete-class actions.

**Do not use frontend testing as a substitute for CLI.** CLI proves backend behavior; frontend proves integration.

---

## Final Merge Gates For Each PR

Every PR in this plan must include:

```powershell
go test ./cmd ./internal/intent ./internal/engine ./internal/knowledge ./eval/skill -count=1
go vet ./cmd ./internal/intent ./internal/engine ./internal/knowledge ./eval/skill
```

Add narrower package tests when the PR touches:

- `internal/routing`
- `internal/skills`
- `internal/workflow`
- `internal/tools`

For any PR that changes planner prompt text:

```powershell
go test ./internal/intent -run "Prompt|systemPrompt|Planner|SkillRegistry" -count=1
```

The commit must state why the prompt hash changed.

For any PR that changes a live route/workflow:

- include a de-identified live CLI report.
- keep raw secrets and raw private IDs out of committed files.
- if raw trace contains private IDs, either redact or keep it local and summarize evidence.

---

## Recommended PR Slicing

1. PR A: Phase 1A only, `network_accelerator_status` default dispatch.
2. PR B: Phase 1B only, #123 diagnosis recall fix plus jitter report.
3. PR C: Phase 2 only, live true-skill diagnosis proof.
4. PR D: Phase 3 only, GPU diagnosis safe evidence.
5. PR E: Phase 4 A/B evaluation report; only flip defaults if report passes.
6. PR F+: read-only routes and saga workflows one at a time.

Do not batch PR A and PR B unless time is more important than review clarity. PR A is byte-stable and low risk; PR B intentionally changes planner behavior.
