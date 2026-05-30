package orchestrator

import (
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
)

// runConfirmStep handles a StepConfirm: it emits awaiting_confirm, calls the
// injected ConfirmFunc, and emits success (approved) or failed (declined).
//
// HITL reuse (ADR-006 §决策3): the orchestrator does NOT own a confirm
// transport. It calls the workflow.ConfirmFunc passed in via Options.Confirm,
// which the two live paths already supply:
//
//   - HTTP: the per-turn SSE-backed closure built in handlers_chat.go (it calls
//     ConfirmBroker.Register, SSE-emits event:confirmation, and blocks on the
//     channel until ConfirmCSAgentAction resolves it) — the shipped in-memory
//     broker, no PauseToken / saga_pauses / cross-process resume.
//   - CLI: cmd/cli.go cliConfirm, which blocks on stdin.
//
// Same synchronous interface, different I/O. A nil ConfirmFunc declines (safe
// default: never auto-approve a mutating step).
func (s *Saga) runConfirmStep(step workflow.Step, idx int, wfCtx *workflow.Context, action string) stepResult {
	var args map[string]any
	if step.BuildArgs != nil {
		built, err := step.BuildArgs(wfCtx)
		if err != nil {
			now := s.now()
			s.emit(observability.StepStateFailed, idx, "", nil, nil, "build_args", now, now)
			return stepResult{
				state: observability.StepStateFailed,
				msg:   "步骤「" + step.Name + "」参数构建失败: " + err.Error(),
			}
		}
		args = built
	}

	started := s.now()
	s.emit(observability.StepStateAwaitingConfirm, idx, "", args, nil, "", started, time.Time{})

	approved := s.confirm != nil && s.confirm(action, args)
	ended := s.now()

	if !approved {
		s.emit(observability.StepStateFailed, idx, "", args, nil, "user_abort", started, ended)
		return stepResult{state: observability.StepStateFailed, msg: "用户取消了操作", confirmDeclined: true}
	}

	s.emit(observability.StepStateSuccess, idx, "", args, nil, "", started, ended)
	return stepResult{state: observability.StepStateSuccess}
}
