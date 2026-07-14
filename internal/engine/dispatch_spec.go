package engine

import (
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
)

type ResponseContract string

const (
	ResponseAgent          ResponseContract = "agent"
	ResponseGrounded       ResponseContract = "grounded"
	ResponsePolicyTerminal ResponseContract = "policy_terminal"
)

type SafetyClass string

const (
	SafetyClassStandard SafetyClass = "standard"
	SafetyClassPolicy   SafetyClass = "policy"
)

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
	Intent           intent.Intent
	ExecutionMode    intent.ExecutionPath
	ToolScope        tools.ToolScope
	ResponseContract ResponseContract
	SafetyClass      SafetyClass
	AgentSkillName   string
}

// specForIntent is the single immutable per-intent dispatch contract. Engine
// consumers read it instead of maintaining parallel response, authorization or
// agent-skill maps.
func specForIntent(i intent.Intent) DispatchSpec {
	spec := DispatchSpec{
		Intent:           i,
		ExecutionMode:    intent.PlannedExecutionPathForIntent(i),
		ToolScope:        dispatchToolScope(i),
		ResponseContract: ResponseAgent,
		SafetyClass:      SafetyClassStandard,
	}
	if i == intent.IntentBillingAccountUnsupported {
		spec.ResponseContract = ResponsePolicyTerminal
		spec.SafetyClass = SafetyClassPolicy
	} else if i == intent.IntentResourceInfo || i == intent.IntentMonitorQuery || i == intent.IntentMonitorHistory || intent.IsRoutingIntent(i) {
		spec.ResponseContract = ResponseGrounded
	}
	switch i {
	case intent.IntentDeployModel:
		spec.AgentSkillName = "deploy_model"
	case intent.IntentCreateInstance:
		spec.AgentSkillName = "create_instance"
	}
	return spec
}

func dispatchToolScope(i intent.Intent) tools.ToolScope {
	if i == intent.IntentKnowledgeQA {
		return tools.ToolScope{Mode: tools.ToolScopeNamed, Names: []string{"SearchKnowledge"}}
	}
	if subset := intent.IntentToolSubset(i); len(subset) > 0 {
		return tools.ToolScope{Mode: tools.ToolScopeNamed, Names: append([]string(nil), subset...)}
	}
	return tools.ToolScope{Mode: tools.ToolScopeReadOnlyFull}
}
