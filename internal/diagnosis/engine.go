package diagnosis

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/tools"
)

type Engine struct {
	executor tools.ToolExecutor
	onStep   func(DiagEvent)
}

func NewEngine(executor tools.ToolExecutor, onStep func(DiagEvent)) *Engine {
	return &Engine{executor: executor, onStep: onStep}
}

func (e *Engine) Run(ctx context.Context, chain *Chain, params map[string]any) (*DiagResult, error) {
	dCtx := NewContext(params)
	total := len(chain.Steps)
	result := &DiagResult{Steps: make([]StepSummary, 0, total)}

	for i, step := range chain.Steps {
		if err := ctx.Err(); err != nil {
			result.Conclusion = fmt.Sprintf("诊断已取消: %v", err)
			return result, nil
		}

		args, err := step.BuildArgs(dCtx)
		if err != nil {
			e.emit(step.Name, i, total, "failed", step.Tool, nil, err.Error())
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: err.Error()})
			result.StoppedAt = step.Name
			result.Conclusion = fmt.Sprintf("步骤「%s」参数构建失败: %v", step.Name, err)
			return result, nil
		}

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

		e.emit(step.Name, i, total, "checked", step.Tool, nil, "")
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "checked"})
	}

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
