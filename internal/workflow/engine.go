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
	// The sealed contract (set once the confirmation gate passes) is surfaced to
	// the engine so it narrates and recovers from the exact confirmed params.
	defer func() { result.Contract = wfCtx.sealed }()

	for i, step := range def.Steps {
		if err := ctx.Err(); err != nil {
			result.StoppedAt = step.Name
			result.Message = fmt.Sprintf("工作流已取消: %v", err)
			return result, nil
		}
		if step.SkipIf != nil {
			skip, err := step.SkipIf(wfCtx)
			if err != nil {
				msg := fmt.Sprintf("步骤「%s」跳过判断失败: %v", step.Name, err)
				e.emit(step.Name, i, total, step.Type, "failed", "", nil, msg)
				result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
				result.StoppedAt = step.Name
				result.Message = msg
				return result, nil
			}
			if skip {
				continue
			}
		}

		switch step.Type {
		case StepToolCall:
			// The seal is enforced inside runToolStep, after BuildArgs — see
			// verifySealedContract for why the check cannot live here.
			if e.runToolStep(ctx, step, i, total, wfCtx, result) == toolStepFailed {
				return result, nil
			}

		case StepResolve:
			if e.runResolveStep(step, i, total, wfCtx, result) == toolStepFailed {
				return result, nil
			}

		case StepConfirm:
			if !e.runConfirmStep(ctx, def, step, i, total, wfCtx, result) {
				return result, nil
			}
			// Seal the confirmed draft: from here the business params are frozen
			// and every subsequent mutating step is checked against them.
			wfCtx.seal(def.Name)

		default:
			// A step type this loop does not handle must stop the workflow. With no
			// default arm, an unhandled type fell through the switch, emitted
			// nothing, and let Run report Success — a step declared in the
			// definition would silently not have happened, which is the worst
			// possible reading of "the workflow completed".
			msg := fmt.Sprintf("工作流定义错误：步骤「%s」的类型 %d 无法执行", step.Name, step.Type)
			e.emit(step.Name, i, total, step.Type, "failed", "", nil, msg)
			result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
			result.StoppedAt = step.Name
			result.Message = msg
			return result, nil
		}
	}

	result.Success = true
	if def.ResultData != nil {
		result.Data = def.ResultData(wfCtx)
	}
	result.Message = "工作流执行完成"
	return result, nil
}

// verifySealedContract enforces the seal for a post-confirmation tool step.
// Before the confirmation gate (no seal yet) it is a no-op. After the gate, the
// live business params must still hash to the sealed digest; a mismatch means
// something rewrote a confirmed field after the user approved it, so the
// workflow fail-stops rather than executing on unconfirmed params. Returns true
// to proceed.
//
// It MUST be called from inside runToolStep, after step.BuildArgs and before the
// executor call, and this is the whole point rather than an implementation
// detail. It used to run in the Run loop BEFORE runToolStep, so it hashed the
// params as they were before BuildArgs — which is not what the call it guards
// executes on. A step whose own BuildArgs rewrote a confirmed param therefore
// passed its own check and made its call on the rewritten value; only the NEXT
// tool step's check noticed. For CreateInstanceWorkflow the write IS the last
// mutating step (only an Optional read-back follows), so "the next step catches
// it" meant "the instance already exists". The guarantee this function's own
// doc claims — that the mutating step executes exactly what the user confirmed —
// is only true when the hash covers the state the call is actually made on.
//
// Calling it here also covers the RevalidateSteps path (runConfirmStep re-runs
// earlier steps through runToolStep directly, bypassing the Run loop). That path
// is pre-seal today, where this is a no-op, but it is no longer an unguarded
// write surface by construction rather than by luck.
func (e *Engine) verifySealedContract(step Step, i, total int, wfCtx *Context, result *Result) bool {
	if wfCtx.sealed == nil || wfCtx.sealed.verifyDigest(wfCtx.Params) {
		return true
	}
	msg := "执行合同校验失败：确认后的参数被改动，请重新发起操作。"
	e.emit(step.Name, i, total, StepToolCall, "failed", step.Tool, nil, msg)
	result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
	result.StoppedAt = step.Name
	result.Message = msg
	return false
}

// runResolveStep executes one StepResolve: a pure computation over facts earlier
// steps established, whose result lands in StepResults like a tool step's.
//
// It enforces the invariant that gives the step type its meaning: a resolve step
// MUST NOT write Params. The check is a before/after digest, always on, rather
// than the narrower "don't break a live seal" — because the two paths differ.
// Under a live seal (the guided create runs this step after six選択 gates have
// each sealed), a write would be caught by the next tool step's
// verifySealedContract; on the plain create there is no seal yet, and
// verifySealedContract fails OPEN on a nil seal, so a write there would sail
// through unnoticed. An invariant that only holds on one of the two paths is not
// an invariant.
//
// The rule is not fussiness about purity. Params is the set the user is being
// asked to confirm; a resolve step writing into it would edit the question while
// it is being asked.
func (e *Engine) runResolveStep(step Step, i, total int, wfCtx *Context, result *Result) toolStepOutcome {
	fail := func(msg string) toolStepOutcome {
		e.emit(step.Name, i, total, StepResolve, "failed", "", nil, msg)
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
		result.StoppedAt = step.Name
		result.Message = msg
		return toolStepFailed
	}
	if step.Resolve == nil {
		return fail(fmt.Sprintf("工作流定义错误：解析步骤「%s」未定义解析函数", step.Name))
	}

	e.emit(step.Name, i, total, StepResolve, "running", "", nil, "")
	before := paramsDigest(wfCtx.Params)
	out, err := step.Resolve(wfCtx)
	if err != nil {
		result.MissingSlots = MissingSlotsFromError(err)
		return fail(fmt.Sprintf("步骤「%s」执行失败: %v", step.Name, err))
	}
	if paramsDigest(wfCtx.Params) != before {
		return fail(fmt.Sprintf("工作流定义错误：解析步骤「%s」改写了业务参数，只能产出候选结果", step.Name))
	}

	wfCtx.StepResults[step.Name] = out
	e.emit(step.Name, i, total, StepResolve, "success", "", nil, "")
	result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})
	return toolStepOK
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
		result.MissingSlots = MissingSlotsFromError(err)
		return toolStepFailed
	}

	// Enforce the seal AFTER BuildArgs and before the call: BuildArgs reads
	// wfCtx.Params, and may write them, so a check that ran before it could not
	// see what this very call is about to execute on.
	if !e.verifySealedContract(step, i, total, wfCtx, result) {
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
		// Keep the typed cause alongside the flattened sentence so callers can
		// classify the failure by its fields instead of by substrings of Message.
		result.Err = err
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
	// Entering a confirmation gate means nothing is confirmed yet, so any seal
	// from an EARLIER gate is void from here. The guided create asks seven times
	// (GPU, zone, card count, CPU/memory, purpose, image, final), and Run seals
	// after each one; without this, a later card's edits would be measured against
	// the seal an earlier card left behind, and its RevalidateSteps — which run
	// through runToolStep, where the digest is now checked — would fail-stop on a
	// change the user is in the middle of making legitimately.
	//
	// The seal's lifetime is therefore exactly: from a PASSED gate to the next
	// gate or the end of the workflow. That is the window in which "the user
	// confirmed these params" is a true statement.
	wfCtx.unseal()

	failStop := func(msg string) bool {
		e.emit(step.Name, i, total, StepConfirm, "failed", "", nil, msg)
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "failed", Message: msg})
		result.StoppedAt = step.Name
		result.Message = msg
		return false
	}

	// pass is every route out of this gate that means "the user said yes". The
	// promote hook runs BEFORE the success event, so a failure to record what was
	// approved is reported as a failure rather than after an event claiming the
	// step succeeded. Run seals immediately after this returns true, so this is
	// the last moment Params can still be written legitimately.
	pass := func() bool {
		if step.PromoteOnConfirm != nil {
			if err := step.PromoteOnConfirm(wfCtx); err != nil {
				return failStop(fmt.Sprintf("步骤「%s」确认后固化参数失败: %v", step.Name, err))
			}
		}
		e.emit(step.Name, i, total, StepConfirm, "success", "", nil, "")
		result.Steps = append(result.Steps, StepSummary{Name: step.Name, Status: "success"})
		return true
	}

	for edits := 0; ; {
		args, err := step.BuildArgs(wfCtx)
		if err != nil {
			result.MissingSlots = MissingSlotsFromError(err)
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
			return pass()
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
			if step.ConfirmSubmitMode == ConfirmSubmitContinue && step.ApplyOverrides != nil {
				defaults := FormDefaultOverrides(form)
				if len(defaults) > 0 {
					if verr := form.ValidateOverrides(defaults); verr != nil {
						return failStop(fmt.Sprintf("默认配置无效: %v", verr))
					}
					if aerr := step.ApplyOverrides(wfCtx, defaults); aerr != nil {
						return failStop(fmt.Sprintf("默认配置无效: %v", aerr))
					}
				}
			}
			return pass()
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
		if step.ConfirmSubmitMode == ConfirmSubmitContinue {
			return pass()
		}
		ok, defErr := e.revalidateFrom(ctx, def, step, i, total, wfCtx, result)
		if !ok {
			if defErr != "" {
				return failStop(defErr)
			}
			return false // the failing step already populated result
		}
		// Loop: rebuild card+form from the revalidated results and re-ask.
	}
}

// revalidateFrom discards and re-runs every step from step.RevalidateFrom up to
// (not including) the confirm at confirmIdx, in definition order, after the user
// edited the params this confirm is gating.
//
// Returns (false, msg) for a definition error the caller should fail-stop with,
// and (false, "") when a re-run step failed and already populated result.
//
// The discard is not redundant with the re-run. A step that fails mid-range
// fail-stops the workflow, so nothing downstream reads a stale result on that
// path — but a SkipIf that starts returning true after the edit would otherwise
// leave the previous round's result in place to be read as if it were fresh.
// Discarding first makes "these results describe params the user has replaced"
// true by construction rather than by whichever paths happen to exist today.
func (e *Engine) revalidateFrom(ctx context.Context, def *Definition, step Step, confirmIdx, total int, wfCtx *Context, result *Result) (bool, string) {
	if step.RevalidateFrom == "" {
		return true, ""
	}
	start := -1
	for i := range def.Steps {
		if def.Steps[i].Name == step.RevalidateFrom {
			start = i
			break
		}
	}
	if start < 0 {
		return false, fmt.Sprintf("工作流定义错误：找不到重跑起点「%s」", step.RevalidateFrom)
	}
	if start >= confirmIdx {
		return false, fmt.Sprintf("工作流定义错误：重跑起点「%s」不在确认步骤「%s」之前", step.RevalidateFrom, step.Name)
	}

	for i := start; i < confirmIdx; i++ {
		delete(wfCtx.StepResults, def.Steps[i].Name)
	}

	for i := start; i < confirmIdx; i++ {
		rs := def.Steps[i]
		// A gate inside the range would ask the user to confirm something in the
		// middle of them editing it, and Run would seal that answer. Refuse the
		// definition rather than interpret it.
		if rs.Type == StepConfirm {
			return false, fmt.Sprintf("工作流定义错误：重跑区间内不能包含确认步骤「%s」", rs.Name)
		}
		if rs.SkipIf != nil {
			skip, err := rs.SkipIf(wfCtx)
			if err != nil {
				return false, fmt.Sprintf("步骤「%s」跳过判断失败: %v", rs.Name, err)
			}
			if skip {
				continue
			}
		}
		switch rs.Type {
		case StepResolve:
			if e.runResolveStep(rs, i, total, wfCtx, result) == toolStepFailed {
				return false, ""
			}
		case StepToolCall:
			if e.runToolStep(ctx, rs, i, total, wfCtx, result) == toolStepFailed {
				return false, ""
			}
		default:
			return false, fmt.Sprintf("工作流定义错误：重跑区间内步骤「%s」的类型 %d 无法执行", rs.Name, rs.Type)
		}
	}
	return true, ""
}

func (e *Engine) emit(name string, idx, total int, st StepType, status, tool string, args map[string]any, msg string) {
	if e.onStep != nil {
		e.onStep(StepEvent{
			StepName: name, StepIndex: idx, Total: total,
			Type: st, Status: status, Tool: tool, Args: args, Message: msg,
		})
	}
}
