package main

import (
	"fmt"
	"log"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/renderer"
	"github.com/compshare-agent/internal/store"
)

func buildHTTPServerPool(cfg *config.Config, messageStore store.MessageStore, getenv getenvFunc) (*agentpool.Pool, error) {
	deps, err := engine.NewSharedDeps(cfg)
	if err != nil {
		return nil, fmt.Errorf("shared deps: %w", err)
	}
	if err := applySharedDepsFromEnv(deps, cfg, getenv); err != nil {
		return nil, fmt.Errorf("apply shared deps from env: %w", err)
	}
	mutating := getenv("COMPSHARE_ENABLE_MUTATING_TOOLS") == "1"
	if mutating {
		log.Printf("runtime: HTTP mutating tools enabled (COMPSHARE_ENABLE_MUTATING_TOOLS=1)")
	}
	useSkillRegistry, unknownSkillRegistry := useSkillRegistryFromEnv(getenv)
	if unknownSkillRegistry != "" {
		log.Printf("warning: ignoring unknown USE_SKILL_REGISTRY value %q", unknownSkillRegistry)
	}
	// Process-global, boot-only (B2b §8a — no hot reload; flips need a restart).
	intent.SetCapabilitySource(useSkillRegistry)
	if useSkillRegistry {
		log.Printf("runtime: HTTP capability source = skill_registry (USE_SKILL_REGISTRY=1)")
	}
	useSkillExecutor, unknownSkillExecutor := useSkillExecutorFromEnv(getenv)
	if unknownSkillExecutor != "" {
		log.Printf("warning: ignoring unknown USE_SKILL_EXECUTOR value %q", unknownSkillExecutor)
	}
	engine.SetSkillExecutorEnabled(useSkillExecutor)
	if useSkillExecutor {
		log.Printf("runtime: HTTP skill executor enabled (USE_SKILL_EXECUTOR=1)")
	}
	return agentpool.NewWithDeps(deps, messageStore, agentpool.Options{
		Capacity:             cfg.Agent.HTTP.PoolCapacity,
		IdleTTL:              cfg.Agent.HTTP.PoolIdleTTL,
		MutatingToolsEnabled: mutating,
	}), nil
}

// buildLLMRouter constructs a per-tier LLM Router from cfg. Called once at
// process boot — cli.go and shared_deps.go each call it for their own
// path (CLI vs HTTP). The Router is cheap (2-3 *Client structs for the
// default 3-tier setup) so building twice in different binary entry
// points is acceptable.
//
// When cfg.Agent.TierRouting is empty, all tiers fall back to
// cfg.Agent.LLM.Model (backward compat per ADR-002 Acceptance #5).
func buildLLMRouter(cfg *config.Config) (*llm.Router, error) {
	var overrides map[llm.Tier]config.LLMConfig
	if len(cfg.Agent.TierRouting) > 0 {
		overrides = make(map[llm.Tier]config.LLMConfig, len(cfg.Agent.TierRouting))
		for k, v := range cfg.Agent.TierRouting {
			overrides[llm.Tier(k)] = v
		}
	}
	return llm.NewRouter(cfg.Agent.LLM, overrides)
}

func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv getenvFunc) error {
	cutoverIntents, unknownCutover := intentPlannerCutoverIntentsFromEnv(getenv)
	for _, value := range unknownCutover {
		log.Printf("warning: ignoring unknown USE_INTENT_PLANNER_FOR value %q", value)
	}

	knowledgeRetrievalRequested, unknownKnowledge := knowledgeRetrievalModeFromEnv(getenv)
	if unknownKnowledge != "" {
		log.Printf("warning: ignoring unknown USE_KNOWLEDGE_RETRIEVAL value %q", unknownKnowledge)
	}
	retriever, knowledgeEnabled, knowledgeErr := knowledgeRetrieverFromEnv(getenv)
	if knowledgeRetrievalRequested && knowledgeErr != nil {
		return fmt.Errorf("RAG enabled but retrieval setup failed: %w", knowledgeErr)
	}
	if knowledgeEnabled {
		deps.KnowledgeRetriever = retriever
	}

	groundedMode, unknownGrounded := groundedRendererModeFromEnv(getenv)
	if unknownGrounded != "" {
		log.Printf("warning: ignoring unknown USE_GROUNDED_RENDERER value %q", unknownGrounded)
	}
	if groundedMode == "llm" || groundedMode == "fast_template" {
		router, err := buildLLMRouter(cfg)
		if err != nil {
			return fmt.Errorf("build LLM router: %w", err)
		}
		// The LLM renderer is built in both modes — under fast_template it
		// still serves knowledge/agent tiers; B3 only diverts fast-tier
		// catalog envelopes to the deterministic template.
		deps.GroundedRenderer = renderer.NewGroundedRenderer(router.For(llm.TierKnowledge))
		deps.GroundedRendererModel = router.Model(llm.TierKnowledge)
		deps.FastTemplateRenderer = groundedMode == "fast_template"
	}

	cutoverEnabled := len(cutoverIntents) > 0
	if cutoverEnabled || knowledgeEnabled {
		deps.IntentPlanner = newCLIPlanner(cfg)
		deps.IntentPlannerModel = cfg.Agent.LLM.Model
		enabled, cutover := engine.BuildIntentPlannerMaps(cutoverIntents)
		deps.IntentPlannerEnabledIntents = enabled
		deps.IntentCutoverIntents = cutover
	}
	return nil
}
