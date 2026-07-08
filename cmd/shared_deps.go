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
	"github.com/compshare-agent/internal/tools"
)

func buildHTTPServerPool(cfg *config.Config, messageStore store.MessageStore, getenv getenvFunc) (*agentpool.Pool, error) {
	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv)
	if err != nil {
		return nil, err
	}
	return agentpool.NewWithDeps(deps, messageStore, agentpool.Options{
		Capacity:             cfg.Agent.HTTP.PoolCapacity,
		IdleTTL:              cfg.Agent.HTTP.PoolIdleTTL,
		MutatingToolsEnabled: mutating,
	}), nil
}

// configureSharedDepsFromEnv builds the shared engine dependencies and applies
// every runtime feature flag exactly as the HTTP server boot does. Extracted
// from buildHTTPServerPool so the in-process behavioral-gate test
// (cmd/behavioral_gate_test.go) can drive an engine wired identically to
// production — the gate would be worthless if it tested a hand-rolled wiring
// that drifted from the server's. Returns the configured deps and whether
// mutating tools are enabled. Behavior is byte-identical to the original inline
// body of buildHTTPServerPool.
func configureSharedDepsFromEnv(cfg *config.Config, getenv getenvFunc) (*engine.SharedDeps, bool, error) {
	unifiedCreate, unknownUnifiedCreate := unifiedCreateEnabledFromEnv(getenv)
	if unknownUnifiedCreate != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_UNIFIED_CREATE value %q", unknownUnifiedCreate)
	}
	engine.SetUnifiedCreateEnabled(unifiedCreate)
	if unifiedCreate {
		log.Printf("runtime: HTTP unified create-family route enabled (default-on; set COMPSHARE_UNIFIED_CREATE=0 to disable; create_instance prompt/schema active)")
	}
	contextContinuation, unknownContextContinuation := contextContinuationEnabledFromEnv(getenv)
	if unknownContextContinuation != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_CONTEXT_CONTINUATION value %q", unknownContextContinuation)
	}
	engine.SetContextContinuationEnabled(contextContinuation)
	if contextContinuation {
		log.Printf("runtime: HTTP context continuation enabled (COMPSHARE_CONTEXT_CONTINUATION default-on; disable with =0)")
	}

	deps, err := engine.NewSharedDeps(cfg)
	if err != nil {
		return nil, false, fmt.Errorf("shared deps: %w", err)
	}
	if err := applySharedDepsFromEnv(deps, cfg, getenv); err != nil {
		return nil, false, fmt.Errorf("apply shared deps from env: %w", err)
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
	agenticSearch, unknownAgenticSearch := agenticSearchKnowledgeEnabledFromEnv(getenv)
	if unknownAgenticSearch != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE value %q", unknownAgenticSearch)
	}
	tools.SetAgenticSearchKnowledgeEnabled(agenticSearch)
	if agenticSearch {
		log.Printf("runtime: HTTP agentic SearchKnowledge enabled (COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE default-on; disable with =0)")
	} else {
		log.Printf("runtime: HTTP agentic SearchKnowledge disabled (COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE=0)")
	}
	groundedValidator, unknownGroundedValidator := groundedAnswerValidatorEnabledFromEnv(getenv)
	if unknownGroundedValidator != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_RAG_GROUNDED_VALIDATOR value %q", unknownGroundedValidator)
	}
	engine.SetGroundedAnswerValidatorEnabled(groundedValidator)
	if groundedValidator {
		log.Printf("runtime: HTTP grounded-answer validator enabled (COMPSHARE_RAG_GROUNDED_VALIDATOR=1; cite-or-refuse on agentic SearchKnowledge)")
	}
	domainMatchGuard, unknownDomainMatchGuard := domainMatchGuardEnabledFromEnv(getenv)
	if unknownDomainMatchGuard != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_RAG_DOMAIN_MATCH_GUARD value %q", unknownDomainMatchGuard)
	}
	engine.SetDomainMatchGuardEnabled(domainMatchGuard)
	if domainMatchGuard {
		log.Printf("runtime: HTTP wrong-domain refuse arm enabled (COMPSHARE_RAG_DOMAIN_MATCH_GUARD=1; #5 cite-relevance)")
	}
	flashKnowledgeRouteGuard, unknownFlashKnowledgeRouteGuard := flashKnowledgeRouteGuardEnabledFromEnv(getenv)
	if unknownFlashKnowledgeRouteGuard != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD value %q", unknownFlashKnowledgeRouteGuard)
	}
	engine.SetFlashKnowledgeRouteGuardEnabled(flashKnowledgeRouteGuard)
	if flashKnowledgeRouteGuard {
		log.Printf("runtime: HTTP flash knowledge route guard enabled (COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD=1; default-off fallback)")
	}
	createPrefExtractor, unknownCreatePrefExtractor := createPreferenceExtractorEnabledFromEnv(getenv)
	if unknownCreatePrefExtractor != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_CREATE_PREF_EXTRACTOR value %q", unknownCreatePrefExtractor)
	}
	engine.SetCreatePreferenceExtractionEnabled(createPrefExtractor)
	if createPrefExtractor {
		log.Printf("runtime: HTTP create/deploy preference extractor enabled (COMPSHARE_CREATE_PREF_EXTRACTOR default-on; disable with =0)")
	}
	knowledgeQAAgentLoop, unknownKnowledgeQAAgentLoop := knowledgeQAAgentLoopEnabledFromEnv(getenv)
	if unknownKnowledgeQAAgentLoop != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP value %q", unknownKnowledgeQAAgentLoop)
	}
	engine.SetKnowledgeQAAgentLoopEnabled(knowledgeQAAgentLoop)
	if knowledgeQAAgentLoop {
		log.Printf("runtime: HTTP knowledge_qa agent-loop route enabled (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP default-on; forced SearchKnowledge first hop, terminal RAG bypassed; disable with =0)")
	} else {
		log.Printf("runtime: HTTP knowledge_qa agent-loop route disabled (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP=0; deterministic terminal RAG route)")
	}
	disciplinedKnowledgeQASynthesis, unknownDisciplinedKnowledgeQASynthesis := disciplinedKnowledgeQASynthesisEnabledFromEnv(getenv)
	if unknownDisciplinedKnowledgeQASynthesis != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS value %q", unknownDisciplinedKnowledgeQASynthesis)
	}
	engine.SetDisciplinedKnowledgeQASynthesisEnabled(disciplinedKnowledgeQASynthesis)
	if disciplinedKnowledgeQASynthesis {
		log.Printf("runtime: HTTP disciplined knowledge_qa synthesis enabled (COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS default-on; terminal-style cited synthesis writes the final answer; disable with =0)")
	} else {
		log.Printf("runtime: HTTP disciplined knowledge_qa synthesis disabled (COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS=0; free ReAct write + cite-retry)")
	}
	kqaSelfRevision, unknownKQASelfRevision := kqaSelfRevisionEnabledFromEnv(getenv)
	if unknownKQASelfRevision != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_KQA_SELF_REVISION value %q", unknownKQASelfRevision)
	}
	engine.SetKQASelfRevisionEnabled(kqaSelfRevision)
	if kqaSelfRevision {
		log.Printf("runtime: HTTP knowledge_qa over-conservatism self-revision enabled (COMPSHARE_KQA_SELF_REVISION default-on; re-read grounded draft + commit, add no new facts, re-validated; disable with =0)")
	} else {
		log.Printf("runtime: HTTP knowledge_qa over-conservatism self-revision disabled (COMPSHARE_KQA_SELF_REVISION=0; disciplined draft delivered as-is)")
	}
	return deps, mutating, nil
}

// buildLLMRouter constructs a per-tier LLM Router from cfg. Called once at
// process boot — cli.go and shared_deps.go each call it for their own
// path (CLI vs HTTP). The Router is cheap (2-3 *Client structs for the
// default 3-tier setup) so building twice in different binary entry
// points is acceptable.
//
// When cfg.Agent.TierRouting is empty, all tiers fall back to
// cfg.Agent.LLM.Model (backward compat per ADR-002 Acceptance #5).
func buildLLMRouter(cfg *config.Config) (*llm.ModelRouter, error) {
	return llm.NewModelRouter(cfg.Agent.LLM, llm.TierOverridesFromConfig(cfg.Agent.TierRouting))
}

func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv getenvFunc) error {
	routeIntents, unknownRoute := intentPlannerRouteIntentsFromEnv(getenv)
	for _, value := range unknownRoute {
		log.Printf("warning: ignoring unknown COMPSHARE_DIRECT_DISPATCH_INTENTS value %q", value)
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
		deps.GroundedGenerator = renderer.NewGroundedGenerator(router.For(llm.TierKnowledge))
		deps.GroundedGeneratorModel = router.Model(llm.TierKnowledge)
		deps.FastTemplateRenderer = groundedMode == "fast_template"
	}

	plannerStructuredOutput, unknownPlannerStructuredOutput := plannerStructuredOutputModeFromEnv(getenv)
	if unknownPlannerStructuredOutput != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT value %q", unknownPlannerStructuredOutput)
	}

	routeEnabled := len(routeIntents) > 0
	if routeEnabled || knowledgeEnabled {
		deps.IntentPlanner = newCLIPlannerWithStructuredOutput(cfg, plannerStructuredOutput)
		deps.IntentPlannerModel = cfg.Agent.LLM.Model
		enabled, routeMap := engine.BuildIntentPlannerMaps(routeIntents)
		deps.IntentPlannerEnabledIntents = enabled
		deps.IntentRouteIntents = routeMap
	}
	return nil
}
