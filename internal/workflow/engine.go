package workflow

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/tools"
)

// Engine executes workflow definitions step by step.
type Engine struct {
	executor  tools.ToolExecutor
	confirmFn ConfirmFunc
	onStep    func(StepEvent)
}

// NewEngine creates a workflow engine.
func NewEngine(executor tools.ToolExecutor, confirmFn ConfirmFunc, onStep func(StepEvent)) *Engine {
	return &Engine{executor: executor, confirmFn: confirmFn, onStep: onStep}
}

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
			args, err := step.BuildArgs(wfCtx)
			if err != nil {
				e.emit(step.Name, i, total, StepToolCall, "failed", step.Tool, nil, err.Error())
				result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
				result.StoppedAt = step.Name
				result.Message = fmt.Sprintf("步骤「%s」参数构建失败: %v", step.Name, err)
				return result, nil
			}

			e.emit(step.Name, i, total, StepToolCall, "running", step.Tool, args, "")

			apiResult, err := e.executor.Execute(ctx, step.Tool, args)
			if err != nil {
				e.emit(step.Name, i, total, StepToolCall, "failed", step.Tool, nil, err.Error())
				result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
				result.StoppedAt = step.Name
				result.Message = fmt.Sprintf("步骤「%s」执行失败: %v", step.Name, err)
				return result, nil
			}

			wfCtx.StepResults[step.Name] = apiResult

			if step.CheckResult != nil {
				ok, msg := step.CheckResult(apiResult)
				if !ok {
					e.emit(step.Name, i, total, StepToolCall, "failed", step.Tool, nil, msg)
					result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
					result.StoppedAt = step.Name
					result.Message = msg
					return result, nil
				}
			}

			e.emit(step.Name, i, total, StepToolCall, "success", step.Tool, nil, "")
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})

		case StepConfirm:
			args, _ := step.BuildArgs(wfCtx)
			e.emit(step.Name, i, total, StepConfirm, "waiting", "", args, "")

			if e.confirmFn == nil || !e.confirmFn(def.Name, args) {
				e.emit(step.Name, i, total, StepConfirm, "cancelled", "", nil, "用户取消了操作")
				result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "cancelled"})
				result.StoppedAt = step.Name
				result.Message = "用户取消了操作"
				return result, nil
			}

			e.emit(step.Name, i, total, StepConfirm, "success", "", nil, "")
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})
		}
	}

	result.Success = true
	result.Message = "工作流执行完成"
	return result, nil
}

func (e *Engine) emit(name string, idx, total int, st StepType, status, tool string, args map[string]any, msg string) {
	if e.onStep != nil {
		e.onStep(StepEvent{
			StepName: name, StepIndex: idx, Total: total,
			Type: st, Status: status, Tool: tool, Args: args, Message: msg,
		})
	}
}
