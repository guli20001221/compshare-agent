package workflow

import (
	"fmt"
	"time"
)

// StepType identifies the kind of workflow step.
type StepType int

const (
	// StepToolCall executes an API tool via executor.
	StepToolCall StepType = iota
	// StepConfirm waits for user confirmation.
	StepConfirm
)

// Step defines one step in a workflow.
type Step struct {
	Name        string
	Type        StepType
	Tool        string                      // API action name (for StepToolCall only)
	ToolFunc    func(wfCtx *Context) string // dynamic tool name (overrides Tool if set)
	BuildArgs   func(wfCtx *Context) (map[string]any, error)
	CheckResult func(wfCtx *Context, result map[string]any) (bool, string)
	// SkipIf lets adaptive workflows omit a step once earlier context has made
	// that choice unambiguous. nil preserves the legacy "always run" behavior.
	SkipIf func(wfCtx *Context) (bool, error)
	// Optional lets a post-success enrichment step fail without failing the
	// whole workflow. Default false preserves existing fail-stop behavior.
	Optional bool
	// Compensate is the compensating action run on a LATER step's failure
	// (reverse-order rollback). nil = no side effect / nothing to roll back
	// (read-only or idempotent setter). B6.1 declares it; only the B6.2
	// orchestrator saga runner consumes it — workflow.Engine.Run ignores it,
	// so existing sync flows are byte-identical. (ADR-006 §决策2)
	Compensate *CompensateStep
	// Timeout is the per-step cancel deadline. 0 = inherit ctx (current
	// behavior). Consumed only by the B6.2 saga runner; ignored by
	// workflow.Engine.Run. (ADR-006 §决策2, default 240s applied by the runner)
	Timeout time.Duration

	// --- Editable confirm form (StepConfirm only; all three nil/empty =
	// legacy boolean confirm, byte-identical). Consumed only by
	// workflow.Engine.Run when a ConfirmEditsFunc is wired (HTTP path with
	// COMPSHARE_CONFIRM_FORM on + client opt-in); the saga runner ignores
	// them, so deploy_model keeps the plain confirm. ---

	// BuildForm builds the editable selection form shown alongside the
	// confirm card. All option values are server-generated; user overrides
	// are validated against them (whitelist) before ApplyOverrides runs.
	BuildForm func(wfCtx *Context) (*ConfirmForm, error)
	// ApplyOverrides merges validated form overrides into wfCtx.Params.
	ApplyOverrides func(wfCtx *Context, overrides map[string]string) error
	// RevalidateSteps names earlier StepToolCall steps to re-run after
	// ApplyOverrides, before re-entering this confirm (e.g. stock + price).
	RevalidateSteps []string
	// ConfirmSubmitMode controls what happens after a form-bearing confirm is
	// submitted with Overrides. Empty preserves the legacy edit→revalidate→
	// re-confirm loop. ConfirmSubmitContinue applies the whitelisted selection
	// and advances to the next workflow step, used by guided multi-step forms.
	ConfirmSubmitMode ConfirmSubmitMode
}

// ConfirmSubmitMode controls StepConfirm form-submission behavior.
type ConfirmSubmitMode string

const (
	ConfirmSubmitRevalidate ConfirmSubmitMode = ""
	ConfirmSubmitContinue   ConfirmSubmitMode = "continue"
)

// CompensateStep is the rollback action for a side-effecting Step, run in
// reverse order by the orchestrator saga when a later step fails (B6.2).
type CompensateStep struct {
	Tool      string
	BuildArgs func(wfCtx *Context, stepResult map[string]any) (map[string]any, error)
	// BestEffort true = a failed compensate logs and continues the rollback
	// rather than wedging it ("partial rollback + tell user" > "rollback
	// deadlock"). Default true is applied by the saga runner.
	BestEffort bool
}

// Definition holds a complete workflow.
type Definition struct {
	Name        string
	Description string
	Steps       []Step
	ResultData  func(wfCtx *Context) map[string]any
}

// Context accumulates state during workflow execution.
type Context struct {
	Params        map[string]any
	InitialParams map[string]any
	StepResults   map[string]map[string]any
}

// NewContext creates a workflow context with the given initial parameters.
func NewContext(params map[string]any) *Context {
	if params == nil {
		params = make(map[string]any)
	}
	initial := make(map[string]any, len(params))
	for k, v := range params {
		initial[k] = v
	}
	return &Context{
		Params:        params,
		InitialParams: initial,
		StepResults:   make(map[string]map[string]any),
	}
}

// Result returns the API result from a previous step, or nil.
func (c *Context) Result(stepName string) map[string]any {
	return c.StepResults[stepName]
}

// Result of executing a workflow.
type Result struct {
	Success   bool           `json:"success"`
	StoppedAt string         `json:"stopped_at,omitempty"`
	Message   string         `json:"message"`
	Data      map[string]any `json:"data,omitempty"`
	Steps     []StepSummary  `json:"steps"`
}

// StepSummary records one step's outcome.
type StepSummary struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // success / failed / cancelled
	Message string `json:"message,omitempty"`
}

// ConfirmFunc asks the user to confirm. Receives workflow name + summary args.
type ConfirmFunc func(action string, args map[string]any) bool

// ConfirmForm is the editable selection form attached to a confirmation.
// It is wire-shaped (PascalCase JSON) — the HTTP layer embeds it verbatim in
// the `confirmation` frame's optional Form field. v1 is select-only: every
// editable field carries server-generated Options and user edits may only
// pick one of them (no free text).
type ConfirmForm struct {
	Version int                `json:"Version"`
	Step    *ConfirmFormStep   `json:"Step,omitempty"`
	Fields  []ConfirmFormField `json:"Fields"`
}

// ConfirmFormStep carries guided-form presentation metadata. It is optional so
// v1 forms and legacy clients keep the same JSON shape.
type ConfirmFormStep struct {
	Index          int    `json:"Index"`
	Total          int    `json:"Total"`
	Title          string `json:"Title,omitempty"`
	Description    string `json:"Description,omitempty"`
	PrimaryLabel   string `json:"PrimaryLabel,omitempty"`
	SecondaryLabel string `json:"SecondaryLabel,omitempty"`
	Skippable      bool   `json:"Skippable,omitempty"`
	Final          bool   `json:"Final,omitempty"`
}

// ConfirmFormField is one editable (or display-only) field of a ConfirmForm.
type ConfirmFormField struct {
	Key      string              `json:"Key"`   // override key, e.g. GpuType / Zone / ImageId / ChargeType
	Label    string              `json:"Label"` // display label, backend-formatted
	Type     string              `json:"Type"`  // v1: "select" only
	Value    string              `json:"Value"` // current/default option value
	Render   string              `json:"Render,omitempty"`
	Editable bool                `json:"Editable"`
	Options  []ConfirmFormOption `json:"Options,omitempty"`
}

// ConfirmFormOption is one selectable value of a select field.
type ConfirmFormOption struct {
	Value    string            `json:"Value"`
	Label    string            `json:"Label"`
	Note     string            `json:"Note,omitempty"`
	Reason   string            `json:"Reason,omitempty"`
	Disabled bool              `json:"Disabled,omitempty"`
	Meta     map[string]string `json:"Meta,omitempty"`
}

// Field returns the form field with the given key, or nil.
func (f *ConfirmForm) Field(key string) *ConfirmFormField {
	if f == nil {
		return nil
	}
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return &f.Fields[i]
		}
	}
	return nil
}

// ValidateOverrides checks user-supplied overrides against the form: every
// key must name an Editable field and every value must be one of that field's
// Options. Returns a user-displayable error on the first violation.
func (f *ConfirmForm) ValidateOverrides(overrides map[string]string) error {
	for k, v := range overrides {
		field := f.Field(k)
		if field == nil || !field.Editable {
			return fmt.Errorf("字段 %s 不可修改", k)
		}
		valid := false
		for _, opt := range field.Options {
			if opt.Value == v && !opt.Disabled {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("字段 %s 的取值不在可选范围内", k)
		}
	}
	return nil
}

// FormDefaultOverrides returns the currently selected value of every editable
// field. Guided multi-step forms use this when the user confirms a card without
// changing anything, so the displayed default becomes the workflow parameter.
func FormDefaultOverrides(form *ConfirmForm) map[string]string {
	if form == nil {
		return nil
	}
	out := map[string]string{}
	for _, field := range form.Fields {
		if field.Editable && field.Value != "" {
			out[field.Key] = field.Value
		}
	}
	return out
}

// ConfirmResolution is the user's decision on a form-bearing confirmation:
// confirmed/denied plus any validated field overrides.
type ConfirmResolution struct {
	Confirmed bool
	Overrides map[string]string
}

// ConfirmEditsFunc is the richer HITL gate used when a StepConfirm carries a
// BuildForm. It presents args + form and returns the user's resolution. The
// transport (HTTP ConfirmBroker) has already validated Overrides against the
// form; Engine.Run re-validates defensively before applying.
type ConfirmEditsFunc func(action string, args map[string]any, form *ConfirmForm) ConfirmResolution

// StepEvent is emitted during workflow execution for UI/CLI display.
type StepEvent struct {
	StepName  string
	StepIndex int
	Total     int
	Type      StepType
	Status    string // running / success / failed / waiting / cancelled
	Tool      string
	Args      map[string]any
	Message   string
}
