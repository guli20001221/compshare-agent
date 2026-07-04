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
	Action                  string
	Route                   ActionRoute
	Class                   ActionClass
	SecurityLevel           security.Level
	NeedsConfirm            bool
	AllowedParams           []string
	InternalAllowedParams   []string
	RedactInResult          []string
	HistoryMonitorGuard     bool
	MaxTargetsPerCall       int
	MaxHistoryWindowSeconds int
	MaxRetries              int
	RetryOn                 []ErrorClass

	// TimeoutMS bounds a single attempt at SafeToolExecutor.Execute. The
	// inner executor receives a derived context.WithTimeout per attempt,
	// so a hung backend cannot block the agent indefinitely. Zero means
	// "use the inner executor's ambient timeout" (currently 60s on
	// ExternalExecutor's http.Client) — kept as a fallback safety net,
	// but every policy in DefaultToolExecutionPolicies sets an explicit
	// per-class value below.
	TimeoutMS int

	// BackoffBaseMS is the linear backoff base between retry attempts:
	// before attempt N (N >= 2) the executor sleeps BackoffBaseMS *
	// (N - 1) ms, respecting ctx cancellation. Zero disables backoff
	// (retries fire back-to-back). Read-class policies use a small
	// (300-500ms) base so a transient upstream hiccup gets a brief
	// breather, while mutating/destructive (MaxRetries=0) inherit
	// zero — no retry, no backoff.
	BackoffBaseMS int
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
		if actionAllowsBackendZoneID(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "zone_id")
		}
		if actionAllowsBackendAzGroup(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "az_group")
		}
		if actionAllowsBackendZoneRegion(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "Zone")
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "Region")
		}
		if actionAllowsBackendIdentity(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "top_organization_id")
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "organization_id")
		}
		policies[action] = policy
	}

	for action := range security.ActionLevels {
		if _, ok := policies[action]; ok {
			continue
		}
		policy := policyForAction(action)
		policy.AllowedParams = internalOnlyAllowedParams(action)
		if actionAllowsBackendZoneID(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "zone_id")
		}
		if actionAllowsBackendAzGroup(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "az_group")
		}
		if actionAllowsBackendZoneRegion(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "Zone")
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "Region")
		}
		if actionAllowsBackendIdentity(action) {
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "top_organization_id")
			policy.InternalAllowedParams = appendAllowedParam(policy.InternalAllowedParams, "organization_id")
		}
		policies[action] = policy
	}
	if _, ok := policies["GetProjectList"]; !ok {
		policies["GetProjectList"] = policyForAction("GetProjectList")
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

	// Per-class timeout + backoff defaults (PR #5 unification). Numbers
	// include cold STS credential acquisition when server mode uses
	// AssumeRole. Cheap reads are usually <2s once credentials are warm,
	// but the first per-role call can spend up to 10s in STS before the
	// business API call starts. Per-instance describes can hit 8-10s; the
	// monitor API is bulk-read and can take longer. Mutating/destructive
	// policies keep a generous ceiling because the agent does not retry them.
	switch policy.Class {
	case ActionClassReadCheap:
		policy.TimeoutMS = 15000
		policy.BackoffBaseMS = 300
	case ActionClassReadExpensiveDefault:
		policy.TimeoutMS = 15000
		policy.BackoffBaseMS = 500
	case ActionClassReadExpensivePerTarget:
		policy.TimeoutMS = 30000
		policy.BackoffBaseMS = 500
	case ActionClassMutating, ActionClassDestructive:
		policy.TimeoutMS = 30000
		policy.BackoffBaseMS = 0
	}

	if policy.SecurityLevel == security.L2 {
		policy.Class = ActionClassDestructive
		policy.MaxRetries = 0
		policy.RetryOn = nil
		policy.BackoffBaseMS = 0
		// Defensive: catches L2 actions whose class was overridden to
		// non-destructive upstream (e.g. a future class-derivation
		// change). For any action routed through classForAction → L2 →
		// Destructive (today's path), TimeoutMS was already set by the
		// switch above, so this branch is unreachable. Kept so the L2
		// invariant is self-contained: any L2 leaves this block with a
		// non-zero TimeoutMS regardless of upstream changes.
		if policy.TimeoutMS == 0 {
			policy.TimeoutMS = 30000
		}
	}
	if action == "ResetCompShareInstancePassword" || action == "ResetPasswordWorkflow" {
		policy.RedactInResult = append(policy.RedactInResult, "Password")
	}
	if action == "DescribeCompShareJupyterToken" {
		policy.RedactInResult = append(policy.RedactInResult, "JupyterToken")
	}
	if action == "GetCompShareInstanceMonitor" {
		policy.HistoryMonitorGuard = true
		policy.MaxTargetsPerCall = 20
		policy.MaxHistoryWindowSeconds = 86400
	}
	if action == "GetCompShareInstancePrice" || action == "GetCompShareInstanceUserPrice" {
		policy.MaxTargetsPerCall = 20
	}
	return policy
}

func internalOnlyAllowedParams(action string) []string {
	switch action {
	case "CreateCFS":
		return []string{"Name", "Size", "TotalFiles", "ChargeType", "Quantity", "CouponId", "Zone", "Region"}
	case "ResizeCFS":
		return []string{"CfsId", "Size", "CouponId"}
	case "GetCompShareAttachedDiskUpgradePrice", "CheckCompShareResizeAttachedDisk":
		return []string{"UHostId", "DiskId", "DiskSpace", "Zone", "Region"}
	case "ResizeCompShareDisk":
		return []string{"UHostId", "UDiskId", "Size", "Zone", "Region"}
	case "ResizeCompShareInstance":
		return []string{"UHostId", "Cpu", "CPU", "Gpu", "GPU", "Memory", "DiskId", "DiskSpace", "WithoutGpu", "Zone", "Region"}
	default:
		return nil
	}
}

func actionAllowsBackendZoneID(action string) bool {
	switch action {
	case "DescribeAvailableCompShareInstanceTypes",
		"DescribeCompShareGpuInventory",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstancePrice",
		"GetCompShareInstanceUserPrice",
		"GetCompShareInstanceUpgradePrice",
		"GetCompShareAttachedDiskUpgradePrice",
		"CheckCompShareResizeAttachedDisk",
		"ResizeCompShareInstance",
		"ResizeCompShareDisk",
		"GetCompShareCFSPrice",
		"GetCompShareCFSUpgradePrice",
		"GetCompShareCFSRefundPrice",
		"DescribeCFS",
		"CreateCFS",
		"ResizeCFS":
		return true
	default:
		return false
	}
}

func actionAllowsBackendAzGroup(action string) bool {
	switch action {
	case "CheckCompShareNetOptimizer",
		"SyncCompShareNetOptimizer",
		"GetCompShareInstancePrice",
		"GetCompShareInstanceUserPrice",
		"GetCompShareInstanceUpgradePrice",
		"GetCompShareAttachedDiskUpgradePrice",
		"CheckCompShareResizeAttachedDisk",
		"ResizeCompShareInstance",
		"ResizeCompShareDisk",
		"GetCompShareCFSPrice",
		"CreateCFS":
		return true
	default:
		return false
	}
}

func actionAllowsBackendZoneRegion(action string) bool {
	switch action {
	case "CheckCompShareNetOptimizer",
		"SyncCompShareNetOptimizer":
		return true
	default:
		return false
	}
}

func actionAllowsBackendIdentity(action string) bool {
	switch action {
	case "CheckCompShareNetOptimizer",
		"SyncCompShareNetOptimizer",
		"DescribeCompShareGpuInventory",
		"DescribeCFS",
		"GetCompShareCFSPrice",
		"GetCompShareCFSUpgradePrice",
		"GetCompShareCFSRefundPrice",
		"CreateCFS",
		"ResizeCFS":
		return true
	default:
		return false
	}
}

func routeForAction(action string) ActionRoute {
	switch {
	case action == "GetGPUSpecs" || action == "GetGPURecommendation" || action == "GetModelVRAMRequirement":
		return ActionRouteKnowledge
	case action == "SearchKnowledge":
		// Agentic-RAG read-only tool (P3). Routed locally on a dedicated engine
		// branch (like the GetGPUSpecs knowledge tools), never through
		// SafeToolExecutor — its Route is Knowledge, not external_api.
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
	case readExpensiveDefaultActions[action]:
		return ActionClassReadExpensiveDefault
	default:
		return ActionClassReadCheap
	}
}

var readExpensiveDefaultActions = map[string]bool{
	"DescribeCompShareInstance":               true,
	"GetCompShareInstancePrice":               true,
	"GetCompShareInstanceUserPrice":           true,
	"GetCompShareRefundPrice":                 true,
	"DescribeCompShareJupyterToken":           true,
	"DescribeCFS":                             true,
	"GetCompShareCFSPrice":                    true,
	"GetCompShareCFSUpgradePrice":             true,
	"GetCompShareCFSRefundPrice":              true,
	"DescribeAvailableCompShareInstanceTypes": true,
	"DescribeCompShareGpuInventory":           true,
	"CheckCompShareResourceCapacity":          true,
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

func appendAllowedParam(params []string, key string) []string {
	for _, p := range params {
		if p == key {
			return params
		}
	}
	return append(params, key)
}
