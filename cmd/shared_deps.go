package main

import (
	"fmt"
	"log"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
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
	log.Printf("runtime: HTTP agentic SearchKnowledge enabled (single production knowledge path)")
	domainMatchGuard, unknownDomainMatchGuard := domainMatchGuardEnabledFromEnv(getenv)
	if unknownDomainMatchGuard != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_RAG_DOMAIN_MATCH_GUARD value %q", unknownDomainMatchGuard)
	}
	engine.SetDomainMatchGuardEnabled(domainMatchGuard)
	if domainMatchGuard {
		log.Printf("runtime: HTTP wrong-domain refuse arm enabled (COMPSHARE_RAG_DOMAIN_MATCH_GUARD=1; #5 cite-relevance)")
	}
	deterministicRender, unknownDeterministicRender := agentDeterministicRenderEnabledFromEnv(getenv)
	if unknownDeterministicRender != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_AGENT_DETERMINISTIC_RENDER value %q", unknownDeterministicRender)
	}
	engine.SetAgentDeterministicRenderEnabled(deterministicRender)
	if deterministicRender {
		log.Printf("runtime: agent-loop deterministic instance rendering enabled (COMPSHARE_AGENT_DETERMINISTIC_RENDER default-on; disable with =0; instance tables are rendered from the payload, not retyped by the model)")
	}
	return deps, mutating, nil
}

func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv getenvFunc) error {
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

	// HTTP sessions use the central Agent runtime. An intent router would add a
	// second semantic model before the Agent and could once again delete or hide
	// context, so it is deliberately not constructed on the server path.
	return nil
}
