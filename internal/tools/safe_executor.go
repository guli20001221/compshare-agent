package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/sanitizer"
	"github.com/compshare-agent/internal/security"
)

var (
	ErrPolicyMissing     = errors.New("tool execution policy missing")
	ErrDestructiveAction = errors.New("destructive action refused")
	ErrUserDeclined      = errors.New("user declined confirmation")
	ErrNonExternalAction = errors.New("non-external action cannot be executed by API executor")
)

type ExecutionOrigin string

const (
	OriginDirectLLM         ExecutionOrigin = "direct_llm"
	OriginWorkflowInternal  ExecutionOrigin = "workflow_internal"
	OriginDiagnosisInternal ExecutionOrigin = "diagnosis_internal"
)

type ConfirmFunc func(action string, args map[string]any) bool

type SafeToolRequest struct {
	Action string
	Args   map[string]any
	Origin ExecutionOrigin
	Hooks  SafeToolHooks
}

type SafeToolHooks struct {
	OnConfirmNeeded func(action string, args map[string]any)
	OnBeforeCall    func(action string, args map[string]any)
}

type SafeToolResult struct {
	Action      string
	Args        map[string]any
	RawResult   map[string]any
	LLMResult   map[string]any
	TraceResult map[string]any
	Display     SafeDisplay
	Attempts    int
	Policy      ToolExecutionPolicy
}

type SafeDisplay struct {
	Kind  string
	Value string
}

type SafeToolExecutor struct {
	inner    ToolExecutor
	policies map[string]ToolExecutionPolicy
	confirm  ConfirmFunc
}

type SafeOption func(*SafeToolExecutor)

func NewSafeToolExecutor(inner ToolExecutor, opts ...SafeOption) *SafeToolExecutor {
	s := &SafeToolExecutor{
		inner:    inner,
		policies: DefaultToolExecutionPolicies(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func WithPolicies(policies map[string]ToolExecutionPolicy) SafeOption {
	return func(s *SafeToolExecutor) {
		s.policies = policies
	}
}

func WithConfirmFunc(confirm ConfirmFunc) SafeOption {
	return func(s *SafeToolExecutor) {
		s.confirm = confirm
	}
}

func (s *SafeToolExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	result, err := s.ExecuteSafe(ctx, SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: OriginDirectLLM,
	})
	if err != nil {
		return nil, err
	}
	return result.RawResult, nil
}

func (s *SafeToolExecutor) AsToolExecutor(origin ExecutionOrigin) ToolExecutor {
	return originExecutor{safe: s, origin: origin}
}

func (s *SafeToolExecutor) ExternalExecutor() *ExternalExecutor {
	if ext, ok := s.inner.(*ExternalExecutor); ok {
		return ext
	}
	if provider, ok := s.inner.(interface{ ExternalExecutor() *ExternalExecutor }); ok {
		return provider.ExternalExecutor()
	}
	return nil
}

func (s *SafeToolExecutor) FilterArgs(action string, args map[string]any) map[string]any {
	policy, ok := s.policies[action]
	if !ok {
		return copyMap(args)
	}
	return filterSafeArgs(args, policy.AllowedParams)
}

func (s *SafeToolExecutor) RedactArgs(action string, args map[string]any) map[string]any {
	return sanitizer.SanitizeArgs(action, args)
}

type originExecutor struct {
	safe   *SafeToolExecutor
	origin ExecutionOrigin
}

func (e originExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	result, err := e.safe.ExecuteSafe(ctx, SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: e.origin,
	})
	if err != nil {
		return nil, err
	}
	return result.RawResult, nil
}

func (s *SafeToolExecutor) ExecuteSafe(ctx context.Context, req SafeToolRequest) (*SafeToolResult, error) {
	if req.Origin == "" {
		req.Origin = OriginDirectLLM
	}

	policy, ok := s.policies[req.Action]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPolicyMissing, req.Action)
	}
	if policy.Route != ActionRouteExternalAPI {
		return nil, fmt.Errorf("%w: %s (%s)", ErrNonExternalAction, req.Action, policy.Route)
	}
	if policy.SecurityLevel == security.L2 || policy.Class == ActionClassDestructive {
		return nil, fmt.Errorf("%w: %s", ErrDestructiveAction, req.Action)
	}

	args := filterSafeArgs(req.Args, policy.AllowedParams)
	if shouldConfirm(policy, req.Origin) {
		if req.Hooks.OnConfirmNeeded != nil {
			req.Hooks.OnConfirmNeeded(req.Action, args)
		}
		if s.confirm == nil || !s.confirm(req.Action, args) {
			return nil, fmt.Errorf("%w: %s", ErrUserDeclined, req.Action)
		}
	}

	if req.Hooks.OnBeforeCall != nil {
		req.Hooks.OnBeforeCall(req.Action, args)
	}

	raw, attempts, err := s.executeWithRetry(ctx, policy, req.Action, args)
	if err != nil {
		return nil, err
	}
	guarded := applyHistoryGuard(policy, args, raw)

	return &SafeToolResult{
		Action:      req.Action,
		Args:        args,
		RawResult:   guarded,
		LLMResult:   redactForLLM(req.Action, policy, guarded),
		TraceResult: mapFromAny(security.RedactForTrace(guarded)),
		Display:     extractDisplay(policy, guarded),
		Attempts:    attempts,
		Policy:      policy,
	}, nil
}

func (s *SafeToolExecutor) executeWithRetry(ctx context.Context, policy ToolExecutionPolicy, action string, args map[string]any) (map[string]any, int, error) {
	var attempts int
	var lastErr error
	maxAttempts := policy.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempts = 1; attempts <= maxAttempts; attempts++ {
		raw, err := s.inner.Execute(ctx, action, args)
		if err == nil {
			return raw, attempts, nil
		}
		lastErr = err
		if attempts >= maxAttempts || !shouldRetry(err, policy.RetryOn) {
			return nil, attempts, err
		}
	}
	return nil, attempts, lastErr
}

func shouldConfirm(policy ToolExecutionPolicy, origin ExecutionOrigin) bool {
	if !policy.NeedsConfirm {
		return false
	}
	switch origin {
	case OriginWorkflowInternal, OriginDiagnosisInternal:
		return false
	default:
		return true
	}
}

func filterSafeArgs(args map[string]any, allowed []string) map[string]any {
	if args == nil {
		return nil
	}
	if allowed == nil {
		return copyMap(args)
	}
	filtered := make(map[string]any, len(allowed))
	for _, key := range allowed {
		if v, ok := args[key]; ok {
			filtered[key] = v
		}
	}
	return filtered
}

func applyHistoryGuard(policy ToolExecutionPolicy, args map[string]any, raw map[string]any) map[string]any {
	if !policy.HistoryMonitorGuard || !hasMonitorTimeRangeArgs(args) || raw == nil {
		return raw
	}
	if monitorResultHasSamples(raw) {
		return raw
	}
	raw["MonitorDataStatus"] = "NO_DATA_IN_REQUESTED_WINDOW"
	raw["MonitorDataGuidance"] = "该请求时间窗没有返回有效监控采样点；不要使用当前实时数据替代，也不要编造 CPU/内存/GPU 数值。"
	return raw
}

func hasMonitorTimeRangeArgs(args map[string]any) bool {
	if args == nil {
		return false
	}
	_, hasStart := args["StartTime"]
	_, hasEnd := args["EndTime"]
	return hasStart || hasEnd
}

func monitorResultHasSamples(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "Value" && val != nil {
				return true
			}
			if monitorResultHasSamples(val) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if monitorResultHasSamples(item) {
				return true
			}
		}
	}
	return false
}

func redactForLLM(action string, policy ToolExecutionPolicy, raw map[string]any) map[string]any {
	redacted := mapFromAny(security.RedactForLLM(raw))
	for _, field := range policy.RedactInResult {
		redactFieldByName(redacted, field)
	}
	return sanitizer.Sanitize(action, redacted)
}

func mapFromAny(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

func extractDisplay(policy ToolExecutionPolicy, raw map[string]any) SafeDisplay {
	if !policy.DualChannelDisplay || policy.Action != "DescribeCompShareJupyterToken" {
		return SafeDisplay{}
	}
	token := sanitizer.ExtractJupyterToken(raw)
	if token == "" {
		return SafeDisplay{}
	}
	return SafeDisplay{Kind: "JupyterToken", Value: token}
}

func redactFieldByName(m map[string]any, field string) {
	for k, v := range m {
		if k == field {
			m[k] = "[REDACTED]"
			continue
		}
		switch typed := v.(type) {
		case map[string]any:
			redactFieldByName(typed, field)
		case []any:
			for _, item := range typed {
				if child, ok := item.(map[string]any); ok {
					redactFieldByName(child, field)
				}
			}
		}
	}
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func shouldRetry(err error, retryOn []ErrorClass) bool {
	if len(retryOn) == 0 {
		return false
	}
	classes := classifyError(err)
	for _, have := range classes {
		for _, want := range retryOn {
			if have == want {
				return true
			}
		}
	}
	return false
}

var statusCodeRE = regexp.MustCompile(`(?i)(status|status code)\D*(\d{3})`)

func classifyError(err error) []ErrorClass {
	if err == nil {
		return nil
	}
	out := []ErrorClass{}
	if errors.Is(err, io.EOF) {
		out = append(out, ErrorClassEOF)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		out = append(out, ErrorClassNetwork)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		out = append(out, ErrorClassNetwork)
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") {
		out = append(out, ErrorClassNetwork)
	}
	if strings.Contains(msg, "eof") {
		out = append(out, ErrorClassEOF)
	}
	if m := statusCodeRE.FindStringSubmatch(msg); len(m) == 3 && strings.HasPrefix(m[2], "5") {
		out = append(out, ErrorClassHTTP5xx)
	}
	return out
}
