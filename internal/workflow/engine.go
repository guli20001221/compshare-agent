package workflow

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/tools"
)

// Engine executes workflow definitions step by step.
type Engine struct {
	executor       tools.ToolExecutor
	confirmFn      ConfirmFunc
	confirmEditsFn ConfirmEditsFunc
	onStep         func(StepEvent)
}

// NewEngine creates a workflow engine.
func NewEngine(executor tools.ToolExecutor, confirmFn ConfirmFunc, onStep func(StepEvent)) *Engine {
	return &Engine{executor: executor, confirmFn: confirmFn, onStep: onStep}
}

// SetConfirmEditsFn wires the richer editable-form HITL gate. nil (default)
// keeps every StepConfirm on the legacy boolean ConfirmFunc, byte-identical.
// Only the HTTP path sets this, and only when COMPSHARE_CONFIRM_FORM is on
// AND the client opted in via Features.
func (e *Engine) SetConfirmEditsFn(fn ConfirmEditsFunc) {
	e.confirmEditsFn = fn
}

// maxConfirmEdits caps how many times a user may submit form overrides on one
// confirm step (each edit re-runs the revalidate steps and re-asks), so a
// client resending overrides cannot spin the workflow indefinitely.
const maxConfirmEdits = 3

// Run executes a workflow definition with the given initial parameters.
// It never returns a Go error for step failures — those are captured in Result.
func (e *Engine) Run(ctx context.Context, def *Definition, params map[string]any) (*Result, error) {
	wfCtx := NewContext(params)
	total := len(def.Steps)
	result := &Result{Steps: make([]StepSummary, 0, total)}

	for i, step := range def.Steps {
		if err := ctx.Err(); err != nil {
			result.StoppedAt = step.Name
			result.Message = fmt.Sprintf("工作流已取消: %v", err)
			return result, nil
		}

		switch step.Type {
		case StepToolCall:
			if e.runToolStep(ctx, step, i, total, wfCtx, result) == toolStepFailed {
				return result, nil
			}

		case StepConfirm:
			if !e.runConfirmStep(ctx, def, step, i, total, wfCtx, result) {
				return result, nil
			}
		}
	}

	result.Success = true
	if def.ResultData != nil {
		result.Data = def.ResultData(wfCtx)
	}
	result.Message = "工作流执行完成"
	return result, nil
}

// toolStepOutcome reports how a StepToolCall ended for the Run loop.
type toolStepOutcome int

const (
	toolStepOK      toolStepOutcome = iota
	toolStepSkipped                 // failed but Optional — workflow continues
	toolStepFailed                  // failed — workflow stops (result populated)
)

// runToolStep executes one StepToolCall exactly as the Run loop always has
// (extracted verbatim so the confirm-edit revalidate path can re-run earlier
// steps through the same code). It appends to result.Steps and populates
// StoppedAt/Message on a fail-stop.
func (e *Engine) runToolStep(ctx context.Context, step Step, i, total int, wfCtx *Context, result *Result) toolStepOutcome {
	// Resolve tool name: ToolFunc (dynamic) takes priority over Tool (static)
	toolName := step.Tool
	if step.ToolFunc != nil {
		toolName = step.ToolFunc(wfCtx)
	}

	args, err := step.BuildArgs(wfCtx)
	if err != nil {
		e.emit(step.Name, i, total, StepToolCall, "failed", toolName, nil, err.Error())
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
		if step.Optional {
			return toolStepSkipped
		}
		result.StoppedAt = step.Name
		result.Message = fmt.Sprintf("步骤「%s」参数构建失败: %v", step.Name, err)
		return toolStepFailed
	}

	e.emit(step.Name, i, total, StepToolCall, "running", toolName, args, "")

	apiResult, err := e.executor.Execute(ctx, toolName, args)
	if err != nil {
		e.emit(step.Name, i, total, StepToolCall, "failed", toolName, nil, err.Error())
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
		if step.Optional {
			return toolStepSkipped
		}
		result.StoppedAt = step.Name
		result.Message = fmt.Sprintf("步骤「%s」执行失败: %v", step.Name, err)
		return toolStepFailed
	}

	wfCtx.StepResults[step.Name] = apiResult

	if step.CheckResult != nil {
		ok, msg := step.CheckResult(wfCtx, apiResult)
		if !ok {
			e.emit(step.Name, i, total, StepToolCall, "failed", toolName, nil, msg)
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
			if step.Optional {
				return toolStepSkipped
			}
			result.StoppedAt = step.Name
			result.Message = msg
			return toolStepFailed
		}
	}

	e.emit(step.Name, i, total, StepToolCall, "success", toolName, nil, "")
	result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})
	return toolStepOK
}

// runConfirmStep runs the HITL gate. Returns true to continue the workflow.
//
// Two gates coexist:
//   - Legacy boolean gate (ConfirmFunc) — used whenever the richer gate or the
//     step's BuildForm is absent. Byte-identical to the pre-form behavior.
//   - Editable-form gate (ConfirmEditsFunc) — the user may confirm as-is, deny,
//     or submit select-only field Overrides. Overrides re-run the step's
//     RevalidateSteps (stock/price) with the new params and re-enter the
//     confirm with a refreshed card+form (方案 A: never create on an
//     unvalidated combination, never show a stale price), capped at
//     maxConfirmEdits rounds.
func (e *Engine) runConfirmStep(ctx context.Context, def *Definition, step Step, i, total int, wfCtx *Context, result *Result) bool {
	failStop := func(msg string) bool {
		e.emit(step.Name, i, total, StepConfirm, "failed", "", nil, msg)
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
		result.StoppedAt = step.Name
		result.Message = msg
		return false
	}

	for edits := 0; ; {
		args, err := step.BuildArgs(wfCtx)
		if err != nil {
			return failStop(fmt.Sprintf("步骤「%s」参数构建失败: %v", step.Name, err))
		}

		// Build the editable form only when both halves exist. A BuildForm
		// error degrades to the plain confirm card — fail-open on the FORM,
		// never on the confirmation gate itself.
		var form *ConfirmForm
		if e.confirmEditsFn != nil && step.BuildForm != nil {
			if f, ferr := step.BuildForm(wfCtx); ferr == nil {
				form = f
			}
		}

		e.emit(step.Name, i, total, StepConfirm, "waiting", "", args, "")

		if form == nil {
			// Legacy boolean gate (CLI, saga-less HTTP, flag-off, opt-out).
			if e.confirmFn == nil || !e.confirmFn(def.Name, args) {
				e.emit(step.Name, i, total, StepConfirm, "cancelled", "", nil, "用户取消了操作")
				result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "cancelled"})
				result.StoppedAt = step.Name
				result.Message = "用户取消了操作"
				return false
			}
			e.emit(step.Name, i, total, StepConfirm, "success", "", nil, "")
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})
			return true
		}

		res := e.confirmEditsFn(def.Name, args, form)
		if !res.Confirmed {
			e.emit(step.Name, i, total, StepConfirm, "cancelled", "", nil, "用户取消了操作")
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "cancelled"})
			result.StoppedAt = step.Name
			result.Message = "用户取消了操作"
			return false
		}
		if len(res.Overrides) == 0 {
			e.emit(step.Name, i, total, StepConfirm, "success", "", nil, "")
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})
			return true
		}

		// Confirmed WITH overrides → validate, apply, revalidate, re-ask.
		edits++
		if edits > maxConfirmEdits {
			return failStop("配置修改次数过多，请重新发起创建。")
		}
		// Defensive re-validation; the HTTP broker already validated against
		// this form, but the gate must not trust the transport.
		if verr := form.ValidateOverrides(res.Overrides); verr != nil {
			return failStop(fmt.Sprintf("配置修改无效: %v", verr))
		}
		if step.ApplyOverrides == nil {
			return failStop("该操作不支持修改配置，请重新发起创建。")
		}
		if aerr := step.ApplyOverrides(wfCtx, res.Overrides); aerr != nil {
			return failStop(fmt.Sprintf("配置修改无效: %v", aerr))
		}
		for _, name := range step.RevalidateSteps {
			rs, idx, ok := findToolStep(def, name)
			if !ok {
				continue
			}
			if e.runToolStep(ctx, rs, idx, total, wfCtx, result) == toolStepFailed {
				return false
			}
		}
		// Loop: rebuild card+form from the revalidated results and re-ask.
	}
}

// findToolStep locates a StepToolCall by name in the definition.
func findToolStep(def *Definition, name string) (Step, int, bool) {
	for i, s := range def.Steps {
		if s.Name == name && s.Type == StepToolCall {
			return s, i, true
		}
	}
	return Step{}, 0, false
}

func (e *Engine) emit(name string, idx, total int, st StepType, status, tool string, args map[string]any, msg string) {
	if e.onStep != nil {
		e.onStep(StepEvent{
			StepName: name, StepIndex: idx, Total: total,
			Type: st, Status: status, Tool: tool, Args: args, Message: msg,
		})
	}
}
