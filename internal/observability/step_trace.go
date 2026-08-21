package observability

import (
	"time"

	"github.com/compshare-agent/internal/security"
)

// StepState is the lifecycle state of one orchestrated step.
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

// StepTrace is the persisted per-step workflow record. It is separate from the
// user-facing workflow event and is embedded in the turn trace JSON.
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

// RedactStepDerivedFields redacts step arguments and results before persistence.
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
