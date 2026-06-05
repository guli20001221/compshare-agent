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
	mutating := getenv("COMPSHARE_ENABLE_MUTATING_TOOLS") == "1"
	if mutating {
		log.Printf("runtime: HTTP mutating tools enabled (COMPSHARE_ENABLE_MUTATING_TOOLS=1)")
	}
	useSkillExecutor, unknownSkillExecutor := useSkillExecutorFromEnv(getenv)
	if unknownSkillExecutor != "" {
		log.Printf("warning: ignoring unknown USE_SKILL_EXECUTOR value %q", unknownSkillExecutor)
	}
	engine.SetSkillExecutorEnabled(useSkillExecutor)
	diagnosisPilots, unknownDiagnosisPilots := skillExecutorDiagnosisPilotsFromEnv(getenv)
	for _, value := range unknownDiagnosisPilots {
		log.Printf("warning: ignoring unknown USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS value %q", value)
	}
	engine.SetSkillExecutorDiagnosisPilots(diagnosisPilots)
	if useSkillExecutor {
		log.Printf("runtime: HTTP skill executor enabled (USE_SKILL_EXECUTOR=1, diagnosis_pilots=%v)", diagnosisPilots)
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
	return llm.NewRouter(cfg.Agent.LLM, llm.TierOverridesFromConfig(cfg.Agent.TierRouting))
}

func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv getenvFunc) error {
	cutoverIntents, unknownCutover := intentPlannerCutoverIntentsFromEnv(getenv)
	for _, value := range unknownCutover {
		log.Printf("warning: ignoring unknown USE_INTENT_PLANNER_FOR value %q", value)
	}
	sessionFactContext, unknownSessionFactContext := sessionFactContextEnabledFromEnv(getenv)
	if unknownSessionFactContext != "" {
		log.Printf("warning: ignoring unknown USE_SESSION_FACT_CONTEXT value %q", unknownSessionFactContext)
	}
	deps.SessionFactContextEnabled = sessionFactContext
	if sessionFactContext {
		log.Printf("runtime: HTTP session fact context enabled (USE_SESSION_FACT_CONTEXT=1)")
	}
	reactResultProjection, unknownReactResultProjection := reactResultProjectionEnabledFromEnv(getenv)
	if unknownReactResultProjection != "" {
		log.Printf("warning: ignoring unknown USE_REACT_RESULT_PROJECTION value %q", unknownReactResultProjection)
	}
	deps.ReactResultProjectionEnabled = reactResultProjection
	if reactResultProjection {
		log.Printf("runtime: HTTP ReAct result projection enabled (USE_REACT_RESULT_PROJECTION=1)")
	}
	reactHistoryCompaction, unknownReactHistoryCompaction := reactHistoryCompactionEnabledFromEnv(getenv)
	if unknownReactHistoryCompaction != "" {
		log.Printf("warning: ignoring unknown USE_REACT_HISTORY_COMPACTION value %q", unknownReactHistoryCompaction)
	}
	deps.ReactHistoryCompactionEnabled = reactHistoryCompaction
	if reactHistoryCompaction {
		log.Printf("runtime: HTTP ReAct history compaction enabled (USE_REACT_HISTORY_COMPACTION=1)")
	}
	intentScopedReActPrompt, unknownIntentScopedReActPrompt := intentScopedReActPromptEnabledFromEnv(getenv)
	if unknownIntentScopedReActPrompt != "" {
		log.Printf("warning: ignoring unknown USE_INTENT_SCOPED_REACT_PROMPT value %q", unknownIntentScopedReActPrompt)
	}
	deps.IntentScopedReActPromptEnabled = intentScopedReActPrompt
	if intentScopedReActPrompt {
		log.Printf("runtime: HTTP intent-scoped ReAct prompt enabled (USE_INTENT_SCOPED_REACT_PROMPT=1)")
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

	plannerStructuredOutput, unknownPlannerStructuredOutput := plannerStructuredOutputModeFromEnv(getenv)
	if unknownPlannerStructuredOutput != "" {
		log.Printf("warning: ignoring unknown PLANNER_STRUCTURED_OUTPUT value %q", unknownPlannerStructuredOutput)
	}

	cutoverEnabled := len(cutoverIntents) > 0
	if cutoverEnabled || knowledgeEnabled {
		deps.IntentPlanner = newCLIPlannerWithStructuredOutput(cfg, plannerStructuredOutput)
		deps.IntentPlannerModel = cfg.Agent.LLM.Model
		enabled, cutover := engine.BuildIntentPlannerMaps(cutoverIntents)
		deps.IntentPlannerEnabledIntents = enabled
		deps.IntentCutoverIntents = cutover
	}
	return nil
}
