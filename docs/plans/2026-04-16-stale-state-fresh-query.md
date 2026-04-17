# Stale Instance State Fresh-Query Guard

> Date: 2026-04-16
> Status: Approved, ready to implement

## Problem

When a user asks the agent to perform an instance operation (e.g. "帮我关掉 xxx"),
the model may reuse stale instance state from conversation history instead of
re-querying the current state. If the instance state changed externally (e.g. user
started it via the console), the agent gives an incorrect response.

**Reproduction:**
1. Turn 1: User asks "帮我关掉 qa-shadow-20260416-01"
2. Agent queries DescribeCompShareInstance → instance is Stopped → replies "已关机"
3. User starts the instance via console (external state change)
4. Turn 2: User asks "帮我关掉 qa-shadow-20260416-01" again
5. Agent reuses Turn 1's stale state → replies "已关机" without re-querying
6. User must say "是运行状态" to force a re-query

## Root Cause

The Engine's ReAct loop (`engine.go`) stores all conversation history in
`e.messages`. When the model sees a recent DescribeCompShareInstance result in
history showing "Stopped", it may decide not to call the tool again and directly
respond with text. The engine has no mechanism to signal that prior instance state
may be stale.

## Solution: Two-Layer Fix

### Layer 1: Prompt Hard Rule (`builder.go`)

Add to `systemTemplate`:

```text
## 实例状态刷新规则
对任何涉及实例变更的请求（开机/关机/重启/定时关机/取消定时关机/改名/重置密码），
即使在本轮之前的对话中已经查询过该实例状态，本轮仍必须先调用 DescribeCompShareInstance 获取最新状态后再决策。
原因：用户可能在控制台侧手动操作了实例，对话历史中的状态信息可能已过时。
禁止仅凭历史对话中的状态结论直接回答，或在未刷新状态的情况下跳过对应工作流。
```

### Layer 2: Engine Stale-State System Note (`engine.go`)

Add two fields to the Engine struct:

```go
type Engine struct {
    // ... existing fields ...
    userTurn              int // incremented at start of each Chat() call
    lastInstanceQueryTurn int // set to userTurn on successful DescribeCompShareInstance
}
```

**Freshness tracking via executor wrapper:**

The key challenge: workflow/diagnosis engines call `executor.Execute` internally,
and their event callbacks cannot distinguish "API succeeded + CheckResult failed"
from "API itself failed". For example, `StopInstanceWorkflow` querying a `Stopped`
instance: the API call succeeds (fresh data obtained), but `CheckResult` rejects
the state and emits `"failed"` — the exact first-turn path in our reproduction.

Solution: wrap the `ToolExecutor` with a `freshnessTracker` that intercepts
`Execute` calls. When `DescribeCompShareInstance` returns without error, it
updates `lastInstanceQueryTurn`. This works uniformly across all code paths
(direct API calls, workflow internals, diagnosis internals).

```go
// freshnessTracker wraps ToolExecutor to track when instance state was last
// successfully queried. Used by the stale-state detection mechanism.
type freshnessTracker struct {
    inner  tools.ToolExecutor
    engine *Engine
}

func (t *freshnessTracker) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
    result, err := t.inner.Execute(ctx, action, args)
    if err == nil && action == "DescribeCompShareInstance" {
        t.engine.lastInstanceQueryTurn = t.engine.userTurn
    }
    return result, err
}
```

Integration points:

1. `New()` — wrap the executor at construction time:
   ```go
   func New(cfg *config.Config, confirmFn ConfirmFunc) *Engine {
       executor := tools.NewExternalExecutor(cfg.Agent)
       eng := &Engine{confirmFn: confirmFn}
       eng.llmClient = llm.NewClient(cfg.Agent.LLM)
       eng.executor = &freshnessTracker{inner: executor, engine: eng}
       return eng
   }
   ```

2. `Chat()` entry — before appending user message:
   ```go
   e.userTurn++
   ```

3. No explicit freshness update needed in `executeTool()`, `executeWorkflow()`,
   or `executeDiagnosis()` — the wrapper handles all paths automatically.

**Injection logic** — new helper `buildMessagesForLLM()`:

```go
func (e *Engine) buildMessagesForLLM() []openai.ChatCompletionMessage {
    if e.lastInstanceQueryTurn == 0 || e.lastInstanceQueryTurn >= e.userTurn {
        return e.messages
    }
    msgs := make([]openai.ChatCompletionMessage, len(e.messages), len(e.messages)+1)
    copy(msgs, e.messages)
    return append(msgs, openai.ChatCompletionMessage{
        Role:    openai.ChatMessageRoleSystem,
        Content: staleStateNote,
    })
}
```

**Stale note content:**

```text
注意：本轮之前的对话中获取的实例状态信息可能已过时，用户可能已在控制台侧手动操作实例。
如果本轮需要基于实例当前状态作出判断，或执行实例变更操作，必须先调用 DescribeCompShareInstance 获取最新状态后再决策。
```

**Key design choices:**

- **Executor wrapper, not callback inspection.** The wrapper fires at the exact
  right moment: after `executor.Execute` succeeds, before CheckResult/Evaluate.
  This covers the "API success + CheckResult failure" path that callbacks miss.
- `lastInstanceQueryTurn` initializes to `-1` (never queried). The `-1` sentinel
  suppresses the note when no instance query has ever been made (e.g., when using
  `InitWithContext` for testing). Once Init() or any tool call queries
  `DescribeCompShareInstance`, the field becomes `>= 0` and participates in
  staleness checks. Init's snapshot (turn 0) IS treated as stale once the first
  Chat() advances `userTurn` to 1, which closes the gap where the user could
  change instance state via the console between startup and their first request.
- Note is inserted right after the system prompt (index 1), keeping it as a
  system-level instruction without interfering with tool results at the end.
- Note is NOT persisted in `e.messages` — only exists in the LLM call parameter.
- Workflow/diagnosis internal DescribeCompShareInstance calls also update
  freshness via the wrapper, preventing unnecessary stale notes after workflows.

## What This Does NOT Do

- No keyword-based intent detection in engine (no NLU in the dispatch layer).
- No modification of historical tool result text.
- No deterministic routing (e.g. "关机 + instance → StopInstanceWorkflow").
- No per-instance freshness tracking (`map[uhostId]int`). Global freshness only
  for v1.

## Tests (6 cases)

All in `engine_test.go` unless noted:

| # | Name | What it verifies |
|---|------|-----------------|
| 1 | Stale state triggers note | `lastInstanceQueryTurn < userTurn` → stale note in LLM messages |
| 2 | Fresh state no note | `lastInstanceQueryTurn == userTurn` → no stale note |
| 3 | Workflow refreshes freshness even on CheckResult failure | StopInstanceWorkflow queries Stopped instance → API succeeds but CheckResult rejects → `lastInstanceQueryTurn` still updated via executor wrapper |
| 4 | Init snapshot stale on first turn | Init sets `lastInstanceQueryTurn=0`; first Chat (turn 1) → note fires |
| 5 | Never queried no note | `lastInstanceQueryTurn=-1` (sentinel) → no note |
| 6 | FAQ not derailed by stale note | Stale note present but FAQ question → model still answers FAQ, no forced workflow |
| 7 | External state change regression | Turn 1: Stopped → "已关机". Turn 2: executor returns Running → must re-query, must not reuse stale conclusion |

## Files Changed

| File | Change |
|------|--------|
| `internal/prompt/builder.go` | Add 实例状态刷新规则 section to systemTemplate |
| `internal/engine/engine.go` | Add userTurn, lastInstanceQueryTurn fields; add freshnessTracker wrapper; update New(), Chat(); add buildMessagesForLLM() |
| `internal/engine/engine_test.go` | Add 6 test cases |
