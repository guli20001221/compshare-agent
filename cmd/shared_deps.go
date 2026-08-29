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
// the same operational configuration as the HTTP server boot. Extracted
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
	// Wire the user-targeted SSH-ops runner when a harness path is configured.
	// deps.ExternalExecutor (a tools.ToolExecutor) satisfies sshops.Describer for the credential fetch.
	instanceOps, err := serverInstanceOpsRunner(cfg, deps.ExternalExecutor, deps.KnowledgeRetriever, db)
	if err != nil {
		return nil, false, fmt.Errorf("ssh-ops runner: %w", err)
	}
	deps.InstanceOps = instanceOps
	mutating, unknown := mutatingToolsEnabledFromEnv(getenv)
	if unknown != "" {
		log.Printf("runtime: unknown COMPSHARE_ENABLE_MUTATING_TOOLS value %q; treating as disabled", unknown)
	}
	if mutating {
		log.Printf("runtime: HTTP mutating tools enabled (COMPSHARE_ENABLE_MUTATING_TOOLS=1)")
	}
	return deps, mutating, nil
}

func applySharedDepsFromEnv(deps *engine.SharedDeps, cfg *config.Config, getenv getenvFunc) error {
	retriever, knowledgeErr := knowledgeRetrieverFromEnv(getenv)
	if knowledgeErr != nil {
		return fmt.Errorf("RAG retrieval setup failed: %w", knowledgeErr)
	}
	deps.KnowledgeRetriever = retriever

	// HTTP sessions use the central Agent directly; no semantic router precedes it.
	return nil
}
