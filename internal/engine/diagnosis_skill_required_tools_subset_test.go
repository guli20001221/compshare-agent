package engine

import (
	"testing"

	"github.com/compshare-agent/internal/tools"
)

// TestBodyReadSkillRequiredToolsSubsetOfAllowedToolSubset closes the body-read
// half of the RequiredTools ⊆ ToolSubset invariant — the half that makes
// "a skill REQUESTS tools, the registry AUTHORIZES them" executable.
//
// Unlike a route (whose ToolSubset is the intent's static ReAct window), a
// piloted diagnosis skill runs through the body-driven executor, which builds
// its tool window from the skill's own RequiredTools and then filters it through
// the read-only visibility gate: runDiagnosisSkill loads the skill via
// findGeneratedSkill (engine.go:4044) and runs it under
// tools.VisibleRegistryForSubset(skill.RequiredTools, false) (engine.go:4086).
// That filter silently drops any tool that is mutating, workflow-routed, or
// unregistered. So if a skill declared such a tool as required, the body would
// reference a tool it was never granted — the skill body cannot self-authorize.
//
// This test asserts every RequiredTool of every piloted diagnosis skill survives
// that exact filter, i.e. the skill only requires tools the executor actually
// authorizes. It is bound to the real selection path, NOT a hypothetical
// CandidateSkills selector (none exists): the set iterated is
// KnownDiagnosisSkillExecutorPilots() — the same allowlist that gates
// diagnosisSkillExecutorPilotForAction (engine.go:4007) — and it loads + filters
// through the same two calls the executor uses at runtime.
func TestBodyReadSkillRequiredToolsSubsetOfAllowedToolSubset(t *testing.T) {
	pilots := KnownDiagnosisSkillExecutorPilots()
	if len(pilots) == 0 {
		t.Fatal("no piloted diagnosis skills to check; allowlist is empty")
	}

	for _, skillName := range pilots {
		skill, ok := findGeneratedSkill(skillName)
		if !ok {
			t.Errorf("piloted diagnosis skill %q not found in generated registry", skillName)
			continue
		}

		// Mirror engine.go:4086 exactly: the executor's read-only tool window.
		visible := tools.VisibleRegistryForSubset(skill.RequiredTools, false)
		authorized := make(map[string]struct{}, len(visible))
		for _, tool := range visible {
			if tool.Function == nil {
				continue
			}
			authorized[tool.Function.Name] = struct{}{}
		}

		for _, req := range skill.RequiredTools {
			if _, ok := authorized[req]; !ok {
				t.Errorf("diagnosis skill %q requires %q, but the read-only executor "+
					"visibility set does not authorize it (mutating/workflow/unregistered "+
					"tools are dropped by VisibleRegistryForSubset)", skillName, req)
			}
		}
	}
}
