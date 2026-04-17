# Design: Vague Failure Clarification Before Diagnosis Routing

**Date**: 2026-04-17  
**Status**: Approved  
**Scope**: `internal/engine/engine.go`, `internal/prompt/builder.go`, `internal/engine/engine_test.go`

---

## Problem

"昨晚那台跑崩了" → agent calls `DiagnoseInitFailure` with no `UHostId` → scans all instances → returns a list of 4 unrelated failed instances.

Root cause: the prompt maps vague fault language ("跑崩了") directly to a specific Diagnose* chain. "跑崩了" can mean SSH failure, GPU OOM, service crash, or init failure — the model must clarify before picking a diagnosis tool.

---

## Approved Design

### Layer 1: Prompt (builder.go)

**Three targeted edits to `systemTemplate`. No other rules touched.**

#### Edit 1 — Add `vague_failure` intent class (before `diagnosis` block)

```
- vague_failure：用户描述了"实例出了问题"，但症状类型不明确
  （如"跑崩了"、"崩了"、"挂了"、"挂住了"、"不对劲"、"不行了"、
  "起不来"、"有问题"、"出问题了"、"异常"等口语表达），
  无法直接确定应走哪条 Diagnose* 工具时 →
  先追问两件事：①哪台实例？②具体是什么现象（SSH 断了？GPU 报错？服务崩了？初始化卡住？）
  不得直接调用任何 Diagnose* 工具。
  注意：即使用户给出了实例 ID 或名称，只要症状描述仍然模糊，也走此路径先追问症状。
```

#### Edit 2 — Tighten DiagnoseInitFailure mapping in `diagnosis` block

Replace:
```
- 创建失败/初始化失败 → 调用 DiagnoseInitFailure
```

With:
```
- 用户明确说"初始化失败"、"Install Fail"、"卡在初始化"、"卡在启动"、"Starting 很久" →
  调用 DiagnoseInitFailure
```

Other Diagnose* paths (DiagnoseSSH, DiagnoseGPU, DiagnoseBilling, DiagnosePortOrFirewall, DiagnoseImageIssue) are already specific enough — no change.

#### Edit 3 — Add exception note under `diagnosis` 重要 block

```
**例外**：若用户描述模糊（如"跑崩了"、"有问题"、"异常"等），无法确定症状类型，
按 vague_failure 处理：先追问实例 + 症状，再决定调哪个诊断工具。
模糊故障描述优先于具体 Diagnose 路由。
```

---

### Layer 2: Engine Guard (engine.go)

Narrow hard guard on `DiagnoseInitFailure` only. Does NOT redirect to another Diagnose* chain.

#### Struct field

```go
type Engine struct {
    // existing fields ...
    lastUserMsg string // raw user message for current turn; read-only after Chat() sets it
}
```

Set at the start of `Chat()`, before the ReAct loop:
```go
e.lastUserMsg = userMsg
```

#### Helper: `normalizeMsg(s string) string`

Strips leading/trailing whitespace, collapses internal whitespace runs, lowercases ASCII. Used only for signal matching; `lastUserMsg` is stored as-is.

#### Helper: `containsInitFailureSignal(msg string) bool`

Returns true if `normalizeMsg(msg)` contains any of:
```
"初始化失败", "install fail", "卡在初始化", "卡在启动", "starting很久", "starting 很久", "一直starting", "一直 starting"
```

This is the symptom-specificity gate. "跑崩了"、"挂了"、"有问题" do NOT match.

#### Helper: `containsScanAllSignal(msg string) bool`

Returns true if `normalizeMsg(msg)` contains any of:
```
"所有实例", "全部实例", "哪些实例", "有哪些", "帮我扫",
"全量", "所有的", "全部失败", "失败的实例", "扫一下失败", "都有哪些"
```

Only used as a second-level check after `containsInitFailureSignal` passes. A scan-all phrase alone (without an init-failure symptom signal) does NOT bypass the guard.

#### Target-presence check

A target is present when ANY of the following is non-empty in `args`:
- `args["UHostId"].(string)`
- `args["Name"].(string)`
- `args["UHostIds"].([]any)` (non-empty slice)

#### Guard in `executeDiagnosis` — two-gate logic

```go
if action == "DiagnoseInitFailure" {
    // Gate 1: symptom specificity — must mention init-failure symptoms explicitly.
    // Blocks vague failure descriptions ("跑崩了", "挂了") regardless of whether
    // a target instance was provided.
    if !containsInitFailureSignal(e.lastUserMsg) {
        msg := "请问是哪台实例出了问题？能描述一下具体现象吗（例如：SSH 断了、GPU 报错、服务崩了、初始化卡住等）？"
        onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
        return finalReplyPrefix + msg
    }

    // Gate 2: instance disambiguation — if symptom is specific but no target
    // and no explicit scan-all intent, ask which instance.
    hasTarget := false
    if s, _ := args["UHostId"].(string); s != "" { hasTarget = true }
    if s, _ := args["Name"].(string); s != "" { hasTarget = true }
    if ids, ok := args["UHostIds"].([]any); ok && len(ids) > 0 { hasTarget = true }

    if !hasTarget && !containsScanAllSignal(e.lastUserMsg) {
        msg := "请问是哪台实例的初始化失败了？"
        onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
        return finalReplyPrefix + msg
    }
}
```

**Decision table:**

| User message | Gate 1 | Gate 2 | Result |
|---|---|---|---|
| "uhost-xxx 跑崩了" | FAIL (no init-failure signal) | — | ❌ Blocked → clarify |
| "昨晚那台跑崩了" | FAIL | — | ❌ Blocked → clarify |
| "帮我扫一下所有有问题的实例" | FAIL (no init-failure signal) | — | ❌ Blocked → clarify |
| "就是 wyptest 那台初始化失败了" | PASS | hasTarget=true → PASS | ✅ Runs |
| "帮我看看哪些实例初始化失败了" | PASS | no target, containsScanAll=true → PASS | ✅ Runs |
| "昨晚那台卡在初始化了" | PASS | no target, no scan-all → FAIL | ❌ Blocked → ask which instance |

---

### Layer 3: Tests (engine_test.go)

#### Unit tests

`TestNormalizeMsg` — whitespace collapse, ASCII lowercasing.

`TestContainsScanAllSignal`
- Positive: "帮我看看哪些实例初始化失败了", "帮我扫全部", "全部失败的实例都查一下", "都有哪些"
- Negative: "跑崩了", "昨晚那台挂了", "uhost-xxx 有问题", "帮我扫一下所有有问题的实例"

`TestContainsInitFailureSignal`
- Positive: "初始化失败了", "install fail", "卡在初始化", "卡在启动", "starting很久", "一直 starting"
- Negative: "跑崩了", "挂了", "有问题", "帮我扫一下所有有问题的实例", "uhost-xxx 崩了"

#### Scenario tests

`TestVagueCrashGuard_VagueNoTargetBlocked`
- Input: "昨晚那台跑崩了" → LLM calls DiagnoseInitFailure with no target → Gate 1 FAIL → returns clarification

`TestVagueCrashGuard_VagueWithTargetBlocked` *(P1 regression)*
- Input: "uhost-xxx 跑崩了" → LLM calls DiagnoseInitFailure with UHostId="uhost-xxx" → Gate 1 FAIL → returns clarification

`TestVagueCrashGuard_VagueScanAllBlocked` *(P2 regression)*
- Input: "帮我扫一下所有有问题的实例" → LLM calls DiagnoseInitFailure with no target → Gate 1 FAIL → returns clarification

`TestVagueCrashGuard_ExplicitInitFailureScanAllPasses`
- Input: "帮我看看哪些实例初始化失败了" → Gate 1 PASS, Gate 2 scan-all PASS → diagnosis runs

`TestVagueCrashGuard_NameTargetPasses`
- Input: "就是 wyptest 那台初始化失败了" → Gate 1 PASS, Gate 2 hasTarget=true → guard does NOT fire

`TestVagueCrashGuard_UHostIdTargetPasses`
- Input: LLM calls DiagnoseInitFailure with UHostId="uhost-xxx", msg="初始化失败了" → Gate 1 PASS, Gate 2 hasTarget → does NOT fire

`TestVagueCrashGuard_SpecificNoTargetBlocked`
- Input: "昨晚那台卡在初始化了" → Gate 1 PASS (has init-failure signal), Gate 2 no target no scan-all → blocked with "哪台实例" question

---

## Constraints (from design review)

1. `vague_failure` is narrowly scoped to ambiguous fault descriptions only. It does not absorb general ambiguous operations or knowledge QA.
2. The engine guard is a scan-all blocker, not a re-router. It returns a clarification message and stops — it does not guess which Diagnose* chain to use instead.
3. `lastUserMsg` is stored on `Engine`; `normalizeMsg` is a reusable helper for all signal-matching functions.

## Out of Scope

- Guards on DiagnoseSSH, DiagnoseGPU, DiagnosePortOrFirewall, DiagnoseBilling, DiagnoseImageIssue
- Changes to `diagnosis/init_failure.go` (fix is upstream)
- Changes to `tools/registry.go` (no schema changes)
