package intent

import (
	"context"

	"github.com/compshare-agent/internal/routing"
)

// skill_registry.go is the bridge between the generated skill registry
// (internal/skills, metadata-only) and package intent (which owns the capability
// handler funcs). The skill registry is the sole capability dispatch +
// planner-prompt source. It keeps internal/skills import-cycle-free: skills
// stores handler_key as a STRING, and the string→func binding lives here.

// isCapabilityIntentSkill is the skill-registry-sourced IsCapabilityIntent: an
// intent is a capability iff a generated capability skill (non-empty intent_label)
// declares it.
func isCapabilityIntentSkill(i Intent) bool {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel != "" && Intent(route.IntentLabel) == i {
			return true
		}
	}
	return false
}

// dispatchCapabilitySkill is the skill-registry-sourced DispatchCapability: it
// resolves the intent's capability skill, binds its handler_key to the Go handler
// via CapabilityHandlerForKey, and invokes it. The func pointer is identical to
// the legacy registry's (pinned by TestCapabilityHandlerByKey_MatchesRegistry).
func (h *DemoHandler) dispatchCapabilitySkill(ctx context.Context, req HandlerRequest) HandlerResult {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" || Intent(route.IntentLabel) != req.Plan.Intent {
			continue
		}
		if handler := CapabilityHandlerForKey(route.HandlerKey); handler != nil {
			return handler(ctx, h, req)
		}
		break
	}
	return FallbackBeforeTool(FallbackValidation)
}

// capabilityHandlerFunc is the capability dispatch handler signature.
type capabilityHandlerFunc = func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult

// capabilityHandlerByKey binds each capability skill's handler_key (the string in
// the SKILL.md frontmatter) to its Go handler func. internal/skills stores only
// the string key; this map is the func-pointer side of the intent-to-handler binding.
// Drift (against the expected per-intent handlers and against the skill-declared
// handler_keys) is caught by skill_registry_test.go.
var capabilityHandlerByKey = map[string]capabilityHandlerFunc{
	"handleGPUSpecsQuery":        handleGPUSpecsQuery,
	"handleStockAvailability":    handleStockAvailability,
	"handleNetAcceleratorStatus": handleNetAcceleratorStatus,
	"handlePlatformImageList":    handlePlatformImageList,
	"handleCustomImageList":      handleCustomImageList,
	"handleCommunityImageList":   handleCommunityImageList,
	"handlePricingQuery":         handlePricingQuery,
}

// CapabilityHandlerForKey resolves a skill handler_key to its capability handler
// func, or nil for an unknown key.
func CapabilityHandlerForKey(key string) capabilityHandlerFunc {
	return capabilityHandlerByKey[key]
}

// skillRegistryCapabilityMetadata projects the generated skill registry into the
// []CapabilityMetadata shape, restricted to capability skills (non-empty
// intent_label) and ORDERED by capabilityIntentOrder (via CapabilityIntents) so the
// planner-prompt fragments stay byte-identity-pinned (systemPromptSHA256Baseline).
func skillRegistryCapabilityMetadata() []CapabilityMetadata {
	byIntent := make(map[Intent]*routing.Route)
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" {
			continue
		}
		byIntent[Intent(route.IntentLabel)] = route
	}
	out := make([]CapabilityMetadata, 0, len(byIntent))
	for _, intentValue := range CapabilityIntents() {
		route, ok := byIntent[intentValue]
		if !ok {
			continue
		}
		out = append(out, routeToCapabilityMetadata(route))
	}
	return out
}

// routeToCapabilityMetadata maps a generated deterministic route into the
// legacy CapabilityMetadata shape. required_tools[0] supplies the singular
// RequiredTool the prompt builder consumes.
func routeToCapabilityMetadata(route *routing.Route) CapabilityMetadata {
	var requiredTool string
	if len(route.RequiredTools) > 0 {
		requiredTool = route.RequiredTools[0]
	}
	examples := make([]CapabilityPlannerExample, 0, len(route.PlannerExamples))
	for _, ex := range route.PlannerExamples {
		examples = append(examples, CapabilityPlannerExample{Question: ex.Question, Confidence: ex.Confidence})
	}
	return CapabilityMetadata{
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
