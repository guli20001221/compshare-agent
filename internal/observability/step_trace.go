package observability

import (
	"time"

	"github.com/compshare-agent/internal/security"
)

// StepState is the lifecycle state of one orchestrated step (ADR-006 §决策1).
// The `timeout` state keeps a step-timeout from collapsing into `failed`.
type StepState string

const (
	StepStatePending         StepState = "pending"
	StepStateRunning         StepState = "running"
	StepStateAwaitingConfirm StepState = "awaiting_confirm"
	StepStateSuccess         StepState = "success"
	StepStateFailed          StepState = "failed"
	StepStateCompensated     StepState = "compensated"
	StepStateTimeout         StepState = "timeout"
)

// StepTrace is the per-step audit record for agent-tier saga execution
// (ADR-006 §决策1). It is DEFINE-FRESH, intentionally NOT a reuse of
// workflow.StepEvent (which is an 8-field, no-json-tag, CLI-display-only
// type with a different field set). StepTrace serializes into the existing
// per-turn trace_json as TraceRecord.Steps[] — zero DDL, no new table/column.
// ErrorCategory is a free-form string (user_abort/api_error/timeout/...).
type StepTrace struct {
	SessionID     string         `json:"session_id"`
	TurnID        string         `json:"turn_id"`
	StepID        string         `json:"step_id"`
	SagaID        string         `json:"saga_id,omitempty"`
	SkillID       string         `json:"skill_id,omitempty"`
	Tool          string         `json:"tool,omitempty"`
	Args          map[string]any `json:"args,omitempty"`
	State         StepState      `json:"state"`
	Result        any            `json:"result,omitempty"`
	ErrorCategory string         `json:"error_category,omitempty"`
	StartedAt     time.Time      `json:"started_at"`
	// EndedAt is a pointer so omitempty actually omits it for in-progress
	// (running / awaiting_confirm) steps. A value time.Time is never "empty"
	// to encoding/json — its zero marshals to "0001-01-01T00:00:00Z" — so a
	// value field would leak a year-0001 sentinel into intermediate step traces.
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	CompensateOf string     `json:"compensate_of,omitempty"`
}

// RedactStepDerivedFields runs each StepTrace's Args/Result through the trace
// secret-redaction boundary (security.RedactForTrace), in place. It is the
// step-trace analogue of RedactQueryDerivedFields — a SEPARATE function because
// RedactQueryDerivedValue covers only the query subtree, not step Args/Result
// (memory: sanitization-covers-all-derived-fields — cover the whole derivation
// tree from one choke point). Wired into prepareForPersist so BOTH the file and
// MySQL sinks cover step traces automatically. RedactForTrace returns a
// redacted copy and is idempotent, so multi-sink fan-out stays safe.
func RedactStepDerivedFields(steps []StepTrace) {
	for i := range steps {
		if steps[i].Args != nil {
			if red, ok := security.RedactForTrace(steps[i].Args).(map[string]any); ok {
				steps[i].Args = red
			}
		}
		if steps[i].Result != nil {
			steps[i].Result = security.RedactForTrace(steps[i].Result)
		}
	}
}
