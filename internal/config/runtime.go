package config

import (
	"strconv"
	"strings"
)

// This file moves the runtime feature flags that used to be configured ONLY via
// environment variables (read in cmd/trace.go / cmd/shared_deps.go / cmd/cli.go /
// cmd/server.go) into the YAML config, so a deployment can be configured from a
// single config.yaml with no env file. Precedence is "YAML wins, env is the
// fallback": a field set in YAML overrides the matching env var; a field omitted
// in YAML falls through to the env var, and if that is unset too, to the binary's
// documented built-in default (the cmd/ *FromEnv parsers — some default ON, some
// OFF). The bridge is (*Config).RuntimeGetenv, which overlays the YAML fields on
// top of os.Getenv so the existing cmd/ parsers keep reading through one getenv —
// the same shape as the pre-existing serverTraceGetenv shim for MYSQL_DSN.

// FeaturesConfig holds the boolean capability toggles. Each *bool is tri-state:
//
//   - nil            → field omitted in YAML; fall back to the env var, then to
//     the built-in default for that flag (NOT all default off — e.g.
//     knowledge verification / external_knowledge default ON).
//   - &true / &false → explicit value; it WINS over any env var.
//
// SkillExecutorDiagnosisPilots is a list (joined to the CSV the env parser
// expects) and only overrides when non-empty.
type FeaturesConfig struct {
	MutatingTools                *bool    `yaml:"mutating_tools"`                  // COMPSHARE_ENABLE_MUTATING_TOOLS (default off)
	DurableTurns                 *bool    `yaml:"durable_turns"`                   // COMPSHARE_DURABLE_TURNS (server-only, default off)
	ConfirmForm                  *bool    `yaml:"confirm_form"`                    // COMPSHARE_CONFIRM_FORM (server-only, default off)
	GuidedCreate                 *bool    `yaml:"guided_create"`                   // COMPSHARE_GUIDED_CREATE (server-only, default off)
	ExternalKnowledge            *bool    `yaml:"external_knowledge"`              // COMPSHARE_EXTERNAL_KNOWLEDGE (default ON)
	DomainMatchGuard             *bool    `yaml:"domain_match_guard"`              // COMPSHARE_RAG_DOMAIN_MATCH_GUARD (default off)
	SessionFactContext           *bool    `yaml:"session_fact_context"`            // USE_SESSION_FACT_CONTEXT (Go default off; deploy on)
	ReactResultProjection        *bool    `yaml:"react_result_projection"`         // USE_REACT_RESULT_PROJECTION (Go default off; deploy on)
	ReactHistoryCompaction       *bool    `yaml:"react_history_compaction"`        // USE_REACT_HISTORY_COMPACTION (Go default off; deploy on)
	UnifiedCreate                *bool    `yaml:"unified_create"`                  // COMPSHARE_UNIFIED_CREATE (default on; false disables)
	// DEPRECATED (convergence plan P5): the body-driven skill executor mechanism
	// was removed. These two fields are now INERT — nothing consumes the
	// USE_SKILL_EXECUTOR / USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS overrides they emit.
	// Kept only so an existing deploy config.yaml carrying these keys still loads
	// (yaml.Unmarshal is lenient); pending product sign-off to drop the config keys.
	SkillExecutor                *bool    `yaml:"skill_executor"`                  // USE_SKILL_EXECUTOR (inert)
	SkillExecutorDiagnosisPilots []string `yaml:"skill_executor_diagnosis_pilots"` // USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS (inert)
}

// RetrievalConfig holds the RAG / knowledge retrieval knobs.
// Empty string / zero int means "omitted — fall through to env, then default".
type RetrievalConfig struct {
	KnowledgeRetrieval     string `yaml:"knowledge_retrieval"`      // USE_KNOWLEDGE_RETRIEVAL: curated|off
	Mode                   string `yaml:"mode"`                     // RAG_RETRIEVAL_MODE: qwen3_rrf|bm25_only|hybrid_cosine|hybrid_rerank|qwen3_full
	CorpusPath             string `yaml:"corpus_path"`              // COMPSHARE_KNOWLEDGE_CORPUS
	EmbeddingsPath         string `yaml:"embeddings_path"`          // COMPSHARE_KNOWLEDGE_EMBEDDINGS
	ExternalCorpusPath     string `yaml:"external_corpus_path"`     // COMPSHARE_EXTERNAL_KNOWLEDGE_CORPUS
	ExternalEmbeddingsPath string `yaml:"external_embeddings_path"` // COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS
	EmbedModel             string `yaml:"embed_model"`              // MODELVERSE_EMBED_MODEL
	RerankerModel          string `yaml:"reranker_model"`           // MODELVERSE_RERANKER_MODEL
	ModelverseBaseURL      string `yaml:"modelverse_base_url"`      // MODELVERSE_BASE_URL
	HybridTimeoutMS        int    `yaml:"hybrid_timeout_ms"`        // RAG_HYBRID_TIMEOUT_MS
	RerankerTimeoutMS      int    `yaml:"reranker_timeout_ms"`      // RAG_RERANKER_TIMEOUT_MS
}

// TraceConfig holds the per-turn JSONL/DB trace sink settings.
type TraceConfig struct {
	Enabled *bool  `yaml:"enabled"` // COMPSHARE_TRACE_ENABLED (Go default off; deploy on)
	Sink    string `yaml:"sink"`    // COMPSHARE_TRACE_SINK: file|mysql|both
	Dir     string `yaml:"dir"`     // COMPSHARE_TRACE_DIR (file/both only)
}

// RuntimeGetenv returns a getenv function that overlays the YAML runtime-flag
// fields (agent.features / agent.retrieval / agent.trace) on top
// of the supplied base getenv (normally os.Getenv). A field SET in YAML wins; a
// field omitted in YAML falls through to base(key). This keeps "YAML is the
// source of truth, env is the fallback" while the cmd/ flag parsers continue to
// read every flag through a single getenv — see cmd/server.go + cmd/cli.go for
// the wiring, and serverTraceGetenv for the same shim shape used for MYSQL_DSN.
//
// Bool fields encode to the canonical on/off string the matching cmd parser
// accepts WITHOUT logging an "unknown value" warning (on = "1"; off = "0" except
// the two parsers — mutating_tools, skill_executor — whose clean "off" is the
// empty string). Strings/ints pass through verbatim and only override when
// non-empty / positive, so an omitted YAML value never masks the env fallback.
func (c *Config) RuntimeGetenv(base func(string) string) func(string) string {
	if c == nil {
		return base
	}
	overrides := map[string]string{}

	f := c.Agent.Features
	// mutating_tools + skill_executor parsers treat "0" as an unknown value
	// (warn); their clean "off" is the empty string. Every other bool parser
	// accepts "0" as off, and the default-ON flags REQUIRE "0" for off ("" =
	// on for those), so they must not use the empty-string off form.
	putBoolEnv(overrides, "COMPSHARE_ENABLE_MUTATING_TOOLS", f.MutatingTools, "1", "")
	putBoolEnv(overrides, "USE_SKILL_EXECUTOR", f.SkillExecutor, "1", "")
	putBoolEnv(overrides, "COMPSHARE_DURABLE_TURNS", f.DurableTurns, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_CONFIRM_FORM", f.ConfirmForm, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_GUIDED_CREATE", f.GuidedCreate, "1", "0")
	putBoolEnv(overrides, "USE_SESSION_FACT_CONTEXT", f.SessionFactContext, "1", "0")
	putBoolEnv(overrides, "USE_REACT_RESULT_PROJECTION", f.ReactResultProjection, "1", "0")
	putBoolEnv(overrides, "USE_REACT_HISTORY_COMPACTION", f.ReactHistoryCompaction, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_UNIFIED_CREATE", f.UnifiedCreate, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_RAG_DOMAIN_MATCH_GUARD", f.DomainMatchGuard, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_EXTERNAL_KNOWLEDGE", f.ExternalKnowledge, "1", "0")
	if len(f.SkillExecutorDiagnosisPilots) > 0 {
		overrides["USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS"] = strings.Join(f.SkillExecutorDiagnosisPilots, ",")
	}

	r := c.Agent.Retrieval
	putStrEnv(overrides, "USE_KNOWLEDGE_RETRIEVAL", r.KnowledgeRetrieval)
	putStrEnv(overrides, "RAG_RETRIEVAL_MODE", r.Mode)
	putStrEnv(overrides, "COMPSHARE_KNOWLEDGE_CORPUS", r.CorpusPath)
	putStrEnv(overrides, "COMPSHARE_KNOWLEDGE_EMBEDDINGS", r.EmbeddingsPath)
	putStrEnv(overrides, "COMPSHARE_EXTERNAL_KNOWLEDGE_CORPUS", r.ExternalCorpusPath)
	putStrEnv(overrides, "COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS", r.ExternalEmbeddingsPath)
	putStrEnv(overrides, "MODELVERSE_EMBED_MODEL", r.EmbedModel)
	putStrEnv(overrides, "MODELVERSE_RERANKER_MODEL", r.RerankerModel)
	putStrEnv(overrides, "MODELVERSE_BASE_URL", r.ModelverseBaseURL)
	putIntEnv(overrides, "RAG_HYBRID_TIMEOUT_MS", r.HybridTimeoutMS)
	putIntEnv(overrides, "RAG_RERANKER_TIMEOUT_MS", r.RerankerTimeoutMS)

	t := c.Agent.Trace
	putBoolEnv(overrides, "COMPSHARE_TRACE_ENABLED", t.Enabled, "1", "0")
	putStrEnv(overrides, "COMPSHARE_TRACE_SINK", t.Sink)
	putStrEnv(overrides, "COMPSHARE_TRACE_DIR", t.Dir)

	// The RAG embedding/reranker clients read the API key through getenv
	// (MODELVERSE_API_KEY, falling back to LLM_API_KEY). Expose the resolved
	// LLM key so a fully-inlined config.yaml (no env) still powers hybrid /
	// qwen3 retrieval. MYSQL_DSN stays handled by serverTraceGetenv.
	putStrEnv(overrides, "LLM_API_KEY", c.Agent.LLM.APIKey)

	if len(overrides) == 0 {
		return base
	}
	return func(key string) string {
		if v, ok := overrides[key]; ok {
			return v
		}
		return base(key)
	}
}

// putBoolEnv records a tri-state *bool as the on/off env-string. A nil pointer
// is left absent so the key falls through to the base getenv. A non-nil pointer
// is always recorded (even when off encodes to ""), so an explicit YAML false
// wins over a set env var.
func putBoolEnv(m map[string]string, key string, v *bool, onStr, offStr string) {
	if v == nil {
		return
	}
	if *v {
		m[key] = onStr
		return
	}
	m[key] = offStr
}

// putStrEnv records a string override only when non-empty; an empty/omitted YAML
// string falls through to the base getenv (it cannot mean "force empty").
func putStrEnv(m map[string]string, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	m[key] = value
}

// putIntEnv records a positive int override only; zero/negative means "omitted —
// fall through to the base getenv and let the parser apply its default".
func putIntEnv(m map[string]string, key string, value int) {
	if value <= 0 {
		return
	}
	m[key] = strconv.Itoa(value)
}
