package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/workflow"
)

// DefaultStepTimeout is the per-step cancel deadline (ADR-006 §决策2, 4 min).
// A step that exceeds it is cancelled and emits StepTrace{State:timeout}.
const DefaultStepTimeout = 240 * time.Second

// agentLLMTimeout mirrors ADR-002 tier_routing.agent.timeout_ms (180s). The
// two-layer invariant agentLLMTimeout < DefaultStepTimeout (180s < 240s) keeps
// a hung LLM call observable — it aborts and the step can still report —
// BEFORE the step's own deadline fires (ADR-006 §决策2 timeline table).
const agentLLMTimeout = 180 * time.Second

// Compile-time assertion of the 180s < 240s invariant: if a future edit inverts
// the order, the const expression goes negative and uint(...) fails to compile.
// (The "-1" makes it a strict "<": equal timeouts would also fail.)
const _ = uint((DefaultStepTimeout - agentLLMTimeout) - 1)

// effectiveTimeout resolves a step's cancel deadline: its override when set,
// else DefaultStepTimeout. An override WIDER than the default is surfaced (not
// silently honored) — the "widening isn't silent" guard from ADR-006 §决策2
// ("Per-step override > 4 min build-time 警告"), realized as a runtime warning
// the first time such a step runs.
func (s *Saga) effectiveTimeout(step workflow.Step) time.Duration {
	if step.Timeout <= 0 {
		return DefaultStepTimeout
	}
	if step.Timeout > DefaultStepTimeout {
		s.logf("orchestrator: step %q timeout %s exceeds default %s (4min) — intentional widening? (ADR-006 §决策2)", step.Name, step.Timeout, DefaultStepTimeout)
	}
	return step.Timeout
}

// runToolStep executes one StepToolCall with per-step timeout enforcement,
// emitting a running trace then a terminal (success / failed / timeout) trace.
func (s *Saga) runToolStep(ctx context.Context, step workflow.Step, idx int, wfCtx *workflow.Context) stepResult {
	toolName := step.Tool
	if step.ToolFunc != nil {
		toolName = step.ToolFunc(wfCtx)
	}

	// Hard safety rule (runtime check for ToolFunc-resolved names; the static
	// pass in validateNoDestructive covers fixed Tool). The saga never executes
	// an L2/destructive action — auto-terminate-the-created-instance is
	// structurally impossible (ADR-006 §决策2 Amendment, no L2-bypass).
	if lvl, err := security.Check(toolName); err == nil && lvl == security.L2 {
		now := s.now()
		msg := fmt.Sprintf("步骤「%s」拒绝执行不可逆操作 %s（saga 禁止 L2/destructive 动作）", step.Name, toolName)
		s.emit(observability.StepStateFailed, idx, toolName, nil, nil, "destructive_refused", now, now)
		return stepResult{state: observability.StepStateFailed, msg: msg}
	}

	args, err := step.BuildArgs(wfCtx)
	if err != nil {
		now := s.now()
		msg := fmt.Sprintf("步骤「%s」参数构建失败: %v", step.Name, err)
		s.emit(observability.StepStateFailed, idx, toolName, nil, nil, "build_args", now, now)
		return stepResult{state: observability.StepStateFailed, msg: msg}
	}

	started := s.now()
	s.emit(observability.StepStateRunning, idx, toolName, args, nil, "", started, time.Time{})

	timeout := s.effectiveTimeout(step)
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	result, execErr := s.executor.Execute(stepCtx, toolName, args)
	stepCtxErr := stepCtx.Err()
	cancel()
	ended := s.now()

	if execErr != nil {
		// Distinguish a per-step timeout (this step's own deadline fired while
		// the parent ctx is still live) from a generic API error or a parent
		// cancellation. Parent cancellation is handled by the Run loop's
		// ctx.Err() check before the next step.
		if ctx.Err() == nil && errors.Is(stepCtxErr, context.DeadlineExceeded) {
			msg := fmt.Sprintf("步骤「%s」执行超时（%s）", step.Name, timeout)
			s.emit(observability.StepStateTimeout, idx, toolName, args, nil, "timeout", started, ended)
			return stepResult{state: observability.StepStateTimeout, msg: msg}
		}
		msg := fmt.Sprintf("步骤「%s」执行失败: %v", step.Name, execErr)
		s.emit(observability.StepStateFailed, idx, toolName, args, nil, "api_error", started, ended)
		return stepResult{state: observability.StepStateFailed, msg: msg}
	}

	wfCtx.StepResults[step.Name] = result

	if step.CheckResult != nil {
		ok, msg := step.CheckResult(wfCtx, result)
		if !ok {
			s.emit(observability.StepStateFailed, idx, toolName, args, result, "check_failed", started, ended)
			return stepResult{state: observability.StepStateFailed, msg: msg}
		}
	}

	s.emit(observability.StepStateSuccess, idx, toolName, args, result, "", started, ended)
	return stepResult{result: result, state: observability.StepStateSuccess}
}

// emit builds and sends one StepTrace to the sink (no-op when sink is nil).
// Args/Result are stored verbatim; secret redaction happens centrally at
// persist time via observability.prepareForPersist → RedactStepDerivedFields,
// so a single choke point covers every sink.
func (s *Saga) emit(state observability.StepState, idx int, tool string, args map[string]any, result any, errCat string, started, ended time.Time) {
	if s.sink == nil {
		return
	}
	_ = s.sink.EmitStep(observability.StepTrace{
		SessionID:     s.sessionID,
		TurnID:        s.turnID,
		StepID:        s.stepID(idx),
		SagaID:        s.sagaID,
		SkillID:       s.skillID,
		Tool:          tool,
		Args:          args,
		State:         state,
		Result:        result,
		ErrorCategory: errCat,
		StartedAt:     started,
		EndedAt:       optionalTime(ended),
	})
}

// optionalTime returns nil for a zero time so StepTrace.EndedAt (omitempty) is
// omitted for in-progress (running / awaiting_confirm) steps rather than
// serializing the "0001-01-01T00:00:00Z" zero-time sentinel.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// stepID is a deterministic per-step id (no randomness, so traces correlate and
// tests stay stable): "<sagaID>-step-NN".
func (s *Saga) stepID(idx int) string {
	return fmt.Sprintf("%s-step-%02d", s.sagaID, idx)
}
