package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
)

// maxConfirmEdits caps how many times a user may submit form overrides on one
// confirm step (mirrors workflow.Engine; never create on an unvalidated combo).
const maxConfirmEdits = 3

// runConfirmStep handles a StepConfirm. Two gates coexist, mirroring
// workflow.Engine.runConfirmStep so the deploy_model saga and the ReAct
// CreateInstanceWorkflow tool behave identically:
//
//   - Editable-form gate (ConfirmEdits) — when wired AND the step declares a
//     BuildForm, the user may confirm as-is, deny, or submit select-only field
//     Overrides. Overrides re-run the step's RevalidateSteps (stock/price) with
//     the new params and re-enter the confirm with a refreshed card+form (方案 A:
//     never create on an unvalidated combination, never show a stale price),
//     capped at maxConfirmEdits rounds.
//   - Legacy boolean gate (Confirm) — used whenever the richer gate or the step's
//     BuildForm is absent (CLI, flag-off, opt-out). Byte-identical to before.
//
// HITL reuse (ADR-006 §决策3): the orchestrator does NOT own a confirm transport;
// it calls the workflow.ConfirmFunc / ConfirmEditsFunc passed via Options, which
// the live paths supply (HTTP SSE broker / CLI stdin). A nil gate declines (safe
// default: never auto-approve a mutating step).
func (s *StepRunner) runConfirmStep(ctx context.Context, def *workflow.Definition, step workflow.Step, idx int, wfCtx *workflow.Context) stepResult {
	action := def.Name
	for edits := 0; ; {
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

		// Build the editable form only when both halves exist. A BuildForm error
		// degrades to the plain confirm card — fail-open on the FORM, never on the
		// confirmation gate itself.
		var form *workflow.ConfirmForm
		if s.confirmEdits != nil && step.BuildForm != nil {
			if f, ferr := step.BuildForm(wfCtx); ferr == nil {
				form = f
			}
		}

		started := s.now()
		s.emit(observability.StepStateAwaitingConfirm, idx, "", args, nil, "", started, time.Time{})

		if form == nil {
			// Legacy boolean gate (CLI, flag-off, opt-out, or no BuildForm).
			approved := s.confirm != nil && s.confirm(action, args)
			ended := s.now()
			if !approved {
				s.emit(observability.StepStateFailed, idx, "", args, nil, "user_abort", started, ended)
				return stepResult{state: observability.StepStateFailed, msg: "用户取消了操作", confirmDeclined: true}
			}
			s.emit(observability.StepStateSuccess, idx, "", args, nil, "", started, ended)
			return stepResult{state: observability.StepStateSuccess}
		}

		res := s.confirmEdits(action, args, form)
		ended := s.now()
		if !res.Confirmed {
			s.emit(observability.StepStateFailed, idx, "", args, nil, "user_abort", started, ended)
			return stepResult{state: observability.StepStateFailed, msg: "用户取消了操作", confirmDeclined: true}
		}
		if len(res.Overrides) == 0 {
			if step.ConfirmSubmitMode == workflow.ConfirmSubmitContinue && step.ApplyOverrides != nil {
				defaults := workflow.FormDefaultOverrides(form)
				if len(defaults) > 0 {
					if verr := form.ValidateOverrides(defaults); verr != nil {
						s.emit(observability.StepStateFailed, idx, "", args, nil, "invalid_defaults", started, ended)
						return stepResult{state: observability.StepStateFailed, msg: fmt.Sprintf("默认配置无效: %v", verr)}
					}
					if aerr := step.ApplyOverrides(wfCtx, defaults); aerr != nil {
						s.emit(observability.StepStateFailed, idx, "", args, nil, "invalid_defaults", started, ended)
						return stepResult{state: observability.StepStateFailed, msg: fmt.Sprintf("默认配置无效: %v", aerr)}
					}
				}
			}
			s.emit(observability.StepStateSuccess, idx, "", args, nil, "", started, ended)
			return stepResult{state: observability.StepStateSuccess}
		}

		// Confirmed WITH overrides → validate, apply, revalidate, re-ask.
		edits++
		if edits > maxConfirmEdits {
			s.emit(observability.StepStateFailed, idx, "", args, nil, "too_many_edits", started, ended)
			return stepResult{state: observability.StepStateFailed, msg: "配置修改次数过多，请重新发起创建。"}
		}
		// Defensive re-validation; the HTTP broker already validated against this
		// form, but the gate must not trust the transport.
		if verr := form.ValidateOverrides(res.Overrides); verr != nil {
			s.emit(observability.StepStateFailed, idx, "", args, nil, "invalid_overrides", started, ended)
			return stepResult{state: observability.StepStateFailed, msg: fmt.Sprintf("配置修改无效: %v", verr)}
		}
		if step.ApplyOverrides == nil {
			s.emit(observability.StepStateFailed, idx, "", args, nil, "overrides_unsupported", started, ended)
			return stepResult{state: observability.StepStateFailed, msg: "该操作不支持修改配置，请重新发起创建。"}
		}
		if aerr := step.ApplyOverrides(wfCtx, res.Overrides); aerr != nil {
			s.emit(observability.StepStateFailed, idx, "", args, nil, "invalid_overrides", started, ended)
			return stepResult{state: observability.StepStateFailed, msg: fmt.Sprintf("配置修改无效: %v", aerr)}
		}
		if step.ConfirmSubmitMode == workflow.ConfirmSubmitContinue {
			s.emit(observability.StepStateSuccess, idx, "", args, nil, "", started, ended)
			return stepResult{state: observability.StepStateSuccess}
		}
		for _, name := range step.RevalidateSteps {
			rs, rIdx, ok := findToolStep(def, name)
			if !ok {
				continue
			}
			// Re-run the stock/price step with the new params. A failure (e.g. the
			// new combo is sold out) STOPS here — never create on an unvalidated
			// combination. runToolStep writes wfCtx.StepResults so the rebuilt
			// card+form on the next loop reflect the revalidated numbers.
			if rr := s.runToolStep(ctx, rs, rIdx, wfCtx); rr.state != observability.StepStateSuccess {
				return rr
			}
		}
		// Loop: rebuild card+form from the revalidated results and re-ask.
	}
}

// findToolStep locates a StepToolCall by name in the definition (mirror of the
// workflow package helper; used to re-run RevalidateSteps after an override).
func findToolStep(def *workflow.Definition, name string) (workflow.Step, int, bool) {
	for i, st := range def.Steps {
		if st.Name == name && st.Type == workflow.StepToolCall {
			return st, i, true
		}
	}
	return workflow.Step{}, 0, false
}
