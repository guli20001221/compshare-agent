package intent

import (
	"context"

	"github.com/compshare-agent/internal/skills"
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
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel != "" && Intent(s.IntentLabel) == i {
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
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel == "" || Intent(s.IntentLabel) != req.Plan.Intent {
			continue
		}
		if handler := CapabilityHandlerForKey(s.HandlerKey); handler != nil {
			return handler(ctx, h, req)
		}
		break
	}
	return FallbackBeforeTool(FallbackValidation)
}

// capabilityHandlerFunc is the capability dispatch handler signature.
type capabilityHandlerFunc = func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult

// capabilityHandlerByKey binds each capability skill's handler_key (the string in
// the skill.md frontmatter) to its Go handler func. internal/skills stores only
// the string key — this map is the func-pointer side of the intent→handler binding.
// Drift (against the expected per-intent handlers and against the skill-declared
// handler_keys) is caught by skill_registry_test.go.
var capabilityHandlerByKey = map[string]capabilityHandlerFunc{
	"handleGPUSpecsQuery":      handleGPUSpecsQuery,
	"handleStockAvailability":  handleStockAvailability,
	"handlePlatformImageList":  handlePlatformImageList,
	"handleCustomImageList":    handleCustomImageList,
	"handleCommunityImageList": handleCommunityImageList,
	"handlePricingQuery":       handlePricingQuery,
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
	byIntent := make(map[Intent]*skills.Skill)
	for _, s := range skills.GeneratedSkills() {
		if s.IntentLabel == "" {
			continue
		}
		byIntent[Intent(s.IntentLabel)] = s
	}
	out := make([]CapabilityMetadata, 0, len(byIntent))
	for _, intentValue := range CapabilityIntents() {
		s, ok := byIntent[intentValue]
		if !ok {
			continue
		}
		out = append(out, skillToCapabilityMetadata(s))
	}
	return out
}

// skillToCapabilityMetadata maps a generated capability Skill into the legacy
// CapabilityMetadata shape. required_tools[0] supplies the singular RequiredTool
// the prompt builder consumes (§3: required_tool → required_tools[0]).
func skillToCapabilityMetadata(s *skills.Skill) CapabilityMetadata {
	var requiredTool string
	if len(s.RequiredTools) > 0 {
		requiredTool = s.RequiredTools[0]
	}
	examples := make([]CapabilityPlannerExample, 0, len(s.PlannerExamples))
	for _, ex := range s.PlannerExamples {
		examples = append(examples, CapabilityPlannerExample{Question: ex.Question, Confidence: ex.Confidence})
	}
	return CapabilityMetadata{
		Name:              s.Name,
		IntentLabel:       s.IntentLabel,
		SkillGroup:        s.SkillGroup,
		RequiredTool:      requiredTool,
		ToolSubset:        s.ReactToolSubset,
		RequiredCitation:  s.RequiredCitation,
		PlannerDirectives: s.PlannerDirectives,
		PlannerExamples:   examples,
	}
}
