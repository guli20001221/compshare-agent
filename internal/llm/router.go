package llm

import (
	"fmt"

	"github.com/compshare-agent/internal/config"
)

// Tier identifies the task-complexity tier defined in ADR-001.
// fast: single-API queries / read-only catalog lookups.
// knowledge: platform docs / FAQ / concept Q&A via RAG.
// agent: multi-step tasks with side effects (deploy, SSH triage).
type Tier string

const (
	TierFast      Tier = "fast"
	TierKnowledge Tier = "knowledge"
	TierAgent     Tier = "agent"
)

// AllTiers enumerates the valid tiers in canonical order
// (fast → knowledge → agent). NewRouter iterates this slice to populate
// the per-tier client map. Order is held stable so downstream iteration
// (per-tier cost reports, observability column ordering, table tests)
// stays consistent across versions. Planner-layer fallback direction
// (tier mis-classification mitigation) lives in ADR-001 Risks, not here.
var AllTiers = []Tier{TierFast, TierKnowledge, TierAgent}

// Router holds one *Client per tier. Construct once at boot via NewRouter,
// then share across sessions. For/Model/Capability panic on unknown tiers
// — misrouting is a programmer bug, not a runtime fallback condition
// (ADR-001 fallback lives in the planner, not in this layer).
type Router struct {
	clients map[Tier]*Client
	configs map[Tier]config.LLMConfig
}

// NewRouter resolves each tier's effective LLMConfig and constructs its
// Client.
//
// Resolution rule:
//   - Start from base (agent.llm in YAML — the default model).
//   - If tierOverrides[tier] is present, non-zero fields replace the
//     corresponding field from base (model / base_url / api_key).
//     Unspecified fields inherit from base.
//   - If tierOverrides is empty or missing the tier, the tier uses base
//     as-is. Backward-compat: legacy configs with no tier_routing block
//     keep working, all tiers share one client.
//
// Returns an error if base.Model is empty — every tier needs some model
// to talk to.
//
// PRECONDITION: callers in the cmd/ binary path get base from
// config.Load, which has already resolved ${ENV_VAR} placeholders and
// rejected literal/missing api_key (resolveRequiredSecret). Direct
// callers from tests or future packages (e.g. B6 orchestrator) MUST
// pre-validate the LLMConfig themselves; NewRouter only guards the
// Model field. Empty BaseURL / APIKey will surface as the first LLM
// call's authentication failure, not at Router construction.
func NewRouter(base config.LLMConfig, tierOverrides map[Tier]config.LLMConfig) (*Router, error) {
	if base.Model == "" {
		return nil, fmt.Errorf("llm.NewRouter: base.Model is required")
	}
	r := &Router{
		clients: make(map[Tier]*Client, len(AllTiers)),
		configs: make(map[Tier]config.LLMConfig, len(AllTiers)),
	}
	for _, tier := range AllTiers {
		effective := base
		if override, ok := tierOverrides[tier]; ok {
			if override.Model != "" {
				effective.Model = override.Model
			}
			if override.BaseURL != "" {
				effective.BaseURL = override.BaseURL
			}
			if override.APIKey != "" {
				effective.APIKey = override.APIKey
			}
		}
		r.configs[tier] = effective
		r.clients[tier] = NewClient(effective)
	}
	return r, nil
}

// For returns the Client for tier. Panics on unknown tier. Since
// NewRouter unconditionally populates entries for every Tier in
// AllTiers, this branch is reachable only when callers cast a raw
// string into Tier (e.g. Tier("knowlege")) — that is the specific bug
// the panic catches. Tier mis-classification from the planner is a
// SEPARATE concern handled at the planner layer (ADR-001 fallback
// direction), not by panic here.
func (r *Router) For(tier Tier) *Client {
	c, ok := r.clients[tier]
	if !ok {
		panic(fmt.Sprintf("llm.Router.For: unknown tier %q (use TierFast/TierKnowledge/TierAgent)", tier))
	}
	return c
}

// Model returns the model name configured for tier. Used by observability
// to populate the model attribution at the actual call site —
// trace.task_tier sits at the TraceRecord top level (ADR-002 acceptance
// #4 schema slot, B4 populates), while the per-call model lands inside
// the nested PlannerTrace.Model / RendererTrace.Model fields that
// already exist. There is no top-level trace.model field. Panics on
// unknown tier.
func (r *Router) Model(tier Tier) string {
	c, ok := r.configs[tier]
	if !ok {
		panic(fmt.Sprintf("llm.Router.Model: unknown tier %q", tier))
	}
	return c.Model
}

// Capability returns the capability matrix for the tier's effective model.
// Convenience wrapper around LookupCapability so callers don't have to
// thread BaseURL+Model through. Panics on unknown tier.
func (r *Router) Capability(tier Tier) Capability {
	c, ok := r.configs[tier]
	if !ok {
		panic(fmt.Sprintf("llm.Router.Capability: unknown tier %q", tier))
	}
	return LookupCapability(c.BaseURL, c.Model)
}
