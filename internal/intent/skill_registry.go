package intent

import (
	"context"

	"github.com/compshare-agent/internal/routing"
)

// skill_registry.go is the bridge between the generated skill registry
// (internal/skills, metadata-only) and package intent (which owns the route
// handler funcs). The skill registry is the sole route dispatch +
// planner-prompt source. It keeps internal/skills import-cycle-free: skills
// stores handler_key as a STRING, and the string→func binding lives here.

// isRoutingIntentRoute is the skill-registry-sourced IsRoutingIntent: an
// intent is a route iff a generated route skill (non-empty intent_label)
// declares it.
func isRoutingIntentRoute(i Intent) bool {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel != "" && Intent(route.IntentLabel) == i {
			return true
		}
	}
	return false
}

// dispatchRoute is the skill-registry-sourced DispatchRoute: it
// resolves the intent's route skill, binds its handler_key to the Go handler
// via RouteHandlerForKey, and invokes it. The func pointer is identical to
// the legacy registry's (pinned by TestRouteHandlerByKey_MatchesRegistry).
func (h *DemoHandler) dispatchRoute(ctx context.Context, req HandlerRequest) HandlerResult {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" || Intent(route.IntentLabel) != req.Plan.Intent {
			continue
		}
		if handler := RouteHandlerForKey(route.HandlerKey); handler != nil {
			return handler(ctx, h, req)
		}
		break
	}
	return FallbackBeforeTool(FallbackValidation)
}

// routeHandlerFunc is the route dispatch handler signature.
type routeHandlerFunc = func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult

// routeHandlerByKey binds each route skill's handler_key (the string in
// the SKILL.md frontmatter) to its Go handler func. internal/skills stores only
// the string key; this map is the func-pointer side of the intent-to-handler binding.
// Drift (against the expected per-intent handlers and against the skill-declared
// handler_keys) is caught by skill_registry_test.go.
var routeHandlerByKey = map[string]routeHandlerFunc{
	"handleGPUSpecsQuery":         handleGPUSpecsQuery,
	"handleStockAvailability":     handleStockAvailability,
	"handleNetAcceleratorStatus":  handleNetAcceleratorStatus,
	"handleRefundEstimate":        handleRefundEstimate,
	"handleCFSInfo":               handleCFSInfo,
	"handleImageTagCatalog":       handleImageTagCatalog,
	"handleModelRepositoryBrowse": handleModelRepositoryBrowse,
	"handleImageList":             handleImageList,
	"handlePricingQuery":          handlePricingQuery,
}

// RouteHandlerForKey resolves a skill handler_key to its route handler
// func, or nil for an unknown key.
func RouteHandlerForKey(key string) routeHandlerFunc {
	return routeHandlerByKey[key]
}

// skillRegistryRouteMetadata projects the generated skill registry into the
// []RouteMetadata shape, restricted to route skills (non-empty
// intent_label) and ORDERED by routingIntentOrder (via RoutingIntents) so the
// planner-prompt fragments stay byte-identity-pinned (systemPromptSHA256Baseline).
func skillRegistryRouteMetadata() []RouteMetadata {
	byIntent := make(map[Intent]*routing.Route)
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" {
			continue
		}
		byIntent[Intent(route.IntentLabel)] = route
	}
	out := make([]RouteMetadata, 0, len(byIntent))
	for _, intentValue := range RoutingIntents() {
		route, ok := byIntent[intentValue]
		if !ok {
			continue
		}
		out = append(out, routeToRouteMetadata(route))
	}
	return out
}

// routeToRouteMetadata maps a generated deterministic route into the
// legacy RouteMetadata shape. required_tools[0] supplies the singular
// RequiredTool the prompt builder consumes.
func routeToRouteMetadata(route *routing.Route) RouteMetadata {
	var requiredTool string
	if len(route.RequiredTools) > 0 {
		requiredTool = route.RequiredTools[0]
	}
	examples := make([]RoutePlannerExample, 0, len(route.PlannerExamples))
	for _, ex := range route.PlannerExamples {
		examples = append(examples, RoutePlannerExample{Question: ex.Question, Confidence: ex.Confidence, ImageSource: ImageSource(ex.ImageSource)})
	}
	return RouteMetadata{
		Name:              route.Name,
		IntentLabel:       route.IntentLabel,
		SkillGroup:        route.RouteGroup,
		RequiredTool:      requiredTool,
		ToolSubset:        route.ToolSubset,
		RequiredCitation:  route.RequiredCitation,
		PlannerDirectives: route.PlannerDirectives,
		PlannerExamples:   examples,
	}
}
