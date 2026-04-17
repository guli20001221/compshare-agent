# Vague-Failure Clarification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make "昨晚那台跑崩了"-style vague fault descriptions trigger a clarification question instead of a broad `DiagnoseInitFailure` scan.

**Architecture:** Two-layer fix. Prompt adds a `vague_failure` intent class and tightens Diagnose* symptom signals. Engine adds a two-gate guard on `DiagnoseInitFailure` (Gate 1 = symptom specificity, Gate 2 = instance disambiguation). Guard relies on the user message stored on the Engine.

**Tech Stack:** Go, `github.com/stretchr/testify/assert`, `github.com/sashabaranov/go-openai`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-04-17-vague-failure-clarification-design.md`

---

## File Structure

- Modify: `internal/engine/engine.go` — add `lastUserMsg` field, set in `Chat()`, add `normalizeMsg`, `containsInitFailureSignal`, `containsScanAllSignal` helpers, add two-gate guard in `executeDiagnosis`
- Modify: `internal/prompt/builder.go` — three edits to `systemTemplate` (add `vague_failure` class, tighten `DiagnoseInitFailure` mapping, add exception note)
- Modify: `internal/engine/engine_test.go` — unit tests for three helpers + seven scenario tests for the two-gate guard

No registry changes. No workflow changes. No `diagnosis/init_failure.go` changes.

---

## Task 1: Add `lastUserMsg` field and wire into `Chat()`

**Files:**
- Modify: `internal/engine/engine.go`

- [ ] **Step 1: Add `lastUserMsg` field to `Engine` struct**

Open `internal/engine/engine.go`. In the `Engine` struct (line 39), add one new field at the bottom of the existing fields:

```go
// Engine runs the ReAct loop: User → LLM → Tool → LLM → ... → Reply.
type Engine struct {
	llmClient             LLMClient
	executor              tools.ToolExecutor
	confirmFn             ConfirmFunc
	messages              []openai.ChatCompletionMessage // conversation history
	userTurn              int                            // incremented at start of each Chat() call
	lastInstanceQueryTurn int                            // set to userTurn on successful DescribeCompShareInstance
	// Diagnosis follow-up tracking (narrow, only DiagnoseBilling for now).
	// Updated after a successful executeDiagnosis run; read at the start of
	// the next Chat() to decide whether to force DiagnoseBilling via tool_choice.
	lastDiagnosisTool    string   // empty until a tracked diagnosis completes
	lastDiagnosisTurn    int      // init -1; set to userTurn when tracked diagnosis runs
	lastDiagnosisTargets []string // target strings extracted from diagnosis args (UHostId, Name)
	// Raw user message for the current turn. Set at the start of Chat().
	// Read by executeDiagnosis guards for signal matching. Never mutated
	// mid-turn.
	lastUserMsg string
}
```

- [ ] **Step 2: Set `lastUserMsg` at the start of `Chat()`**

In `Chat()` (line 120), right after `e.userTurn++`, set the field:

```go
func (e *Engine) Chat(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, error) {
	e.userTurn++
	e.lastUserMsg = userMsg

	// Trim before appending to guarantee the new user message is never dropped.
	e.trimHistory()
```

- [ ] **Step 3: Build to confirm compile passes**

Run: `cd /f/compshare-agent && go build ./internal/engine/...`
Expected: exits 0, no output.

- [ ] **Step 4: Commit**

```bash
cd /f/compshare-agent
git add internal/engine/engine.go
git commit -m "engine: add lastUserMsg field for per-turn signal matching"
```

---

## Task 2: Add `normalizeMsg` helper (TDD)

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

- [ ] **Step 1: Write failing test**

Append to `internal/engine/engine_test.go` (at the very end of the file):

```go
func TestNormalizeMsg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"trim leading trailing spaces", "  hello  ", "hello"},
		{"collapse internal spaces", "foo   bar", "foo bar"},
		{"collapse tabs and newlines", "foo\t\nbar", "foo bar"},
		{"lowercase ascii", "Install Fail", "install fail"},
		{"preserve chinese", "初始化失败", "初始化失败"},
		{"mixed ascii chinese", " Install  Fail 初始化", "install fail 初始化"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeMsg(tc.in))
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestNormalizeMsg -v`
Expected: FAIL — `undefined: normalizeMsg`.

- [ ] **Step 3: Implement `normalizeMsg`**

Open `internal/engine/engine.go`. At the top of the file, ensure `unicode` is importable by adding to the import block (leave existing imports in place):

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/sanitizer"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"

	openai "github.com/sashabaranov/go-openai"
)
```

Add the helper just below the existing `billingFollowUpKeywords` var block (near line 627):

```go
// normalizeMsg standardizes a user message for signal matching:
// trims whitespace, collapses internal whitespace runs to a single space,
// and lowercases ASCII letters. CJK characters are preserved as-is.
// The returned value is used only for substring matching; the caller's
// original string is never mutated.
func normalizeMsg(s string) string {
	var b strings.Builder
	prevSpace := true // treat start as space so leading whitespace collapses
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	out := b.String()
	return strings.TrimRight(out, " ")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestNormalizeMsg -v`
Expected: PASS — all 7 subtests pass.

- [ ] **Step 5: Commit**

```bash
cd /f/compshare-agent
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "engine: add normalizeMsg helper for signal matching"
```

---

## Task 3: Add `containsInitFailureSignal` helper (TDD)

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

- [ ] **Step 1: Write failing test**

Append to `internal/engine/engine_test.go`:

```go
func TestContainsInitFailureSignal(t *testing.T) {
	positives := []string{
		"初始化失败了",
		"Install Fail",
		"install fail",
		"卡在初始化",
		"卡在启动",
		"starting很久",
		"一直 starting",
		"uhost-xxx 初始化失败",
	}
	negatives := []string{
		"跑崩了",
		"挂了",
		"有问题",
		"帮我扫一下所有有问题的实例",
		"uhost-xxx 崩了",
		"昨晚那台不行了",
		"",
	}
	for _, msg := range positives {
		t.Run("positive/"+msg, func(t *testing.T) {
			assert.True(t, containsInitFailureSignal(msg), "want true for %q", msg)
		})
	}
	for _, msg := range negatives {
		t.Run("negative/"+msg, func(t *testing.T) {
			assert.False(t, containsInitFailureSignal(msg), "want false for %q", msg)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestContainsInitFailureSignal -v`
Expected: FAIL — `undefined: containsInitFailureSignal`.

- [ ] **Step 3: Implement helper**

In `internal/engine/engine.go`, add below `normalizeMsg`:

```go
// initFailureSignalKeywords is a narrow word list that marks a user message
// as specifically about init-failure symptoms. Keep it tight — keywords
// like "起不来" are too ambiguous (could be SSH / GPU / service) and must
// NOT live here.
var initFailureSignalKeywords = []string{
	"初始化失败",
	"install fail",
	"卡在初始化",
	"卡在启动",
	"starting很久",
	"starting 很久",
	"一直starting",
	"一直 starting",
}

// containsInitFailureSignal reports whether the user message contains an
// init-failure-specific symptom signal. This is Gate 1 of the
// DiagnoseInitFailure guard: vague fault language ("跑崩了", "挂了") does
// NOT match; the user must have named the symptom type explicitly.
func containsInitFailureSignal(msg string) bool {
	n := normalizeMsg(msg)
	for _, kw := range initFailureSignalKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestContainsInitFailureSignal -v`
Expected: PASS — all 15 subtests pass.

- [ ] **Step 5: Commit**

```bash
cd /f/compshare-agent
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "engine: add containsInitFailureSignal (Gate 1 of vague-crash guard)"
```

---

## Task 4: Add `containsScanAllSignal` helper (TDD)

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

- [ ] **Step 1: Write failing test**

Append to `internal/engine/engine_test.go`:

```go
func TestContainsScanAllSignal(t *testing.T) {
	positives := []string{
		"帮我看看哪些实例初始化失败了",
		"帮我扫全部",
		"全部失败的实例都查一下",
		"都有哪些失败的",
		"所有实例的状态",
		"有哪些实例挂了",
		"扫一下失败的",
	}
	negatives := []string{
		"跑崩了",
		"昨晚那台挂了",
		"uhost-xxx 有问题",
		"wyptest 那台",
		"",
	}
	for _, msg := range positives {
		t.Run("positive/"+msg, func(t *testing.T) {
			assert.True(t, containsScanAllSignal(msg), "want true for %q", msg)
		})
	}
	for _, msg := range negatives {
		t.Run("negative/"+msg, func(t *testing.T) {
			assert.False(t, containsScanAllSignal(msg), "want false for %q", msg)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestContainsScanAllSignal -v`
Expected: FAIL — `undefined: containsScanAllSignal`.

- [ ] **Step 3: Implement helper**

In `internal/engine/engine.go`, add below `containsInitFailureSignal`:

```go
// scanAllSignalKeywords is a narrow list of phrases that indicate the user
// explicitly wants a broad scan across all instances. Used only as Gate 2
// of the DiagnoseInitFailure guard — consulted AFTER the symptom-specificity
// gate passes. A scan-all phrase alone (without an init-failure signal)
// does NOT bypass the guard.
var scanAllSignalKeywords = []string{
	"所有实例",
	"全部实例",
	"哪些实例",
	"有哪些",
	"帮我扫",
	"全量",
	"所有的",
	"全部失败",
	"失败的实例",
	"扫一下失败",
	"都有哪些",
}

// containsScanAllSignal reports whether the user message expresses an
// explicit intent to scan across all instances.
func containsScanAllSignal(msg string) bool {
	n := normalizeMsg(msg)
	for _, kw := range scanAllSignalKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestContainsScanAllSignal -v`
Expected: PASS — all 12 subtests pass.

- [ ] **Step 5: Commit**

```bash
cd /f/compshare-agent
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "engine: add containsScanAllSignal (Gate 2 of vague-crash guard)"
```

---

## Task 5: Implement Gate 1 of the guard (symptom specificity)

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

This task installs Gate 1 and verifies it blocks vague failure descriptions in three key scenarios: (a) no target, (b) target provided (P1 regression), (c) scan-all phrasing without init-failure signal (P2 regression).

- [ ] **Step 1: Write scenario helper and failing tests**

Append to `internal/engine/engine_test.go`:

```go
// initFailureScenarioExecutor returns a mockExecutor with a minimal
// UHostSet so that DiagnoseInitFailure's chain can execute when allowed
// past the guard. The host state is configurable so tests can model
// different outcomes.
func initFailureScenarioExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":            "uhost-init-001",
					"Name":               "wyptest",
					"State":              "Install Fail",
					"CompShareImageName": "cuda130_torch291_py312",
				},
			},
		},
	}}
}

const vagueClarifyPrefix = "请问是哪台实例出了问题？"

func TestVagueCrashGuard_VagueNoTargetBlocked(t *testing.T) {
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "昨晚那台跑崩了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, vagueClarifyPrefix,
		"vague failure with no target must trigger Gate 1 clarification")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance",
		"guard must stop the chain before any API call")
}

func TestVagueCrashGuard_VagueWithTargetBlocked(t *testing.T) {
	// P1 regression: guard must fire even when the LLM provides a target,
	// because the user's symptom description is still vague.
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-init-001"}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "uhost-init-001 跑崩了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, vagueClarifyPrefix,
		"vague failure wording must trigger Gate 1 even when target is known")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance")
}

func TestVagueCrashGuard_VagueScanAllBlocked(t *testing.T) {
	// P2 regression: scan-all phrasing alone must NOT bypass the guard when
	// the user has not named an init-failure symptom. "所有有问题的实例"
	// is vague — could be SSH, GPU, billing, etc.
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我扫一下所有有问题的实例", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, vagueClarifyPrefix,
		"scan-all phrasing without init-failure signal must still be blocked")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestVagueCrashGuard -v`
Expected: FAIL — tests run, but reply does NOT contain the clarification prefix (guard not installed yet). The scenarios will fall through to the empty LLM queue and likely return "no more mock responses" or similar.

- [ ] **Step 3: Install Gate 1 in `executeDiagnosis`**

Open `internal/engine/engine.go`. In `executeDiagnosis` (line 388), add the Gate 1 block immediately after the chain lookup and before `diagEngine := diagnosis.NewEngine(...)`:

```go
// executeDiagnosis runs a diagnostic chain and returns the result as JSON.
func (e *Engine) executeDiagnosis(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	chain, ok := diagnosis.GetChain(action)
	if !ok {
		msg := fmt.Sprintf("未知的诊断链: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
	}

	// Vague-failure guard — DiagnoseInitFailure only.
	// Gate 1 (symptom specificity): the user message must contain an
	// init-failure-specific signal. Vague fault language like "跑崩了" /
	// "挂了" is blocked here, even if the LLM provided a target instance.
	// This is a hard safety net behind the prompt-level vague_failure
	// routing class — deliberately does NOT redirect to another Diagnose*.
	if action == "DiagnoseInitFailure" && !containsInitFailureSignal(e.lastUserMsg) {
		msg := "请问是哪台实例出了问题？能描述一下具体现象吗（例如：SSH 断了、GPU 报错、服务崩了、初始化卡住等）？"
		onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
		return finalReplyPrefix + msg
	}

	diagEngine := diagnosis.NewEngine(e.executor, func(ev diagnosis.DiagEvent) {
```

- [ ] **Step 4: Run tests to verify Gate 1 tests pass**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestVagueCrashGuard -v`
Expected: PASS — all three Gate 1 tests pass.

- [ ] **Step 5: Commit**

```bash
cd /f/compshare-agent
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "engine: install Gate 1 of vague-crash guard (symptom specificity)"
```

---

## Task 6: Implement Gate 2 of the guard (instance disambiguation)

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

Gate 2 fires only after Gate 1 passes: if the user named an init-failure symptom but no target, the guard asks which instance (unless the user also said scan-all).

- [ ] **Step 1: Write failing tests for Gate 2**

Append to `internal/engine/engine_test.go`:

```go
const specificClarifyPrefix = "请问是哪台实例的初始化失败了？"

func TestVagueCrashGuard_SpecificNoTargetBlocked(t *testing.T) {
	// Gate 1 passes (has init-failure signal), Gate 2 fires (no target, no scan-all).
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "昨晚那台卡在初始化了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, specificClarifyPrefix,
		"init-failure signal but no target must trigger Gate 2 clarification")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance")
}

func TestVagueCrashGuard_NameTargetPasses(t *testing.T) {
	// Gate 1 passes (has init-failure signal), Gate 2 passes (Name is non-empty).
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"Name":"wyptest"}`),
		}},
		{Content: "wyptest 初始化失败，建议重建"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "就是 wyptest 那台初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.NotContains(t, reply, specificClarifyPrefix)
	assert.NotContains(t, reply, vagueClarifyPrefix)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance",
		"diagnosis chain must run when guard passes")
}

func TestVagueCrashGuard_UHostIdTargetPasses(t *testing.T) {
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-init-001"}`),
		}},
		{Content: "实例初始化失败，建议重建"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "uhost-init-001 初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.NotContains(t, reply, specificClarifyPrefix)
	assert.NotContains(t, reply, vagueClarifyPrefix)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
}

func TestVagueCrashGuard_ExplicitInitFailureScanAllPasses(t *testing.T) {
	// Gate 1 passes (init-failure signal), Gate 2 passes (scan-all intent).
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
		{Content: "共发现 1 台初始化失败的实例"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我看看哪些实例初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.NotContains(t, reply, specificClarifyPrefix)
	assert.NotContains(t, reply, vagueClarifyPrefix)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance",
		"scan-all must be allowed when both init-failure signal and scan-all phrasing are present")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestVagueCrashGuard -v`
Expected: `SpecificNoTargetBlocked` FAILS (guard not installed — falls through and diagnosis runs, so reply does not contain `specificClarifyPrefix`). The three "passes" tests will also fail because the diagnosis chain runs but the LLM follow-up response is not properly wired — actually they should pass the guard check and run through the chain. Focus on: `SpecificNoTargetBlocked` must fail.

- [ ] **Step 3: Install Gate 2 in `executeDiagnosis`**

In `internal/engine/engine.go`, extend the guard block to add Gate 2 immediately after the Gate 1 return:

```go
	// Vague-failure guard — DiagnoseInitFailure only.
	// Gate 1 (symptom specificity): the user message must contain an
	// init-failure-specific signal. Vague fault language like "跑崩了" /
	// "挂了" is blocked here, even if the LLM provided a target instance.
	// This is a hard safety net behind the prompt-level vague_failure
	// routing class — deliberately does NOT redirect to another Diagnose*.
	if action == "DiagnoseInitFailure" && !containsInitFailureSignal(e.lastUserMsg) {
		msg := "请问是哪台实例出了问题？能描述一下具体现象吗（例如：SSH 断了、GPU 报错、服务崩了、初始化卡住等）？"
		onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
		return finalReplyPrefix + msg
	}

	// Gate 2 (instance disambiguation): symptom is specific, but if no
	// target was provided and the user did not ask for a scan-all, ask
	// which instance. Avoids implicit scan-all when the user has a
	// specific instance in mind but didn't name it.
	if action == "DiagnoseInitFailure" {
		hasTarget := false
		if s, _ := args["UHostId"].(string); s != "" {
			hasTarget = true
		}
		if s, _ := args["Name"].(string); s != "" {
			hasTarget = true
		}
		if ids, ok := args["UHostIds"].([]any); ok && len(ids) > 0 {
			hasTarget = true
		}
		if !hasTarget && !containsScanAllSignal(e.lastUserMsg) {
			msg := "请问是哪台实例的初始化失败了？"
			onStep(StepEvent{Type: StepBlocked, Action: action, Message: msg})
			return finalReplyPrefix + msg
		}
	}
```

- [ ] **Step 4: Run all guard tests to verify they pass**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -run TestVagueCrashGuard -v`
Expected: PASS — all 7 scenario tests pass (`VagueNoTargetBlocked`, `VagueWithTargetBlocked`, `VagueScanAllBlocked`, `SpecificNoTargetBlocked`, `NameTargetPasses`, `UHostIdTargetPasses`, `ExplicitInitFailureScanAllPasses`).

- [ ] **Step 5: Run full engine test suite to catch regressions**

Run: `cd /f/compshare-agent && go test ./internal/engine/ -v`
Expected: PASS — all existing tests still pass (billing guard tests, scenario tests, etc.).

- [ ] **Step 6: Commit**

```bash
cd /f/compshare-agent
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "engine: install Gate 2 of vague-crash guard (instance disambiguation)"
```

---

## Task 7: Update prompt (builder.go)

**Files:**
- Modify: `internal/prompt/builder.go`

- [ ] **Step 1: Edit 1 — Add `vague_failure` intent class**

Open `internal/prompt/builder.go`. The `行为规则` section currently lists `simple_query`, `knowledge_qa`, `complex_task`, `diagnosis`, `recommendation`. Insert the new `vague_failure` class immediately BEFORE the `diagnosis` bullet.

Find this block (around line 45):

```
- diagnosis：用户报告了问题 → 使用诊断工具自动排查：
```

Replace with:

```
- vague_failure：用户描述了"实例出了问题"，但症状类型不明确（如"跑崩了"、"崩了"、"挂了"、"挂住了"、"不对劲"、"不行了"、"起不来"、"有问题"、"出问题了"、"异常"等口语表达），无法直接确定应走哪条 Diagnose* 工具时 → 先追问两件事：①哪台实例？②具体是什么现象（SSH 断了？GPU 报错？服务崩了？初始化卡住？） 不得直接调用任何 Diagnose* 工具。 注意：即使用户给出了实例 ID 或名称，只要症状描述仍然模糊，也走此路径先追问症状。
- diagnosis：用户报告了问题 → 使用诊断工具自动排查：
```

- [ ] **Step 2: Edit 2 — Tighten `DiagnoseInitFailure` mapping**

Find this line (around line 47):

```
  - 创建失败/初始化失败 → 调用 DiagnoseInitFailure
```

Replace with:

```
  - 用户明确说"初始化失败"、"Install Fail"、"卡在初始化"、"卡在启动"、"Starting 很久" → 调用 DiagnoseInitFailure
```

- [ ] **Step 3: Edit 3 — Add exception note under `重要` block**

Find this block (around line 53):

```
  **重要**：用户描述了具体问题/故障时（SSH连不上、端口不通、nvidia-smi报错、初始化失败、扣费异常、镜像无法使用等），必须调用对应的 Diagnose* 诊断工具进行自动排查，禁止仅用知识文本直接回答。诊断工具会自动排查并给出结论。
```

Append a new paragraph directly after (on the next line within the same string literal):

```
  **例外**：若用户描述模糊（如"跑崩了"、"有问题"、"异常"等），无法确定症状类型，按 vague_failure 处理：先追问实例 + 症状，再决定调哪个诊断工具。模糊故障描述优先于具体 Diagnose 路由。
```

- [ ] **Step 4: Verify builder compiles and unit tests pass**

Run: `cd /f/compshare-agent && go test ./internal/prompt/ -v`
Expected: PASS — existing `builder_test.go` tests still pass (the template expands to a well-formed prompt; no structural changes).

- [ ] **Step 5: Commit**

```bash
cd /f/compshare-agent
git add internal/prompt/builder.go
git commit -m "prompt: add vague_failure intent class and tighten DiagnoseInitFailure mapping"
```

---

## Task 8: Full-suite regression check

**Files:** none (verification only)

- [ ] **Step 1: Run all Go tests**

Run: `cd /f/compshare-agent && go test ./... -count=1`
Expected: PASS for `internal/engine`, `internal/prompt`, and all other packages that were already passing before these changes.

If any previously-passing test now fails, stop and investigate before continuing. Do NOT mask regressions with quick fixes — root-cause them.

- [ ] **Step 2: Verify no uncommitted changes remain**

Run: `cd /f/compshare-agent && git status`
Expected: `working tree clean` for the files changed in this plan (other pre-existing modifications in the repo are unrelated to this change).

---

## Summary of deliverables

After completing all tasks:
- `internal/engine/engine.go` — `lastUserMsg` field, three helpers (`normalizeMsg`, `containsInitFailureSignal`, `containsScanAllSignal`), two-gate guard in `executeDiagnosis`
- `internal/engine/engine_test.go` — 3 unit tests + 7 scenario tests for the guard
- `internal/prompt/builder.go` — `vague_failure` intent class, tightened DiagnoseInitFailure mapping, exception note

**Spec coverage check:**
- Prompt Edit 1 (`vague_failure` class) → Task 7 Step 1 ✓
- Prompt Edit 2 (tighten DiagnoseInitFailure) → Task 7 Step 2 ✓
- Prompt Edit 3 (exception note) → Task 7 Step 3 ✓
- Engine `lastUserMsg` field → Task 1 ✓
- `normalizeMsg` helper → Task 2 ✓
- `containsInitFailureSignal` helper → Task 3 ✓
- `containsScanAllSignal` helper → Task 4 ✓
- Gate 1 (symptom specificity) → Task 5 ✓
- Gate 2 (instance disambiguation) → Task 6 ✓
- All 7 scenario tests from spec → Tasks 5 & 6 ✓
- `TestNormalizeMsg`, `TestContainsInitFailureSignal`, `TestContainsScanAllSignal` → Tasks 2, 3, 4 ✓
