package intent

import (
	"context"

	"github.com/compshare-agent/internal/skills"
)

// skill_registry.go is the B2b P2 bridge between the generated skill registry
// (internal/skills, metadata-only) and package intent (which owns the capability
// handler funcs). It lets the skill registry serve as the capability dispatch +
// planner-prompt source while keeping internal/skills import-cycle-free: skills
// stores handler_key as a STRING, and the string→func binding lives here.
//
// B2b P2 wires the bridge + proves byte-identity against the legacy
// capabilityMetadata source. The USE_SKILL_REGISTRY flag (useSkillRegistrySource
// below, set at boot via SetCapabilitySource) selects whether IsCapabilityIntent /
// DispatchCapability / CapabilityPromptFragments read this generated registry or
// the legacy capabilityRegistry — default legacy, flag-on byte-identical.

// useSkillRegistrySource selects whether capability dispatch + planner-prompt
// fragments come from the generated skill registry (true) or the legacy
// capabilityRegistry/capabilityMetadata (false, default).
//
// BOOT-ONLY switch: set once via SetCapabilitySource before the engine serves
// any turn (the cmd layer reads USE_SKILL_REGISTRY at process boot — cli.go for
// the CLI, shared_deps.go for the server, mirroring COMPSHARE_ENABLE_MUTATING_TOOLS).
// It is then read — never written — by IsCapabilityIntent / DispatchCapability /
// CapabilityPromptFragments, so there is no concurrent-write race with sessions.
// Flipping it on the server requires a restart (process-global, no hot reload —
// B2b §8a). Flag-on is byte-identical to flag-off (the prompt fragments are byte-
// equal: TestSkillRegistryCapabilityFragments_ByteIdenticalToLegacy; dispatch
// resolves the same func pointers: TestCapabilityHandlerByKey_MatchesRegistry),
// so this changes no behavior — its purpose is to exercise the skill-registry
// path in production ahead of P3 / the agent tier.
var useSkillRegistrySource bool

// SetCapabilitySource selects the capability dispatch + planner-prompt source.
// MUST be called once at process boot, before the first Chat. Not safe to call
// concurrently with engine turns.
func SetCapabilitySource(useSkillRegistry bool) { useSkillRegistrySource = useSkillRegistry }

// CapabilitySourceIsSkillRegistry reports the active source (for runtime-line
// logging and tests).
func CapabilitySourceIsSkillRegistry() bool { return useSkillRegistrySource }

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

// capabilityHandlerFunc is the capability dispatch handler signature (identical
// to capabilityEntry.handler).
type capabilityHandlerFunc = func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult

// capabilityHandlerByKey binds each capability skill's handler_key (the string in
// the skill.md frontmatter) to its Go handler func. internal/skills stores only
// the string key — this map is the func-pointer side, mirroring
// capabilityRegistry's intent→handler binding. Drift (against the registry and
// against the skill-declared handler_keys) is caught by skill_registry_test.go.
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
// legacy []CapabilityMetadata shape, restricted to capability skills (non-empty
// intent_label) and ORDERED by capabilityRegistry registration order so the
// resulting planner-prompt fragments are byte-identical to the legacy
// capabilityMetadata source.
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
