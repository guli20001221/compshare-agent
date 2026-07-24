package diagnosis

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/tools"
)

// VerdictAction determines what happens after a diagnosis step evaluates.
type VerdictAction int

const (
	Continue VerdictAction = iota
	Conclude
)

type Verdict struct {
	Action         VerdictAction
	Conclusion     string
	Suggestion     string
	PrecheckStatus PrecheckStatus
}

type PrecheckStatus string

const (
	PrecheckConfigured PrecheckStatus = "configured"
	PrecheckBlocked    PrecheckStatus = "blocked"
	PrecheckUnknown    PrecheckStatus = "unknown"
)

type Step struct {
	Name      string
	Tool      string
	BuildArgs func(dCtx *Context) (map[string]any, error)
	// Execute optionally owns a multi-call read such as pagination. When nil,
	// Engine performs the ordinary single Tool call. The function receives the
	// already-built arguments and must return one merged response.
	Execute  func(context.Context, tools.ToolExecutor, map[string]any) (map[string]any, error)
	Evaluate func(result map[string]any, dCtx *Context) Verdict
}

type Chain struct {
	Name     string
	Steps    []Step
	Fallback Verdict
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

// RequireUHostId extracts and validates the UHostId param.
// Returns the ID or an error if missing/empty.
func (c *Context) RequireUHostId() (string, error) {
	id, ok := c.Params["UHostId"]
	if !ok || id == nil || id == "" {
		return "", fmt.Errorf("missing required param: UHostId")
	}
	s, ok := id.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("missing required param: UHostId")
	}
	return s, nil
}

type DiagResult struct {
	Success        bool           `json:"success"`
	Conclusion     string         `json:"conclusion"`
	Suggestion     string         `json:"suggestion"`
	PrecheckStatus PrecheckStatus `json:"precheck_status,omitempty"`
	StoppedAt      string         `json:"stopped_at,omitempty"`
	Steps          []StepSummary  `json:"steps"`
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
