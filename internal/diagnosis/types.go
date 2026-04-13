package diagnosis

// VerdictAction determines what happens after a diagnosis step evaluates.
type VerdictAction int

const (
	Continue VerdictAction = iota
	Conclude
)

type Verdict struct {
	Action     VerdictAction
	Conclusion string
	Suggestion string
}

type Step struct {
	Name      string
	Tool      string
	BuildArgs func(dCtx *Context) (map[string]any, error)
	Evaluate  func(result map[string]any, dCtx *Context) Verdict
}

type Chain struct {
	Name        string
	Description string
	Steps       []Step
	Fallback    Verdict
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

type DiagResult struct {
	Success    bool          `json:"success"`
	Conclusion string        `json:"conclusion"`
	Suggestion string        `json:"suggestion"`
	StoppedAt  string        `json:"stopped_at,omitempty"`
	Steps      []StepSummary `json:"steps"`
}

type StepSummary struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type DiagEvent struct {
	StepName  string
	StepIndex int
	Total     int
	Status    string
	Tool      string
	Args      map[string]any
	Message   string
}
