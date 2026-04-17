# W4: 诊断链 + Tool Description 补全 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add SSH diagnostic chain and init failure diagnostic chain with a dedicated diagnosis engine, register 2 new API tools, and update all existing tool descriptions with API parameter constraints.

**Architecture:** Diagnosis engine is a separate `internal/diagnosis/` package. Steps evaluate results to either Continue (next step) or Conclude (stop with diagnosis + suggestion). Integrates as meta-tools in the ReAct engine, same pattern as workflows. Each step calls an API tool via the shared `ToolExecutor`.

**Tech Stack:** Go 1.22, testify/assert, go-openai (tool registry)

---

### Task 1: Diagnosis Engine Types

**Files:**
- Create: `internal/diagnosis/types.go`
- Test: `internal/diagnosis/engine_test.go` (test Context/Verdict types)

**Step 1: Write the types file**

```go
package diagnosis

// VerdictAction determines what happens after a diagnosis step evaluates.
type VerdictAction int

const (
	// Continue moves to the next step.
	Continue VerdictAction = iota
	// Conclude stops the chain with a diagnosis result.
	Conclude
)

// Verdict is returned by a step's Evaluate function.
type Verdict struct {
	Action     VerdictAction
	Conclusion string // human-readable diagnosis (only for Conclude)
	Suggestion string // recommended next action (only for Conclude)
}

// Step defines one check in a diagnostic chain.
type Step struct {
	Name      string
	Tool      string // API action to call
	BuildArgs func(dCtx *Context) (map[string]any, error)
	Evaluate  func(result map[string]any, dCtx *Context) Verdict
}

// Chain defines a complete diagnostic chain.
type Chain struct {
	Name        string
	Description string
	Steps       []Step
	// Fallback is returned when all steps pass without concluding.
	Fallback Verdict
}

// Context accumulates state during diagnosis execution.
type Context struct {
	Params      map[string]any
	StepResults map[string]map[string]any
}

// NewContext creates a diagnosis context with the given initial parameters.
func NewContext(params map[string]any) *Context {
	if params == nil {
		params = make(map[string]any)
	}
	return &Context{
		Params:      params,
		StepResults: make(map[string]map[string]any),
	}
}

// Result returns the API result from a previous step, or nil.
func (c *Context) Result(stepName string) map[string]any {
	return c.StepResults[stepName]
}

// DiagResult is the final output of running a diagnostic chain.
type DiagResult struct {
	Success    bool          `json:"success"`
	Conclusion string        `json:"conclusion"`
	Suggestion string        `json:"suggestion"`
	StoppedAt  string        `json:"stopped_at,omitempty"`
	Steps      []StepSummary `json:"steps"`
}

// StepSummary records one step's outcome.
type StepSummary struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // checked / concluded / failed
	Message string `json:"message,omitempty"`
}

// DiagEvent is emitted during diagnosis execution for UI/CLI display.
type DiagEvent struct {
	StepName  string
	StepIndex int
	Total     int
	Status    string // running / checked / concluded / failed
	Tool      string
	Args      map[string]any
	Message   string
}
```

**Step 2: Write type tests**

In `internal/diagnosis/engine_test.go`:

```go
package diagnosis

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContext_NewAndResult(t *testing.T) {
	params := map[string]any{"UHostId": "uhost-abc"}
	dCtx := NewContext(params)

	assert.Equal(t, "uhost-abc", dCtx.Params["UHostId"])
	assert.NotNil(t, dCtx.StepResults)

	dCtx.StepResults["check_state"] = map[string]any{"State": "Running"}
	result := dCtx.Result("check_state")
	assert.Equal(t, "Running", result["State"])

	assert.Nil(t, dCtx.Result("nonexistent"))
}

func TestContext_NilParams(t *testing.T) {
	dCtx := NewContext(nil)
	assert.NotNil(t, dCtx.Params)
	assert.NotNil(t, dCtx.StepResults)
}
```

**Step 3: Run tests**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -run TestContext -v`
Expected: PASS

**Step 4: Commit**

```bash
git add internal/diagnosis/types.go internal/diagnosis/engine_test.go
git commit -m "feat(diagnosis): add types for diagnostic chain engine"
```

---

### Task 2: Diagnosis Engine

**Files:**
- Create: `internal/diagnosis/engine.go`
- Modify: `internal/diagnosis/engine_test.go`

**Step 1: Write engine tests**

Append to `internal/diagnosis/engine_test.go`:

```go
// engine_test.go continued — add these imports/helpers/tests

import (
	"context"
	"fmt"
	// existing imports...
)

// --- Mock Executor ---

type mockExecutor struct {
	results map[string]map[string]any
	calls   []executorCall
	failOn  string
}

type executorCall struct {
	action string
	args   map[string]any
}

func (m *mockExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, executorCall{action, args})
	if action == m.failOn {
		return nil, fmt.Errorf("API error: %s failed", action)
	}
	if r, ok := m.results[action]; ok {
		return r, nil
	}
	return map[string]any{"RetCode": 0}, nil
}

func collectEvents() (func(DiagEvent), *[]DiagEvent) {
	var events []DiagEvent
	return func(ev DiagEvent) { events = append(events, ev) }, &events
}

// --- Engine Tests ---

func TestEngine_Run_ConcludeAtFirstStep(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Stopped"},
			},
		},
	}}
	onStep, events := collectEvents()

	chain := &Chain{
		Name: "TestDiag",
		Steps: []Step{
			{
				Name: "检查状态",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostIds": []any{dCtx.Params["UHostId"]}}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					hosts, _ := result["UHostSet"].([]any)
					if len(hosts) == 0 {
						return Verdict{Action: Conclude, Conclusion: "实例不存在", Suggestion: "请检查实例 ID"}
					}
					host, _ := hosts[0].(map[string]any)
					state, _ := host["State"].(string)
					if state == "Stopped" {
						return Verdict{Action: Conclude, Conclusion: "实例已关机", Suggestion: "需要先开机才能 SSH 连接"}
					}
					return Verdict{Action: Continue}
				},
			},
			{
				Name: "检查端口",
				Tool: "DescribeCompShareSoftwarePort",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "未找到明确原因", Suggestion: "请检查本地网络"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "实例已关机", result.Conclusion)
	assert.Equal(t, "需要先开机才能 SSH 连接", result.Suggestion)
	assert.Equal(t, "检查状态", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)

	// Only first tool called
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)

	// Events emitted
	assert.NotEmpty(t, *events)
}

func TestEngine_Run_AllStepsPass_Fallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"ToolA": {"ok": true},
		"ToolB": {"ok": true},
	}}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "TestDiag",
		Steps: []Step{
			{
				Name: "step_a", Tool: "ToolA",
				BuildArgs: func(dCtx *Context) (map[string]any, error) { return map[string]any{}, nil },
				Evaluate:  func(result map[string]any, dCtx *Context) Verdict { return Verdict{Action: Continue} },
			},
			{
				Name: "step_b", Tool: "ToolB",
				BuildArgs: func(dCtx *Context) (map[string]any, error) { return map[string]any{}, nil },
				Evaluate:  func(result map[string]any, dCtx *Context) Verdict { return Verdict{Action: Continue} },
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "未发现问题", Suggestion: "请联系客服"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "未发现问题", result.Conclusion)
	assert.Equal(t, "请联系客服", result.Suggestion)
	assert.Len(t, result.Steps, 2)
	assert.Equal(t, "checked", result.Steps[0].Status)
	assert.Equal(t, "checked", result.Steps[1].Status)
}

func TestEngine_Run_ToolFailure(t *testing.T) {
	executor := &mockExecutor{failOn: "ToolA"}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "TestDiag",
		Steps: []Step{
			{
				Name: "step_a", Tool: "ToolA",
				BuildArgs: func(dCtx *Context) (map[string]any, error) { return map[string]any{}, nil },
				Evaluate:  func(result map[string]any, dCtx *Context) Verdict { return Verdict{Action: Continue} },
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "fallback"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "step_a", result.StoppedAt)
	assert.Contains(t, result.Conclusion, "检查失败")
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "failed", result.Steps[0].Status)
}

func TestEngine_Run_BuildArgsError(t *testing.T) {
	executor := &mockExecutor{}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "TestDiag",
		Steps: []Step{
			{
				Name: "step_a", Tool: "ToolA",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return nil, fmt.Errorf("missing UHostId")
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict { return Verdict{Action: Continue} },
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "fallback"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Conclusion, "参数构建失败")
	assert.Empty(t, executor.calls)
}

func TestEngine_Run_ContextCancelled(t *testing.T) {
	executor := &mockExecutor{}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "TestDiag",
		Steps: []Step{
			{
				Name: "step_a", Tool: "ToolA",
				BuildArgs: func(dCtx *Context) (map[string]any, error) { return map[string]any{}, nil },
				Evaluate:  func(result map[string]any, dCtx *Context) Verdict { return Verdict{Action: Continue} },
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "fallback"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(ctx, chain, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Conclusion, "已取消")
}

func TestEngine_Run_StepResultsAccumulate(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"ToolA": {"value": "from_a"},
		"ToolB": {"value": "from_b"},
	}}
	onStep, _ := collectEvents()

	var capturedCtx *Context
	chain := &Chain{
		Name: "TestDiag",
		Steps: []Step{
			{
				Name: "step_a", Tool: "ToolA",
				BuildArgs: func(dCtx *Context) (map[string]any, error) { return map[string]any{}, nil },
				Evaluate:  func(result map[string]any, dCtx *Context) Verdict { return Verdict{Action: Continue} },
			},
			{
				Name: "step_b", Tool: "ToolB",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					// Verify step_a result is accessible
					prev := dCtx.Result("step_a")
					if prev == nil {
						return nil, fmt.Errorf("step_a result missing")
					}
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					capturedCtx = dCtx
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "done"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.NotNil(t, capturedCtx)
	assert.Equal(t, "from_a", capturedCtx.Result("step_a")["value"])
	assert.Equal(t, "from_b", capturedCtx.Result("step_b")["value"])
}
```

**Step 2: Run tests to verify they fail**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -v`
Expected: FAIL — `NewEngine` not defined

**Step 3: Write the engine**

```go
package diagnosis

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/tools"
)

// Engine executes diagnostic chains step by step with evaluate-and-branch semantics.
type Engine struct {
	executor tools.ToolExecutor
	onStep   func(DiagEvent)
}

// NewEngine creates a diagnosis engine.
func NewEngine(executor tools.ToolExecutor, onStep func(DiagEvent)) *Engine {
	return &Engine{executor: executor, onStep: onStep}
}

// Run executes a diagnostic chain with the given initial parameters.
// Returns a DiagResult — Go errors are only for truly unexpected failures.
func (e *Engine) Run(ctx context.Context, chain *Chain, params map[string]any) (*DiagResult, error) {
	dCtx := NewContext(params)
	total := len(chain.Steps)
	result := &DiagResult{Steps: make([]StepSummary, 0, total)}

	for i, step := range chain.Steps {
		if err := ctx.Err(); err != nil {
			result.Conclusion = fmt.Sprintf("诊断已取消: %v", err)
			return result, nil
		}

		// Build args
		args, err := step.BuildArgs(dCtx)
		if err != nil {
			e.emit(step.Name, i, total, "failed", step.Tool, nil, err.Error())
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
			result.StoppedAt = step.Name
			result.Conclusion = fmt.Sprintf("步骤「%s」参数构建失败: %v", step.Name, err)
			return result, nil
		}

		// Execute tool
		e.emit(step.Name, i, total, "running", step.Tool, args, "")

		apiResult, err := e.executor.Execute(ctx, step.Tool, args)
		if err != nil {
			e.emit(step.Name, i, total, "failed", step.Tool, nil, err.Error())
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
			result.StoppedAt = step.Name
			result.Conclusion = fmt.Sprintf("步骤「%s」检查失败: %v", step.Name, err)
			return result, nil
		}

		dCtx.StepResults[step.Name] = apiResult

		// Evaluate result
		verdict := step.Evaluate(apiResult, dCtx)

		if verdict.Action == Conclude {
			e.emit(step.Name, i, total, "concluded", step.Tool, nil, verdict.Conclusion)
			result.Steps = append(result.Steps, StepSummary{
				Name: step.Name, Status: "concluded", Message: verdict.Conclusion,
			})
			result.Success = true
			result.StoppedAt = step.Name
			result.Conclusion = verdict.Conclusion
			result.Suggestion = verdict.Suggestion
			return result, nil
		}

		// Continue
		e.emit(step.Name, i, total, "checked", step.Tool, nil, "")
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "checked"})
	}

	// All steps passed without concluding — use fallback
	result.Success = true
	result.Conclusion = chain.Fallback.Conclusion
	result.Suggestion = chain.Fallback.Suggestion
	return result, nil
}

func (e *Engine) emit(name string, idx, total int, status, tool string, args map[string]any, msg string) {
	if e.onStep != nil {
		e.onStep(DiagEvent{
			StepName: name, StepIndex: idx, Total: total,
			Status: status, Tool: tool, Args: args, Message: msg,
		})
	}
}
```

**Step 4: Run tests**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/diagnosis/engine.go internal/diagnosis/engine_test.go
git commit -m "feat(diagnosis): add diagnosis engine with evaluate/conclude semantics"
```

---

### Task 3: Diagnosis Registry

**Files:**
- Create: `internal/diagnosis/registry.go`
- Modify: `internal/diagnosis/engine_test.go` (add registry tests)

**Step 1: Write registry tests**

Append to `engine_test.go`:

```go
func TestRegistry_IsDiagnosisTool(t *testing.T) {
	assert.True(t, IsDiagnosisTool("DiagnoseSSH"))
	assert.True(t, IsDiagnosisTool("DiagnoseInitFailure"))
	assert.False(t, IsDiagnosisTool("DescribeCompShareInstance"))
	assert.False(t, IsDiagnosisTool("CreateInstanceWorkflow"))
}

func TestRegistry_GetChain(t *testing.T) {
	chain, ok := GetChain("DiagnoseSSH")
	assert.True(t, ok)
	assert.Equal(t, "DiagnoseSSH", chain.Name)
	assert.NotEmpty(t, chain.Steps)

	_, ok = GetChain("NonExistent")
	assert.False(t, ok)
}
```

**Step 2: Run tests to verify they fail**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -run TestRegistry -v`
Expected: FAIL — `IsDiagnosisTool` not defined

**Step 3: Write registry**

```go
package diagnosis

// chainRegistry maps diagnosis tool names to their factory functions.
var chainRegistry = map[string]func() *Chain{
	"DiagnoseSSH":         SSHFailureChain,
	"DiagnoseInitFailure": InitFailureChain,
}

// IsDiagnosisTool reports whether the given action name is a registered diagnosis chain.
func IsDiagnosisTool(action string) bool {
	_, ok := chainRegistry[action]
	return ok
}

// GetChain returns a fresh Chain for the named diagnosis.
func GetChain(action string) (*Chain, bool) {
	factory, ok := chainRegistry[action]
	if !ok {
		return nil, false
	}
	return factory(), true
}
```

Note: This will fail to compile until Task 4 and 5 create `SSHFailureChain` and `InitFailureChain`. Write placeholder stubs in the same step:

Create `internal/diagnosis/ssh_failure.go` (stub):

```go
package diagnosis

// SSHFailureChain returns the diagnostic chain for SSH connection failures.
// Implemented in Task 4.
func SSHFailureChain() *Chain {
	return &Chain{Name: "DiagnoseSSH", Steps: []Step{}, Fallback: Verdict{Action: Conclude, Conclusion: "placeholder"}}
}
```

Create `internal/diagnosis/init_failure.go` (stub):

```go
package diagnosis

// InitFailureChain returns the diagnostic chain for instance initialization failures.
// Implemented in Task 5.
func InitFailureChain() *Chain {
	return &Chain{Name: "DiagnoseInitFailure", Steps: []Step{}, Fallback: Verdict{Action: Conclude, Conclusion: "placeholder"}}
}
```

**Step 4: Run tests**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/diagnosis/registry.go internal/diagnosis/ssh_failure.go internal/diagnosis/init_failure.go
git commit -m "feat(diagnosis): add chain registry with stubs for SSH and init failure"
```

---

### Task 4: SSH Diagnostic Chain

**Files:**
- Modify: `internal/diagnosis/ssh_failure.go`
- Create: `internal/diagnosis/ssh_failure_test.go`

**Step 1: Write SSH chain tests**

```go
package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSSHChain_Stopped(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Stopped"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "关机")
	assert.Contains(t, result.Suggestion, "开机")
	assert.Equal(t, "检查实例状态", result.StoppedAt)
	assert.Len(t, executor.calls, 1) // only DescribeCompShareInstance called
}

func TestSSHChain_Installing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Install"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化")
	assert.Len(t, executor.calls, 1)
}

func TestSSHChain_InstallFail(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "InstallFail"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化失败")
	assert.Contains(t, result.Suggestion, "删除重建")
	assert.Len(t, executor.calls, 1)
}

func TestSSHChain_Running_NoSSHPort(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
				// No SSH port
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "SSH 端口未开放")
	assert.Len(t, executor.calls, 2)
}

func TestSSHChain_Running_HighCPU(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "SSH", "Port": float64(22)},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "uhost_cpu_used",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(98.5)},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "资源")
	assert.Contains(t, result.Suggestion, "重启")
	assert.Len(t, executor.calls, 3)
}

func TestSSHChain_Running_AllNormal_Fallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "SSH", "Port": float64(22)},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "uhost_cpu_used",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(35.0)},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未发现")
	assert.Contains(t, result.Suggestion, "JupyterLab")
	assert.Len(t, executor.calls, 3) // all 3 steps checked
	assert.Len(t, result.Steps, 3)
	for _, s := range result.Steps {
		assert.Equal(t, "checked", s.Status)
	}
}

func TestSSHChain_InstanceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{}, // empty
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
}
```

**Step 2: Run tests to verify they fail**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -run TestSSHChain -v`
Expected: FAIL — `SSHFailureChain` returns placeholder

**Step 3: Implement SSH diagnostic chain**

Replace `internal/diagnosis/ssh_failure.go`:

```go
package diagnosis

// SSHFailureChain returns the 3-step diagnostic chain for SSH connection failures.
// Flow: check instance state → check SSH port → check resource usage → fallback.
func SSHFailureChain() *Chain {
	return &Chain{
		Name:        "DiagnoseSSH",
		Description: "诊断 SSH 连接失败：检查实例状态 → 检查 SSH 端口 → 检查资源使用 → 兜底建议",
		Steps: []Step{
			stepCheckInstanceState(),
			stepCheckSSHPort(),
			stepCheckResourceUsage(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "未发现明确的 SSH 连接问题。实例运行正常，SSH 端口已开放，资源使用正常。",
			Suggestion: "请检查您本地的网络连接，或尝试使用 JupyterLab 网页终端作为替代方案。SSH 登录命令可在实例详情中找到。",
		},
	}
}

func stepCheckInstanceState() Step {
	return Step{
		Name: "检查实例状态",
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{dCtx.Params["UHostId"]},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			hosts, _ := result["UHostSet"].([]any)
			if len(hosts) == 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "未找到该实例，可能已被释放或 ID 输入有误。",
					Suggestion: "请使用 DescribeCompShareInstance 查看当前实例列表，确认实例 ID。",
				}
			}
			host, _ := hosts[0].(map[string]any)
			state, _ := host["State"].(string)

			switch state {
			case "Stopped":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前处于关机状态，无法进行 SSH 连接。",
					Suggestion: "需要先开机才能 SSH 连接。可以使用 StartInstanceWorkflow 开机。",
				}
			case "Install":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在初始化中，尚未就绪。初始化通常需要 2-3 分钟。",
					Suggestion: "请耐心等待初始化完成后再尝试 SSH 连接。",
				}
			case "InstallFail":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例初始化失败，无法正常使用。",
					Suggestion: "建议到控制台删除该实例后重新创建。如果问题反复出现，请联系客服。",
				}
			case "Starting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在启动中，请稍等片刻。",
					Suggestion: "启动通常需要 1-2 分钟，完成后即可 SSH 连接。",
				}
			case "Stopping":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在关机中，无法 SSH 连接。",
					Suggestion: "请等待关机完成后，再使用 StartInstanceWorkflow 重新开机。",
				}
			case "Rebooting":
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例正在重启中，请稍等片刻。",
					Suggestion: "重启通常需要 1-2 分钟，完成后即可 SSH 连接。",
				}
			case "Running":
				return Verdict{Action: Continue}
			default:
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例当前状态为「" + state + "」，可能处于异常状态。",
					Suggestion: "请到控制台查看实例详情或联系客服。",
				}
			}
		},
	}
}

func stepCheckSSHPort() Step {
	return Step{
		Name: "检查 SSH 端口",
		Tool: "DescribeCompShareSoftwarePort",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			ports, _ := result["SoftwarePort"].([]any)
			for _, p := range ports {
				port, _ := p.(map[string]any)
				software, _ := port["Software"].(string)
				if software == "SSH" {
					return Verdict{Action: Continue}
				}
			}
			return Verdict{
				Action:     Conclude,
				Conclusion: "SSH 端口未开放。平台当前软件端口列表中没有 SSH 服务。",
				Suggestion: "请联系客服开放 SSH 端口，或使用 JupyterLab 网页终端替代。",
			}
		},
	}
}

func stepCheckResourceUsage() Step {
	return Step{
		Name: "检查资源使用",
		Tool: "GetCompShareInstanceMonitor",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{dCtx.Params["UHostId"]},
			}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			cpuUsage, memUsage := extractLatestMetrics(result)
			const threshold = 95.0

			if cpuUsage >= threshold || memUsage >= threshold {
				detail := ""
				if cpuUsage >= threshold {
					detail += fmt.Sprintf("CPU 使用率 %.1f%%", cpuUsage)
				}
				if memUsage >= threshold {
					if detail != "" {
						detail += "，"
					}
					detail += fmt.Sprintf("内存使用率 %.1f%%", memUsage)
				}
				return Verdict{
					Action:     Conclude,
					Conclusion: "实例资源耗尽：" + detail + "。系统资源不足可能导致 SSH 无法响应。",
					Suggestion: "建议通过控制台重启实例释放资源，或升级到更高配置。",
				}
			}
			return Verdict{Action: Continue}
		},
	}
}

// extractLatestMetrics gets the latest CPU and memory usage from monitor data.
func extractLatestMetrics(result map[string]any) (cpu, mem float64) {
	data, _ := result["Data"].(map[string]any)
	if data == nil {
		return 0, 0
	}
	list, _ := data["List"].([]any)
	if len(list) == 0 {
		return 0, 0
	}
	instance, _ := list[0].(map[string]any)
	metrics, _ := instance["Metrics"].([]any)

	for _, m := range metrics {
		metric, _ := m.(map[string]any)
		key, _ := metric["MetricKey"].(string)
		val := latestValue(metric)
		switch key {
		case "uhost_cpu_used":
			cpu = val
		case "cloudwatch_memory_usage":
			mem = val
		}
	}
	return cpu, mem
}

// latestValue extracts the most recent value from a metric's Results.
func latestValue(metric map[string]any) float64 {
	results, _ := metric["Results"].([]any)
	if len(results) == 0 {
		return 0
	}
	first, _ := results[0].(map[string]any)
	values, _ := first["Values"].([]any)
	if len(values) == 0 {
		return 0
	}
	last, _ := values[len(values)-1].(map[string]any)
	val, _ := last["Value"].(float64)
	return val
}
```

Note: add `"fmt"` to the ssh_failure.go imports.

**Step 4: Run tests**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -run TestSSHChain -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/diagnosis/ssh_failure.go internal/diagnosis/ssh_failure_test.go
git commit -m "feat(diagnosis): implement SSH failure diagnostic chain"
```

---

### Task 5: Init Failure Diagnostic Chain

**Files:**
- Modify: `internal/diagnosis/init_failure.go`
- Create: `internal/diagnosis/init_failure_test.go`

**Step 1: Write tests**

```go
package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInitFailureChain_InstallFail(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "InstallFail", "CompShareImageName": "PyTorch 2.1"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化失败")
	assert.Contains(t, result.Suggestion, "删除")
}

func TestInitFailureChain_Installing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Install"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化中")
}

func TestInitFailureChain_Running(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "正常运行")
}

func TestInitFailureChain_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
}
```

**Step 2: Run tests to verify they fail**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -run TestInitFailure -v`
Expected: FAIL — `InitFailureChain` returns placeholder

**Step 3: Implement init failure chain**

Replace `internal/diagnosis/init_failure.go`:

```go
package diagnosis

// InitFailureChain returns the diagnostic chain for instance initialization failures.
// Single-step: check instance state and provide guidance based on the current state.
func InitFailureChain() *Chain {
	return &Chain{
		Name:        "DiagnoseInitFailure",
		Description: "诊断实例初始化失败：检查实例状态并给出修复建议",
		Steps: []Step{
			{
				Name: "检查实例状态",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{
						"UHostIds": []any{dCtx.Params["UHostId"]},
					}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					hosts, _ := result["UHostSet"].([]any)
					if len(hosts) == 0 {
						return Verdict{
							Action:     Conclude,
							Conclusion: "未找到该实例，可能已被释放或 ID 输入有误。",
							Suggestion: "请使用 DescribeCompShareInstance 查看当前实例列表，确认实例 ID。",
						}
					}
					host, _ := hosts[0].(map[string]any)
					state, _ := host["State"].(string)
					imageName, _ := host["CompShareImageName"].(string)

					switch state {
					case "InstallFail":
						conclusion := "实例初始化失败。"
						if imageName != "" {
							conclusion = "实例初始化失败（镜像：" + imageName + "）。"
						}
						return Verdict{
							Action:     Conclude,
							Conclusion: conclusion + "可能原因包括：镜像异常、资源分配冲突、或平台临时问题。",
							Suggestion: "建议到控制台删除该实例后重新创建。如使用自定义/社区镜像，请尝试换用官方系统镜像。如问题反复出现，请联系客服。",
						}
					case "Install":
						return Verdict{
							Action:     Conclude,
							Conclusion: "实例仍在初始化中，尚未失败。初始化通常需要 2-3 分钟，部分镜像可能需要更长时间。",
							Suggestion: "请耐心等待。如果超过 10 分钟仍未完成，请联系客服。",
						}
					case "Running":
						return Verdict{
							Action:     Conclude,
							Conclusion: "实例已正常运行，初始化已成功完成。",
							Suggestion: "您可以正常使用实例。如遇到其他问题，请描述具体症状。",
						}
					default:
						return Verdict{
							Action:     Conclude,
							Conclusion: "实例当前状态为「" + state + "」，并非初始化失败。",
							Suggestion: "如果实例之前初始化失败后已被操作（如开机/关机），当前状态可能已改变。请描述具体问题以便进一步排查。",
						}
					}
				},
			},
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "无法确定初始化状态。",
			Suggestion: "请联系客服获取帮助。",
		},
	}
}
```

**Step 4: Run tests**

Run: `cd F:/compshare-agent && go test ./internal/diagnosis/... -run TestInitFailure -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/diagnosis/init_failure.go internal/diagnosis/init_failure_test.go
git commit -m "feat(diagnosis): implement init failure diagnostic chain"
```

---

### Task 6: Register New API Tools + Diagnosis Meta-Tools

**Files:**
- Modify: `internal/tools/registry.go`

**Step 1: Run existing tests to establish baseline**

Run: `cd F:/compshare-agent && go test ./... -count=1`
Expected: ALL PASS (105 tests)

**Step 2: Add new tools to registry**

Add after the existing `DescribeCompShareImages` entry and before `// --- Workflow Meta-Tools ---`:

```go
// New API tools for diagnosis
{
    Type: openai.ToolTypeFunction,
    Function: &openai.FunctionDefinition{
        Name:        "DescribeCompShareSoftwarePort",
        Description: "查询平台支持的软件及其端口映射列表（SSH、JupyterLab、FileBrowser 等）。用于诊断端口连通性问题。仅需 Region 参数（自动填充）。",
        Parameters: map[string]any{
            "type":       "object",
            "properties": map[string]any{},
            "required":   []string{},
        },
    },
},
{
    Type: openai.ToolTypeFunction,
    Function: &openai.FunctionDefinition{
        Name:        "GetCompShareInstanceMonitor",
        Description: "获取实例监控数据（CPU/内存/GPU/显存使用率等）。必须传 UHostIds。查多实例时仅返回最近 60 秒基础指标；查单实例可传 StartTime/EndTime 获取扩展指标（网络、磁盘）。",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "UHostIds": map[string]any{
                    "type":        "array",
                    "items":       map[string]any{"type": "string"},
                    "description": "实例 ID 列表（必填）",
                },
                "StartTime": map[string]any{
                    "type":        "integer",
                    "description": "查询起始时间（Unix 时间戳），仅单实例查询时有效",
                },
                "EndTime": map[string]any{
                    "type":        "integer",
                    "description": "查询结束时间（Unix 时间戳），仅单实例查询时有效",
                },
            },
            "required": []string{"UHostIds"},
        },
    },
},
```

Add diagnosis meta-tools after the existing `// --- Workflow Meta-Tools ---` block:

```go
// --- Diagnosis Meta-Tools ---
{
    Type: openai.ToolTypeFunction,
    Function: &openai.FunctionDefinition{
        Name:        "DiagnoseSSH",
        Description: "诊断 SSH 连接失败。自动执行：检查实例状态 → 检查 SSH 端口 → 检查资源使用 → 给出结论和建议。用户反馈 SSH 连不上、连接超时、连接被拒时使用。",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "UHostId": map[string]any{
                    "type":        "string",
                    "description": "要诊断的实例 ID",
                },
            },
            "required": []string{"UHostId"},
        },
    },
},
{
    Type: openai.ToolTypeFunction,
    Function: &openai.FunctionDefinition{
        Name:        "DiagnoseInitFailure",
        Description: "诊断实例初始化失败。检查实例当前状态并给出修复建议。用户反馈创建失败、初始化失败、实例异常时使用。",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "UHostId": map[string]any{
                    "type":        "string",
                    "description": "要诊断的实例 ID",
                },
            },
            "required": []string{"UHostId"},
        },
    },
},
```

**Step 3: Run tests**

Run: `cd F:/compshare-agent && go test ./... -count=1`
Expected: ALL PASS

**Step 4: Commit**

```bash
git add internal/tools/registry.go
git commit -m "feat(tools): register DescribeCompShareSoftwarePort, GetCompShareInstanceMonitor, and diagnosis meta-tools"
```

---

### Task 7: Update Existing Tool Descriptions with API Parameter Constraints

**Files:**
- Modify: `internal/tools/registry.go`

Cross-reference each registered tool with `docs/api/` and add critical parameter constraints to the `Description` field. The key constraints LLM needs to see:

**Step 1: Update tool descriptions**

Update each tool's `Description` in `internal/tools/registry.go`:

1. **DescribeCompShareInstance**: Add Zone format note.
   ```
   "查询用户的算力共享实例列表及详情。返回实例状态（Running/Stopped/Install/InstallFail/Starting/Stopping/Rebooting）、GPU 类型、IP、计费等。不传 UHostIds 查全部。Limit 最大 100。State 含义：Install=初始化中, InstallFail=初始化失败。"
   ```

2. **GetCompShareInstancePrice**: Add Zone format and Memory unit.
   ```
   "查询创建实例的价格。返回按量/包日/包月/抢占式等分项价格（实例、磁盘、镜像）。Zone 格式为 cn-wlcb-01。Memory 单位为 MB（如 65536 = 64GB）。不传 ChargeType 则返回所有计费方式的价格。"
   ```

3. **CheckCompShareResourceCapacity**: Add required params and format constraints.
   ```
   "检查 GPU 库存是否充足。Zone 必须为 cn-wlcb-01 格式。MachineType 固定传 G。MinimalCpuPlatform 传 Auto（或 Intel/Auto、Amd/Auto）。CompShareImageId 和 ChargeType 必填。Disks 至少包含一个系统盘，如 [{IsBoot:true, Type:CLOUD_SSD, Size:60}]。返回各 GPU/CPU/Memory 组合的可用性。"
   ```

4. **DescribeCompShareImages**: Add ImageType enum.
   ```
   "查询可用的算力共享镜像列表。ImageType 枚举：System（平台公共镜像）、Custom（自定义镜像）、App（应用镜像），不传返回全部。可按 Name、Author、Tag 筛选。返回 CompShareImageId 和 Name 等字段。"
   ```

**Step 2: Run tests**

Run: `cd F:/compshare-agent && go test ./... -count=1`
Expected: ALL PASS

**Step 3: Commit**

```bash
git add internal/tools/registry.go
git commit -m "docs(tools): add API parameter constraints to tool descriptions for LLM accuracy"
```

---

### Task 8: Engine Integration — Diagnosis Dispatch

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

**Step 1: Write engine integration tests**

Append to `internal/engine/engine_test.go`:

```go
func TestChat_DiagnosisTool_SSHStopped(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-diag-001", "State": "Stopped"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseSSH", `{"UHostId":"uhost-diag-001"}`),
		}},
		{Content: "诊断结果：实例已关机，需要先开机"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "SSH连不上 uhost-diag-001", onStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")

	// Executor should have been called by diagnosis engine
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")

	// Should have diagnosis-related events
	hasDiagCall := false
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DiagnoseSSH" {
			hasDiagCall = true
		}
	}
	assert.True(t, hasDiagCall)

	// Tool result fed to LLM should be valid JSON with conclusion
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var result map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &result)
	assert.NoError(t, err)
	assert.Equal(t, true, result["success"])
	assert.Contains(t, result["conclusion"], "关机")
}

func TestChat_DiagnosisTool_InitFailure(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-fail-001", "State": "InstallFail", "CompShareImageName": "PyTorch 2.1"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-fail-001"}`),
		}},
		{Content: "初始化失败，建议删除重建"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "实例初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "初始化失败")
}

func TestChat_DiagnosisTool_ArgsFiltered(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-diag-002", "State": "Running"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-diag-002","evil":"injection"}`),
		}},
		{Content: "done"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "test", onStep)

	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DiagnoseInitFailure" {
			assert.NotContains(t, ev.Args, "evil")
			assert.Contains(t, ev.Args, "UHostId")
		}
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd F:/compshare-agent && go test ./internal/engine/... -run TestChat_Diagnosis -v`
Expected: FAIL — diagnosis dispatch not implemented

**Step 3: Add diagnosis dispatch to engine.go**

Add import:
```go
"github.com/compshare-agent/internal/diagnosis"
```

Add after the workflow dispatch block in `executeTool` (after `if workflow.IsWorkflowTool(action) {` block, before `// External API tools: security check`):

```go
// Diagnosis meta-tools → delegate to diagnosis engine.
if diagnosis.IsDiagnosisTool(action) {
    args = filterAllowedParams(action, args)
    onStep(StepEvent{Type: StepToolCall, Action: action, Args: args})
    return e.executeDiagnosis(ctx, action, args, onStep)
}
```

Add the `executeDiagnosis` method:

```go
// executeDiagnosis runs a diagnostic chain and returns the result as JSON.
func (e *Engine) executeDiagnosis(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	chain, ok := diagnosis.GetChain(action)
	if !ok {
		msg := fmt.Sprintf("未知的诊断链: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
	}

	diagEngine := diagnosis.NewEngine(e.executor, func(ev diagnosis.DiagEvent) {
		eventType := StepToolCall
		if ev.Status == "failed" {
			eventType = StepError
		}
		onStep(StepEvent{
			Type:    eventType,
			Action:  ev.Tool,
			Args:    ev.Args,
			Message: fmt.Sprintf("[诊断 %d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status),
		})
	})

	result, err := diagEngine.Run(ctx, chain, args)
	if err != nil {
		msg := fmt.Sprintf("诊断执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Message: msg})
		return msg
	}

	b, _ := json.Marshal(result)
	return string(b)
}
```

**Step 4: Run tests**

Run: `cd F:/compshare-agent && go test ./... -count=1 -v 2>&1 | tail -20`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): integrate diagnosis chain dispatch into ReAct loop"
```

---

### Task 9: Update System Prompt for Diagnosis

**Files:**
- Modify: `internal/prompt/builder.go`
- Modify: `internal/prompt/builder_test.go`

**Step 1: Update system prompt template**

In `builder.go`, update the `diagnosis` line in `## 行为规则`:

From:
```
- diagnosis：用户报告了问题 → 按诊断流程逐步排查
```
To:
```
- diagnosis：用户报告了问题 → 使用诊断工具自动排查：
  - SSH 连不上/超时/被拒 → 调用 DiagnoseSSH
  - 创建失败/初始化失败 → 调用 DiagnoseInitFailure
  - 其他问题 → 先查实例状态（DescribeCompShareInstance），结合知识给建议
```

**Step 2: Update prompt test**

In `builder_test.go`, add a test (or update existing) verifying the new diagnosis instructions appear:

```go
func TestBuildSystem_ContainsDiagnosis(t *testing.T) {
	prompt := BuildSystem("test context")
	assert.Contains(t, prompt, "DiagnoseSSH")
	assert.Contains(t, prompt, "DiagnoseInitFailure")
}
```

**Step 3: Run tests**

Run: `cd F:/compshare-agent && go test ./internal/prompt/... -v`
Expected: ALL PASS

**Step 4: Commit**

```bash
git add internal/prompt/builder.go internal/prompt/builder_test.go
git commit -m "feat(prompt): add diagnosis tool routing instructions to system prompt"
```

---

### Task 10: Run Full Test Suite + Final Verification

**Step 1: Run all tests**

Run: `cd F:/compshare-agent && go test ./... -count=1 -v`
Expected: ALL PASS, total should be ~130+ tests (105 existing + ~25 new)

**Step 2: Verify build**

Run: `cd F:/compshare-agent && go build ./...`
Expected: clean build, no errors

**Step 3: Verify no regressions by running specific W1-W3 tests**

Run: `cd F:/compshare-agent && go test ./internal/engine/... -v -count=1`
Run: `cd F:/compshare-agent && go test ./internal/workflow/... -v -count=1`
Run: `cd F:/compshare-agent && go test ./internal/knowledge/... -v -count=1`
Expected: ALL PASS

**Step 4: Final commit if any cleanup needed**

---

## Summary of New/Modified Files

| File | Action | Description |
|------|--------|-------------|
| `internal/diagnosis/types.go` | Create | Verdict, Step, Chain, Context, DiagResult, DiagEvent |
| `internal/diagnosis/engine.go` | Create | Diagnosis engine: evaluate → branch/conclude |
| `internal/diagnosis/registry.go` | Create | Chain registry (IsDiagnosisTool, GetChain) |
| `internal/diagnosis/ssh_failure.go` | Create | SSH diagnostic chain (3 steps) |
| `internal/diagnosis/init_failure.go` | Create | Init failure diagnostic chain (1 step) |
| `internal/diagnosis/engine_test.go` | Create | Engine + registry tests |
| `internal/diagnosis/ssh_failure_test.go` | Create | SSH chain tests (7 cases) |
| `internal/diagnosis/init_failure_test.go` | Create | Init failure chain tests (4 cases) |
| `internal/tools/registry.go` | Modify | Add 2 API tools + 2 diagnosis meta-tools + update all descriptions |
| `internal/engine/engine.go` | Modify | Add diagnosis dispatch (IsDiagnosisTool → executeDiagnosis) |
| `internal/engine/engine_test.go` | Modify | Add 3 diagnosis integration tests |
| `internal/prompt/builder.go` | Modify | Add diagnosis routing to system prompt |
| `internal/prompt/builder_test.go` | Modify | Add diagnosis prompt test |
