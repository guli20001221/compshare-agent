package engine

import "github.com/compshare-agent/internal/intent"

// DispatchSpec is the NOMINAL, pure dispatch contract for an intent: a flat,
// table-able projection of the per-intent routing facts that are pure functions
// of the intent label alone — the nominal runtime lane, the ReAct tool subset,
// and the agent-skill name (if any).
//
// It is deliberately NOT the EFFECTIVE dispatch. Runtime guards (flag gates,
// snapshot counts, screenshot suppression, per-engine enables, the knowledge_qa
// agent-loop AND-gate) stay in the engine's dispatch chain, resolved separately;
// none of them belongs here. Red line: no DispatchSpec field may close over
// runtime state — that would re-create a second router inside the table. See
// docs/plans/2026-06-09-intent-router-dispatch-restructure.md (§4.2, §7) and
// docs/adr/009-intent-router-dispatch-contract.md (D2).
//
// PR1 is a read-only projection with parity tests only: nothing dispatches off
// DispatchSpec yet. It exists so later PRs can read routing truth from one
// contract instead of the scattered planner-prompt directives + engine if-chain.
type DispatchSpec struct {
	Intent         intent.Intent
	NominalLane    intent.RuntimeForm
	ToolSubset     []string
	AgentSkillName string
}

// specForIntent projects the three live per-intent surfaces into a DispatchSpec.
// It is a pure function of the intent: the nominal lane from
// intent.PlannedRuntimeFormForIntent, the ReAct tool subset from
// intent.IntentToolSubset (defensively copied so a caller holding the spec cannot
// mutate the surface every other consumer reads), and the agent-skill name from
// the agentSkillForIntent map (engine.go; "" for non-agent intents). It must stay
// byte-equal to those surfaces — TestSpecForIntent_MatchesExistingSurfaces locks
// the parity across intent.AllIntents().
func specForIntent(i intent.Intent) DispatchSpec {
	return DispatchSpec{
		Intent:         i,
		NominalLane:    intent.PlannedRuntimeFormForIntent(i),
		ToolSubset:     append([]string(nil), intent.IntentToolSubset(i)...),
		AgentSkillName: agentSkillForIntent[i],
	}
}
