package workflow

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
	Tool        string // API action name (for StepToolCall only)
	BuildArgs   func(wfCtx *Context) (map[string]any, error)
	CheckResult func(result map[string]any) (bool, string)
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
