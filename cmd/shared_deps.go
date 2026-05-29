package main

import (
	"fmt"
	"log"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
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
	mutating, unknownMutating := mutatingToolsEnabledFromEnv(getenv)
	if unknownMutating != "" {
		log.Printf("warning: unknown COMPSHARE_ENABLE_MUTATING_TOOLS value %q (defaulting to enabled)", unknownMutating)
	}
	if mutating {
		log.Printf("runtime: HTTP mutating tools enabled (default ON; COMPSHARE_ENABLE_MUTATING_TOOLS=%q)", getenv("COMPSHARE_ENABLE_MUTATING_TOOLS"))
	} else {
		log.Printf("runtime: HTTP mutating tools DISABLED (read-only mode; COMPSHARE_ENABLE_MUTATING_TOOLS=%q)", getenv("COMPSHARE_ENABLE_MUTATING_TOOLS"))
	}
	return agentpool.NewWithDeps(deps, messageStore, agentpool.Options{
		Capacity:             cfg.Agent.HTTP.PoolCapacity,
		IdleTTL:              cfg.Agent.HTTP.PoolIdleTTL,
		MutatingToolsEnabled: mutating,
	}), nil
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
	if groundedMode == "llm" {
		deps.GroundedRenderer = renderer.NewGroundedRenderer(llm.NewClient(cfg.Agent.LLM))
		deps.GroundedRendererModel = cfg.Agent.LLM.Model
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
