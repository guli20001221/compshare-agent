package workflow

type StepType int

const (
	StepToolCall StepType = iota
	StepConfirm
)

type Step struct {
	Name        string
	Type        StepType
	Tool        string
	BuildArgs   func(wfCtx *Context) (map[string]any, error)
	CheckResult func(result map[string]any) (bool, string)
}

type Definition struct {
	Name        string
	Description string
	Steps       []Step
}

type Context struct {
	Params      map[string]any
	StepResults map[string]map[string]any
}

func NewContext(params map[string]any) *Context {
	if params == nil {
		params = make(map[string]any)
	}
	return &Context{
		Params:      params,
		StepResults: make(map[string]map[string]any),
	}
}

func (c *Context) Result(stepName string) map[string]any {
	return c.StepResults[stepName]
}

type Result struct {
	Success   bool          `json:"success"`
	StoppedAt string        `json:"stopped_at,omitempty"`
	Message   string        `json:"message"`
	Steps     []StepSummary `json:"steps"`
}

type StepSummary struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type ConfirmFunc func(action string, args map[string]any) bool

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
