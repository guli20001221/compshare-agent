package main

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

func buildHTTPServerPool(cfg *config.Config, messageStore store.MessageStore, getenv getenvFunc, db *sql.DB) (*agentpool.Pool, error) {
	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv, db)
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
func configureSharedDepsFromEnv(cfg *config.Config, getenv getenvFunc, db *sql.DB) (*engine.SharedDeps, bool, error) {
	deps, err := engine.NewSharedDeps(cfg)
	if err != nil {
		return nil, false, fmt.Errorf("shared deps: %w", err)
	}
	if err := applySharedDepsFromEnv(deps, cfg, getenv); err != nil {
		return nil, false, fmt.Errorf("apply shared deps from env: %w", err)
	}
	// Wire the consent-gated SSH-ops runner (server path). Off by default; the gate returns nil with a
	// logged reason unless SSH_OPS + per-tenant STS + a DB all hold (see serverInstanceOpsRunner).
	// deps.ExternalExecutor (a tools.ToolExecutor) satisfies sshops.Describer for the credential fetch.
	instanceOps, err := serverInstanceOpsRunner(cfg, getenv, deps.ExternalExecutor, db)
	if err != nil {
		return nil, false, fmt.Errorf("ssh-ops runner: %w", err)
	}
	deps.InstanceOps = instanceOps
	mutating := getenv("COMPSHARE_ENABLE_MUTATING_TOOLS") == "1"
	if mutating {
		log.Printf("runtime: HTTP mutating tools enabled (COMPSHARE_ENABLE_MUTATING_TOOLS=1)")
	}
	log.Printf("runtime: HTTP agentic SearchKnowledge enabled (the Agent chooses retrieval in-loop)")
	canonicalTranscript, unknownCanonicalTranscript := canonicalTranscriptEnabledFromEnv(getenv)
	if unknownCanonicalTranscript != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_CANONICAL_TRANSCRIPT value %q", unknownCanonicalTranscript)
	}
	engine.SetCanonicalTranscriptEnabled(canonicalTranscript)
	if canonicalTranscript {
		log.Printf("runtime: canonical tool transcript replayed to the model (COMPSHARE_CANONICAL_TRANSCRIPT=1)")
	}
	return deps, mutating, nil
}

func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv getenvFunc) error {
	reactResultProjection, unknownReactResultProjection := reactResultProjectionEnabledFromEnv(getenv)
	if unknownReactResultProjection != "" {
		log.Printf("warning: ignoring unknown USE_REACT_RESULT_PROJECTION value %q", unknownReactResultProjection)
	}
	deps.ReactResultProjectionEnabled = reactResultProjection
	if reactResultProjection {
		log.Printf("runtime: HTTP ReAct result projection enabled (USE_REACT_RESULT_PROJECTION=1)")
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
	webSearchRequested, unknownWebSearch := webSearchEnabledFromEnv(getenv)
	if unknownWebSearch != "" {
		log.Printf("warning: ignoring unknown COMPSHARE_WEB_SEARCH_ENABLED value %q", unknownWebSearch)
	}
	searcher, webSearchEnabled, webSearchErr := webSearcherFromEnv(getenv)
	if webSearchRequested && webSearchErr != nil {
		return fmt.Errorf("web-search fallback enabled but setup failed: %w", webSearchErr)
	}
	if webSearchEnabled {
		deps.WebSearcher = searcher
		log.Printf("runtime: external web-search fallback enabled; it is exposed only after an empty curated-KB search")
	}

	// HTTP sessions use the central Agent runtime. An intent router would add a
	// second semantic model before the Agent and could once again delete or hide
	// context, so it is deliberately not constructed on the server path.
	return nil
}
