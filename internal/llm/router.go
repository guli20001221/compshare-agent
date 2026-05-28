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

// AllTiers enumerates valid tiers in ADR-001 fallback-preference order
// (fast > knowledge > agent: degrading agent→fast is a quality drop,
// over-classifying fast→agent is just cost waste — favor cheap).
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

// For returns the Client for tier. Panics on unknown tier — callers must
// use one of the package-level Tier constants.
func (r *Router) For(tier Tier) *Client {
	c, ok := r.clients[tier]
	if !ok {
		panic(fmt.Sprintf("llm.Router.For: unknown tier %q (use TierFast/TierKnowledge/TierAgent)", tier))
	}
	return c
}

// Model returns the model name configured for tier. Used by observability
// (trace.task_tier + trace.model — ADR-002 acceptance #4). Panics on
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
