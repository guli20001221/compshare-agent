// Package orchestrator is the agent-tier multi-step runner (ADR-006). It drives
// a workflow.Definition with per-step StepTrace emission, per-step timeout, and
// in-request HITL confirmation, using the strong (TierAgent) model for any
// LLM-in-the-loop reasoning.
//
// It is a SEPARATE runner from workflow.Engine.Run: that one stays the
// synchronous fail-stop runner for the shipped CLI create/diagnosis flows and
// is intentionally left untouched (ADR-007 anti-framework; B6 spec §3
// two-runner shape). The orchestrator consumes the SAME workflow.Step contract
// and adds the agent-grade capabilities on top.
//
// Compensation (ADR-006 §决策2) — reverse in-memory rollback of side-effecting
// steps — is DEFERRED per the lead's 2026-05-31 amendment (ADR-006 §决策2
// Amendment / B6 spec §5 Amendment):
//
//   - create-fail bills nothing → no side effect to roll back;
//   - create-success is a user-owned resource, surfaced via trace_json.steps[]
//   - conversation context for the user to keep or terminate in console;
//   - deleting it is TerminateCompShareInstance = L2, the irreversible op we
//     never auto-execute.
//
// So this runner does NOT implement a reverse-compensation phase, and
// workflow.Step.Compensate stays the B6.1 reserved-but-unconsumed field. As a
// HARD SAFETY RULE the runner refuses any step whose (static or resolved) Tool
// is L2/destructive (security.Check) AND any declared Compensate tool that is
// L2 — so "auto-terminate the created instance" is structurally impossible and
// no L2-bypass exists anywhere. Failure semantics are stop + report, mirroring
// workflow.Engine.Run.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// StepSink receives each StepTrace transition the saga produces (one per state
// change: running→success/failed/timeout for tool steps, awaiting_confirm→
// success/failed for confirm steps). Production wiring is the per-turn trace
// recorder, which appends each StepTrace to the turn's in-memory
// TraceRecord.Steps[] and persists the whole record ONCE at Finish — never a
// per-step INSERT (a per-step INSERT would collide uk_request_uuid, one
// agent_traces row per turn; ADR-006 §决策1). nil = no observability.
type StepSink interface {
	EmitStep(observability.StepTrace) error
}

// Options configures a Saga run.
type Options struct {
	// Executor runs each tool step. When backed by *tools.SafeToolExecutor it
	// MUST be wired with OriginWorkflowInternal (use NewWithSafeExecutor, or
	// safe.AsToolExecutor(tools.OriginWorkflowInternal)) — NOT the raw
	// SafeToolExecutor, whose Execute pins Origin=OriginDirectLLM and re-triggers
	// the per-action NeedsConfirm gate (safe_executor.go:402-411) for L1 steps
	// the saga's StepConfirm already approved (double-prompt / silent
	// ErrUserDeclined). The StepConfirm step is the saga's SOLE HITL gate,
	// mirroring the existing workflow.Engine wiring (engine.go:3048). The
	// L2-always-refuse gate (ExecuteSafe:189) still applies regardless.
	Executor  tools.ToolExecutor   // required: runs each tool step
	Confirm   workflow.ConfirmFunc // HITL gate for StepConfirm; nil = decline all
	Sink      StepSink             // step-trace sink; nil = no trace
	SessionID string               // stamped into every StepTrace
	TurnID    string               // stamped into every StepTrace
	SagaID    string               // multi-step task group id; derived from TurnID when empty
	SkillID   string               // ADR-003 skill name driving this run; may be empty
	Now       func() time.Time     // clock; defaults to time.Now (injected for tests)
	Logf      func(string, ...any) // warning sink; defaults to log.Printf
}

// Saga is the agent-tier step runner. Construct with New; drive with Run.
type Saga struct {
	executor  tools.ToolExecutor
	confirm   workflow.ConfirmFunc
	sink      StepSink
	sessionID string
	turnID    string
	sagaID    string
	skillID   string
	now       func() time.Time
	logf      func(string, ...any)
}

// New builds a Saga, filling defaults (Now=time.Now, Logf=log.Printf,
// SagaID derived from TurnID).
func New(opts Options) *Saga {
	s := &Saga{
		executor:  opts.Executor,
		confirm:   opts.Confirm,
		sink:      opts.Sink,
		sessionID: opts.SessionID,
		turnID:    opts.TurnID,
		sagaID:    opts.SagaID,
		skillID:   opts.SkillID,
		now:       opts.Now,
		logf:      opts.Logf,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	if s.sagaID == "" {
		if s.turnID != "" {
			s.sagaID = s.turnID + "-saga"
		} else {
			s.sagaID = "saga"
		}
	}
	return s
}

// NewWithSafeExecutor builds a Saga whose executor is the SafeToolExecutor wired
// with OriginWorkflowInternal — making the double-confirm misuse described on
// Options.Executor unrepresentable at the (B8) call site. The saga's StepConfirm
// step remains the sole HITL gate; the L2-always-refuse gate (ExecuteSafe:189)
// still applies. opts.Executor, if set, is overridden.
func NewWithSafeExecutor(safe *tools.SafeToolExecutor, opts Options) *Saga {
	opts.Executor = safe.AsToolExecutor(tools.OriginWorkflowInternal)
	return New(opts)
}

// stepResult is the internal outcome of one step.
type stepResult struct {
	result          map[string]any
	state           observability.StepState // success / failed / timeout
	msg             string                  // failure message (StepSummary + Result.Message)
	confirmDeclined bool                    // true → StepSummary status "cancelled"
}

// Run executes the definition forward, emitting a StepTrace per transition. On
// any failure it STOPS and returns a workflow.Result with StoppedAt set — it
// does NOT compensate (see package doc; ADR-006 §决策2 Amendment). Like
// workflow.Engine.Run it never returns a Go error for step failures; those are
// captured in the Result. A nil/invalid definition or an L2-containing
// definition returns a Go error (programming error, not a step failure).
func (s *Saga) Run(ctx context.Context, def *workflow.Definition, params map[string]any) (*workflow.Result, error) {
	if def == nil {
		return nil, errors.New("orchestrator: nil workflow definition")
	}
	if err := validateNoDestructive(def); err != nil {
		return nil, err
	}

	wfCtx := workflow.NewContext(params)
	result := &workflow.Result{Steps: make([]workflow.StepSummary, 0, len(def.Steps))}

	for i, step := range def.Steps {
		if err := ctx.Err(); err != nil {
			result.StoppedAt = step.Name
			result.Message = fmt.Sprintf("任务已取消: %v", err)
			return result, nil
		}

		var sr stepResult
		switch step.Type {
		case workflow.StepConfirm:
			sr = s.runConfirmStep(step, i, wfCtx, def.Name)
		default:
			sr = s.runToolStep(ctx, step, i, wfCtx)
		}

		if sr.state == observability.StepStateSuccess {
			result.Steps = append(result.Steps, workflow.StepSummary{Name: step.Name, Status: "success"})
			continue
		}

		status := "failed"
		if sr.confirmDeclined {
			status = "cancelled"
		}
		result.Steps = append(result.Steps, workflow.StepSummary{Name: step.Name, Status: status, Message: sr.msg})
		result.StoppedAt = step.Name
		result.Message = sr.msg
		return result, nil
	}

	result.Success = true
	result.Message = "任务执行完成"
	return result, nil
}

// validateNoDestructive enforces the hard safety rule: no step's forward Tool
// and no declared Compensate tool may be an L2/destructive action. Only an
// explicit security.Check L2 classification rejects — unknown actions (Check
// errors) are left to the executor's own gate (SafeToolExecutor refuses L2 at
// ExecuteSafe regardless). ToolFunc-resolved names are re-checked at runtime in
// runToolStep, since they are not knowable statically.
func validateNoDestructive(def *workflow.Definition) error {
	for _, step := range def.Steps {
		if step.Tool != "" {
			if lvl, err := security.Check(step.Tool); err == nil && lvl == security.L2 {
				return fmt.Errorf("orchestrator: step %q tool %q is L2/destructive and cannot enter the saga forward pass (ADR-006 §决策2)", step.Name, step.Tool)
			}
		}
		if step.Compensate != nil {
			if lvl, err := security.Check(step.Compensate.Tool); err == nil && lvl == security.L2 {
				return fmt.Errorf("orchestrator: step %q compensate tool %q is L2/destructive; the saga never auto-executes irreversible compensation (ADR-006 §决策2 Amendment)", step.Name, step.Compensate.Tool)
			}
		}
	}
	return nil
}
