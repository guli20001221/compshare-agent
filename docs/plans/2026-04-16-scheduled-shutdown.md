# Scheduled Shutdown Workflow Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add SetStopSchedulerWorkflow and CancelStopSchedulerWorkflow to let users set/cancel timed auto-shutdown for GPU instances.

**Architecture:** Two independent 3-step workflows following the existing Stop/Start pattern. Time conversion (AfterMinutes or ShutdownAt → unix timestamp) is handled in workflow code, not by the LLM. Tool schema exposes both workflows with human-friendly time parameters.

**Tech Stack:** Go, existing workflow engine, stretchr/testify for tests.

---

### Task 1: Implement resolveShutdownTime helper + tests

**Files:**
- Create: `internal/workflow/scheduler.go`
- Create: `internal/workflow/scheduler_test.go`

**Step 1: Write the failing tests**

In `internal/workflow/scheduler_test.go`:

```go
package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveShutdownTime_AfterMinutes(t *testing.T) {
	before := time.Now().Unix()
	ts, display, err := resolveShutdownTime(map[string]any{"AfterMinutes": float64(60)})
	after := time.Now().Unix()

	assert.NoError(t, err)
	assert.InDelta(t, before+3600, ts, float64(after-before+1))
	assert.Contains(t, display, "北京时间")
	assert.Contains(t, display, "约 1 小时后")
}

func TestResolveShutdownTime_ShutdownAt_WithTimezone(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	ts, display, err := resolveShutdownTime(map[string]any{"ShutdownAt": future})

	assert.NoError(t, err)
	assert.Greater(t, ts, time.Now().Unix())
	assert.Contains(t, display, "北京时间")
}

func TestResolveShutdownTime_ShutdownAt_NoTimezone(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	future := time.Now().In(loc).Add(2 * time.Hour).Format("2006-01-02 15:04")
	ts, _, err := resolveShutdownTime(map[string]any{"ShutdownAt": future})

	assert.NoError(t, err)
	assert.Greater(t, ts, time.Now().Unix())
}

func TestResolveShutdownTime_BothProvided_Error(t *testing.T) {
	_, _, err := resolveShutdownTime(map[string]any{
		"AfterMinutes": float64(60),
		"ShutdownAt":   "2026-04-16 23:00",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "不能同时指定")
}

func TestResolveShutdownTime_NeitherProvided_Error(t *testing.T) {
	_, _, err := resolveShutdownTime(map[string]any{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "请指定关机时间")
}

func TestResolveShutdownTime_AfterMinutes_TooSmall(t *testing.T) {
	_, _, err := resolveShutdownTime(map[string]any{"AfterMinutes": float64(3)})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "5 分钟")
}

func TestResolveShutdownTime_AfterMinutes_Fractional_Error(t *testing.T) {
	_, _, err := resolveShutdownTime(map[string]any{"AfterMinutes": float64(30.5)})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "正整数")
}

func TestResolveShutdownTime_TooSoon_Error(t *testing.T) {
	// ShutdownAt 2 minutes from now — should fail the >= now+5m check
	loc, _ := time.LoadLocation("Asia/Shanghai")
	soon := time.Now().In(loc).Add(2 * time.Minute).Format("2006-01-02 15:04")
	_, _, err := resolveShutdownTime(map[string]any{"ShutdownAt": soon})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "5 分钟")
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/workflow/ -run TestResolveShutdownTime -v`
Expected: FAIL — `resolveShutdownTime` undefined

**Step 3: Write implementation**

In `internal/workflow/scheduler.go`:

```go
package workflow

import (
	"fmt"
	"math"
	"time"
)

// shanghaiLoc is preloaded for time parsing without timezone.
var shanghaiLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic("failed to load Asia/Shanghai timezone: " + err.Error())
	}
	return loc
}()

// resolveShutdownTime converts user-supplied AfterMinutes or ShutdownAt into
// a unix timestamp and a human-readable display string.
//
// Rules:
//   - Exactly one of AfterMinutes or ShutdownAt must be provided.
//   - AfterMinutes must be a positive integer >= 5.
//   - ShutdownAt with timezone: parse as RFC3339. Without: parse as Asia/Shanghai.
//   - Result must be >= now + 5 minutes.
func resolveShutdownTime(params map[string]any) (unix int64, display string, err error) {
	_, hasAfter := params["AfterMinutes"]
	_, hasAt := params["ShutdownAt"]

	if !hasAfter && !hasAt {
		return 0, "", fmt.Errorf("请指定关机时间（AfterMinutes 或 ShutdownAt）")
	}
	if hasAfter && hasAt {
		return 0, "", fmt.Errorf("AfterMinutes 和 ShutdownAt 不能同时指定，请只传其中一个")
	}

	now := time.Now()
	var target time.Time

	if hasAfter {
		minutes := paramNum(params, "AfterMinutes", 0)
		if minutes != math.Floor(minutes) || minutes < 1 {
			return 0, "", fmt.Errorf("AfterMinutes 必须为正整数")
		}
		if minutes < 5 {
			return 0, "", fmt.Errorf("定时关机时间必须晚于当前时间至少 5 分钟（AfterMinutes >= 5）")
		}
		target = now.Add(time.Duration(minutes) * time.Minute)
	} else {
		s, _ := params["ShutdownAt"].(string)
		// Try RFC3339 first (has timezone info)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			// Fall back to local format in Asia/Shanghai
			t, err = time.ParseInLocation("2006-01-02 15:04", s, shanghaiLoc)
			if err != nil {
				return 0, "", fmt.Errorf("无法解析时间「%s」，请使用格式：2006-01-02 15:04 或 RFC3339", s)
			}
		}
		target = t
	}

	// Validate >= now + 5 minutes
	if target.Before(now.Add(5 * time.Minute)) {
		return 0, "", fmt.Errorf("定时关机时间必须晚于当前时间至少 5 分钟")
	}

	unix = target.Unix()
	display = formatShutdownDisplay(target, now)
	return unix, display, nil
}

// formatShutdownDisplay renders "2026-04-16 23:00（北京时间，约 2 小时后）".
func formatShutdownDisplay(target, now time.Time) string {
	local := target.In(shanghaiLoc)
	diff := target.Sub(now).Round(time.Minute)

	var relative string
	hours := int(diff.Hours())
	minutes := int(diff.Minutes()) % 60
	if hours > 0 && minutes > 0 {
		relative = fmt.Sprintf("约 %d 小时 %d 分钟后", hours, minutes)
	} else if hours > 0 {
		relative = fmt.Sprintf("约 %d 小时后", hours)
	} else {
		relative = fmt.Sprintf("约 %d 分钟后", int(diff.Minutes()))
	}

	return fmt.Sprintf("%s（北京时间，%s）", local.Format("2006-01-02 15:04"), relative)
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/workflow/ -run TestResolveShutdownTime -v`
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/workflow/scheduler.go internal/workflow/scheduler_test.go
git commit -m "feat(workflow): add resolveShutdownTime helper for scheduled shutdown"
```

---

### Task 2: Implement SetStopSchedulerWorkflow + tests

**Files:**
- Modify: `internal/workflow/scheduler.go` (append workflow definition)
- Modify: `internal/workflow/scheduler_test.go` (append workflow tests)

**Step 1: Write the failing tests**

Append to `internal/workflow/scheduler_test.go`:

```go
// --- SetStopSchedulerWorkflow tests ---

func schedulerMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
				"Zone":       "cn-wlcb-01",
			},
		}},
		"UpdateCompShareStopScheduler": {"UHostId": "uhost-xxx", "RetCode": float64(0)},
	}}
}

func TestSetStopScheduler_HappyPath(t *testing.T) {
	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)

	// Verify the scheduler API was called with a timestamp
	var schedArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "UpdateCompShareStopScheduler" {
			schedArgs = call.args
			break
		}
	}
	assert.NotNil(t, schedArgs)
	assert.Equal(t, "uhost-xxx", schedArgs["UHostId"])
	assert.Equal(t, "cn-wlcb-01", schedArgs["Zone"])
	ts, ok := schedArgs["SchedulerStopTime"].(int64)
	assert.True(t, ok)
	assert.Greater(t, ts, time.Now().Unix())
}

func TestSetStopScheduler_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未找到")
}

func TestSetStopScheduler_NotRunning(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-xxx", "State": "Stopped", "ChargeType": "Dynamic"},
		}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未运行")
}

func TestSetStopScheduler_SpotRejected(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-xxx", "State": "Running", "ChargeType": "Spot"},
		}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "抢占式")
}

func TestSetStopScheduler_ConfirmShowsTime(t *testing.T) {
	executor := schedulerMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	shutdownTime, ok := capturedArgs["shutdownTime"].(string)
	assert.True(t, ok)
	assert.Contains(t, shutdownTime, "北京时间")
}

func TestSetStopScheduler_ConfirmDenied(t *testing.T) {
	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认设置", result.StoppedAt)
}

func TestSetStopScheduler_BadTime(t *testing.T) {
	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(3), // < 5 minutes
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "5 分钟")
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/workflow/ -run TestSetStopScheduler -v`
Expected: FAIL — `SetStopSchedulerDef` undefined

**Step 3: Write implementation**

Append to `internal/workflow/scheduler.go`:

```go
// SetStopSchedulerDef returns the 3-step workflow for setting scheduled shutdown.
func SetStopSchedulerDef() *Definition {
	return &Definition{
		Name:        "SetStopSchedulerWorkflow",
		Description: "查询实例 → 确认设置 → 设置定时关机",
		Steps: []Step{
			stepQueryForScheduler(),
			stepConfirmScheduler(),
			stepSetScheduler(),
		},
	}
}

func stepQueryForScheduler() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			switch state {
			case "":
				return false, "未找到该实例。"
			case "Running":
				// Check for Spot instances
				chargeType := extractField(result, "ChargeType")
				if chargeType == "Spot" {
					return false, "抢占式实例不支持定时关机。"
				}
				return true, ""
			default:
				return false, "实例当前未运行（状态：" + state + "），无需设置定时关机。"
			}
		},
	}
}

func stepConfirmScheduler() Step {
	return Step{
		Name: "确认设置",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			_, display, err := resolveShutdownTime(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["shutdownTime"] = display
			return summary, nil
		},
	}
}

func stepSetScheduler() Step {
	return Step{
		Name: "设置定时关机",
		Type: StepToolCall,
		Tool: "UpdateCompShareStopScheduler",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			ts, _, err := resolveShutdownTime(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"Zone":              extractInstanceZone(wfCtx.Result("查询实例"), defaultZone),
				"UHostId":           wfCtx.Params["UHostId"],
				"SchedulerStopTime": ts,
			}, nil
		},
	}
}

// extractField returns a string field from the first UHostSet entry.
func extractField(result map[string]any, key string) string {
	if result == nil {
		return ""
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return ""
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := first[key].(string); ok {
		return v
	}
	return ""
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/workflow/ -run TestSetStopScheduler -v`
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/workflow/scheduler.go internal/workflow/scheduler_test.go
git commit -m "feat(workflow): add SetStopSchedulerWorkflow"
```

---

### Task 3: Implement CancelStopSchedulerWorkflow + tests

**Files:**
- Modify: `internal/workflow/scheduler.go` (append)
- Modify: `internal/workflow/scheduler_test.go` (append)

**Step 1: Write the failing tests**

Append to `internal/workflow/scheduler_test.go`:

```go
// --- CancelStopSchedulerWorkflow tests ---

func cancelSchedulerMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"DeleteCompShareStopScheduler": {"UHostId": "uhost-xxx", "RetCode": float64(0)},
	}}
}

func TestCancelStopScheduler_HappyPath(t *testing.T) {
	executor := cancelSchedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)

	var deleteCall map[string]any
	for _, call := range executor.calls {
		if call.action == "DeleteCompShareStopScheduler" {
			deleteCall = call.args
			break
		}
	}
	assert.NotNil(t, deleteCall)
	assert.Equal(t, "uhost-xxx", deleteCall["UHostId"])
}

func TestCancelStopScheduler_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-nonexistent",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未找到")
}

func TestCancelStopScheduler_ConfirmDenied(t *testing.T) {
	executor := cancelSchedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认取消", result.StoppedAt)
}

func TestCancelStopScheduler_StoppedInstance_Allowed(t *testing.T) {
	// Stopped instances may have residual scheduler tasks — should be allowed.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-xxx", "State": "Stopped", "ChargeType": "Dynamic"},
		}},
		"DeleteCompShareStopScheduler": {"UHostId": "uhost-xxx", "RetCode": float64(0)},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/workflow/ -run TestCancelStopScheduler -v`
Expected: FAIL — `CancelStopSchedulerDef` undefined

**Step 3: Write implementation**

Append to `internal/workflow/scheduler.go`:

```go
// CancelStopSchedulerDef returns the 3-step workflow for cancelling scheduled shutdown.
func CancelStopSchedulerDef() *Definition {
	return &Definition{
		Name:        "CancelStopSchedulerWorkflow",
		Description: "查询实例 → 确认取消 → 取消定时关机",
		Steps: []Step{
			stepQueryForCancelScheduler(),
			stepConfirmCancelScheduler(),
			stepDeleteScheduler(),
		},
	}
}

func stepQueryForCancelScheduler() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			if state == "" {
				return false, "未找到该实例。"
			}
			return true, ""
		},
	}
}

func stepConfirmCancelScheduler() Step {
	return Step{
		Name: "确认取消",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["warning"] = "将尝试取消该实例的定时关机任务。"
			return summary, nil
		},
	}
}

func stepDeleteScheduler() Step {
	return Step{
		Name: "取消定时关机",
		Type: StepToolCall,
		Tool: "DeleteCompShareStopScheduler",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
			}, nil
		},
	}
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/workflow/ -run TestCancelStopScheduler -v`
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/workflow/scheduler.go internal/workflow/scheduler_test.go
git commit -m "feat(workflow): add CancelStopSchedulerWorkflow"
```

---

### Task 4: Register workflows + tool schemas

**Files:**
- Modify: `internal/workflow/registry.go` (add 2 entries)
- Modify: `internal/tools/registry.go` (add 2 tool definitions)
- Modify: `internal/workflow/registry_test.go` (update test)

**Step 1: Add to workflow registry**

In `internal/workflow/registry.go`, add to `workflowRegistry` map:

```go
"SetStopSchedulerWorkflow":    SetStopSchedulerDef,
"CancelStopSchedulerWorkflow": CancelStopSchedulerDef,
```

**Step 2: Add tool schema definitions**

In `internal/tools/registry.go`, add before the `// --- Diagnosis Meta-Tools ---` comment:

```go
{
    Type: openai.ToolTypeFunction,
    Function: &openai.FunctionDefinition{
        Name:        "SetStopSchedulerWorkflow",
        Description: "设置定时关机工作流。为运行中的实例设置自动关机时间。支持相对时间（如30分钟后）或绝对时间。抢占式实例不支持。用户要求定时关机、自动关机、延时关机时使用此工具。",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "UHostId": map[string]any{
                    "type":        "string",
                    "description": "要设置定时关机的实例 ID",
                },
                "AfterMinutes": map[string]any{
                    "type":        "number",
                    "description": "几分钟后关机（正整数，最小 5）。与 ShutdownAt 二选一。如：60 表示 1 小时后关机。",
                },
                "ShutdownAt": map[string]any{
                    "type":        "string",
                    "description": "指定关机时间。支持格式：2026-04-16 23:00（按北京时间解析）或 RFC3339。与 AfterMinutes 二选一。",
                },
            },
            "required": []string{"UHostId"},
        },
    },
},
{
    Type: openai.ToolTypeFunction,
    Function: &openai.FunctionDefinition{
        Name:        "CancelStopSchedulerWorkflow",
        Description: "取消定时关机工作流。取消实例已设置的定时关机任务。用户要求取消定时关机、取消自动关机时使用此工具。",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "UHostId": map[string]any{
                    "type":        "string",
                    "description": "要取消定时关机的实例 ID",
                },
            },
            "required": []string{"UHostId"},
        },
    },
},
```

**Step 3: Update registry test**

In `internal/workflow/registry_test.go`, add the two new workflow names to the test data.

**Step 4: Run all tests**

Run: `go test ./...`
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/workflow/registry.go internal/tools/registry.go internal/workflow/registry_test.go
git commit -m "feat(workflow): register SetStopScheduler and CancelStopScheduler workflows"
```

---

### Task 5: Full verification

**Step 1: Run all tests**

Run: `go test ./... -v 2>&1 | tail -20`
Expected: all packages PASS

**Step 2: Build check**

Run: `go build ./...`
Expected: clean, no errors

**Step 3: Verify go vet**

Run: `go vet ./...`
Expected: clean
