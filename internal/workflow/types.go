package workflow

import "time"

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
}

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
}

// Context accumulates state during workflow execution.
type Context struct {
	Params      map[string]any
	StepResults map[string]map[string]any
}

// NewContext creates a workflow context with the given initial parameters.
func NewContext(params map[string]any) *Context {
	if params == nil {
		params = make(map[string]any)
	}
	return &Context{
		Params:      params,
		StepResults: make(map[string]map[string]any),
	}
}

// Result returns the API result from a previous step, or nil.
func (c *Context) Result(stepName string) map[string]any {
	return c.StepResults[stepName]
}

// Result of executing a workflow.
type Result struct {
	Success   bool          `json:"success"`
	StoppedAt string        `json:"stopped_at,omitempty"`
	Message   string        `json:"message"`
	Steps     []StepSummary `json:"steps"`
}

// StepSummary records one step's outcome.
type StepSummary struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // success / failed / cancelled
	Message string `json:"message,omitempty"`
}

// ConfirmFunc asks the user to confirm. Receives workflow name + summary args.
type ConfirmFunc func(action string, args map[string]any) bool

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
