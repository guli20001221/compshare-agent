package tools

import (
	"strings"

	"github.com/compshare-agent/internal/security"
)

type ActionClass string

const (
	ActionClassReadCheap              ActionClass = "read_cheap"
	ActionClassReadExpensiveDefault   ActionClass = "read_expensive_default"
	ActionClassReadExpensivePerTarget ActionClass = "read_expensive_per_target"
	ActionClassMutating               ActionClass = "mutating"
	ActionClassDestructive            ActionClass = "destructive"
)

type ErrorClass string

const (
	ErrorClassNetwork ErrorClass = "network"
	ErrorClassEOF     ErrorClass = "eof"
	ErrorClassHTTP5xx ErrorClass = "http_5xx"
)

type ToolExecutionPolicy struct {
	Action              string
	Route               ActionRoute
	Class               ActionClass
	SecurityLevel       security.Level
	NeedsConfirm        bool
	AllowedParams       []string
	RedactInResult      []string
	DualChannelDisplay  bool
	HistoryMonitorGuard bool
	MaxRetries          int
	RetryOn             []ErrorClass
}

type ActionRoute string

const (
	ActionRouteExternalAPI ActionRoute = "external_api"
	ActionRouteKnowledge   ActionRoute = "knowledge"
	ActionRouteWorkflow    ActionRoute = "workflow"
	ActionRouteDiagnosis   ActionRoute = "diagnosis"
)

func DefaultToolExecutionPolicies() map[string]ToolExecutionPolicy {
	policies := map[string]ToolExecutionPolicy{}
	registryParams := registryAllowedParams()

	for action, allowed := range registryParams {
		policy := policyForAction(action)
		policy.AllowedParams = allowed
		policies[action] = policy
	}

	for action := range security.ActionLevels {
		if _, ok := policies[action]; ok {
			continue
		}
		policies[action] = policyForAction(action)
	}

	return policies
}

func policyForAction(action string) ToolExecutionPolicy {
	level, ok := security.ActionLevels[action]
	if !ok {
		level = inferredRegistrySecurityLevel(action)
	}

	policy := ToolExecutionPolicy{
		Action:        action,
		Route:         routeForAction(action),
		Class:         classForAction(action, level),
		SecurityLevel: level,
		NeedsConfirm:  level == security.L1,
	}

	if policy.Class == ActionClassReadCheap ||
		policy.Class == ActionClassReadExpensiveDefault ||
		policy.Class == ActionClassReadExpensivePerTarget {
		policy.MaxRetries = 1
		policy.RetryOn = []ErrorClass{ErrorClassNetwork, ErrorClassEOF, ErrorClassHTTP5xx}
	}
	if policy.SecurityLevel == security.L2 {
		policy.Class = ActionClassDestructive
		policy.MaxRetries = 0
		policy.RetryOn = nil
	}
	if action == "DescribeCompShareJupyterToken" {
		policy.DualChannelDisplay = true
		policy.RedactInResult = []string{"JupyterToken"}
	}
	if action == "ResetCompShareInstancePassword" || action == "ResetPasswordWorkflow" {
		policy.RedactInResult = append(policy.RedactInResult, "Password")
	}
	if action == "GetCompShareInstanceMonitor" {
		policy.HistoryMonitorGuard = true
	}

	return policy
}

func routeForAction(action string) ActionRoute {
	switch {
	case action == "GetGPUSpecs" || action == "GetGPURecommendation":
		return ActionRouteKnowledge
	case strings.HasSuffix(action, "Workflow"):
		return ActionRouteWorkflow
	case strings.HasPrefix(action, "Diagnose"):
		return ActionRouteDiagnosis
	default:
		return ActionRouteExternalAPI
	}
}

func inferredRegistrySecurityLevel(action string) security.Level {
	switch {
	case strings.HasSuffix(action, "Workflow"):
		return security.L1
	default:
		return security.L0
	}
}

func classForAction(action string, level security.Level) ActionClass {
	switch level {
	case security.L2:
		return ActionClassDestructive
	case security.L1:
		return ActionClassMutating
	}

	switch {
	case action == "GetCompShareInstanceMonitor":
		return ActionClassReadExpensivePerTarget
	case strings.HasPrefix(action, "Diagnose"):
		return ActionClassReadExpensiveDefault
	case strings.Contains(strings.ToLower(action), "price"):
		return ActionClassReadExpensiveDefault
	default:
		return ActionClassReadCheap
	}
}

func registryAllowedParams() map[string][]string {
	out := map[string][]string{}
	for _, tool := range Registry {
		if tool.Function == nil {
			continue
		}
		out[tool.Function.Name] = allowedParamsFromDefinition(tool.Function.Parameters)
	}
	return out
}

func allowedParamsFromDefinition(params any) []string {
	m, ok := params.(map[string]any)
	if !ok {
		return nil
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	return keys
}
