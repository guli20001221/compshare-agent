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
//     agentic_search_knowledge / knowledge_qa_* / external_knowledge default ON).
//   - &true / &false → explicit value; it WINS over any env var.
//
// SkillExecutorDiagnosisPilots is a list (joined to the CSV the env parser
// expects) and only overrides when non-empty.
type FeaturesConfig struct {
	MutatingTools                   *bool    `yaml:"mutating_tools"`                     // COMPSHARE_ENABLE_MUTATING_TOOLS (default off)
	ConfirmForm                     *bool    `yaml:"confirm_form"`                       // COMPSHARE_CONFIRM_FORM (server-only, default off)
	GuidedCreate                    *bool    `yaml:"guided_create"`                      // COMPSHARE_GUIDED_CREATE (server-only, default off)
	AgenticSearchKnowledge          *bool    `yaml:"agentic_search_knowledge"`           // COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE (default ON)
	KnowledgeQAAgentLoop            *bool    `yaml:"knowledge_qa_agent_loop"`            // COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP (default ON)
	KnowledgeQADisciplinedSynthesis *bool    `yaml:"knowledge_qa_disciplined_synthesis"` // COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS (default ON)
	KnowledgeQASelfRevision         *bool    `yaml:"knowledge_qa_self_revision"`         // COMPSHARE_KQA_SELF_REVISION (default ON)
	ExternalKnowledge               *bool    `yaml:"external_knowledge"`                 // COMPSHARE_EXTERNAL_KNOWLEDGE (default ON)
	GroundedValidator               *bool    `yaml:"grounded_validator"`                 // COMPSHARE_RAG_GROUNDED_VALIDATOR (default off)
	DomainMatchGuard                *bool    `yaml:"domain_match_guard"`                 // COMPSHARE_RAG_DOMAIN_MATCH_GUARD (default off)
	FlashKnowledgeRouteGuard        *bool    `yaml:"flash_knowledge_route_guard"`        // COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD (default off)
	SessionFactContext              *bool    `yaml:"session_fact_context"`               // USE_SESSION_FACT_CONTEXT (Go default off; deploy on)
	ReactResultProjection           *bool    `yaml:"react_result_projection"`            // USE_REACT_RESULT_PROJECTION (Go default off; deploy on)
	ReactHistoryCompaction          *bool    `yaml:"react_history_compaction"`           // USE_REACT_HISTORY_COMPACTION (Go default off; deploy on)
	IntentScopedReactPrompt         *bool    `yaml:"intent_scoped_react_prompt"`         // USE_INTENT_SCOPED_REACT_PROMPT (default off)
	CreatePreferenceExtractor       *bool    `yaml:"create_preference_extractor"`        // COMPSHARE_CREATE_PREF_EXTRACTOR (default on; false disables)
	UnifiedCreate                   *bool    `yaml:"unified_create"`                     // COMPSHARE_UNIFIED_CREATE (default on; false disables)
	ContextContinuation             *bool    `yaml:"context_continuation"`               // COMPSHARE_CONTEXT_CONTINUATION (default on; false disables)
	SkillExecutor                   *bool    `yaml:"skill_executor"`                     // USE_SKILL_EXECUTOR (default off)
	SkillExecutorDiagnosisPilots    []string `yaml:"skill_executor_diagnosis_pilots"`    // USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS (CSV)
}

// RetrievalConfig holds the RAG / knowledge retrieval + grounded-renderer knobs.
// Empty string / zero int means "omitted — fall through to env, then default".
type RetrievalConfig struct {
	KnowledgeRetrieval     string `yaml:"knowledge_retrieval"`      // USE_KNOWLEDGE_RETRIEVAL: curated|off
	Mode                   string `yaml:"mode"`                     // RAG_RETRIEVAL_MODE: qwen3_rrf|bm25_only|hybrid_cosine|hybrid_rerank|qwen3_full
	GroundedRenderer       string `yaml:"grounded_renderer"`        // USE_GROUNDED_RENDERER: llm|fast_template|off
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

// SSHOpsConfig holds the consent-gated, read-only in-instance SSH diagnosis lane
// (internal/sshops). Enabled is tri-state like TraceConfig.Enabled: nil means the
// section was omitted and the COMPSHARE_SSH_OPS env var decides, which — with the
// env var unset — leaves the lane OFF. It stays default-off deliberately: turning
// it on additionally requires the audit table (deploy/migrations/0005, the writer
// is fail-closed), the ccr gateway reachable at GatewayURL, and python + the
// Claude Agent SDK on the server host.
//
// The string knobs override the built-in defaults in cmd/server.go; an omitted
// (empty) value falls through to the env var and then to that default.
type SSHOpsConfig struct {
	Enabled     *bool  `yaml:"enabled"`      // COMPSHARE_SSH_OPS (default off)
	Python      string `yaml:"python"`       // COMPSHARE_SSH_OPS_PYTHON (default "python3")
	HarnessPath string `yaml:"harness_path"` // COMPSHARE_SSH_OPS_HARNESS (default "deploy/ssh_ops_harness/harness.py")
	GatewayURL  string `yaml:"gateway_url"`  // COMPSHARE_SSH_OPS_GATEWAY (default "http://127.0.0.1:3456")
	Model       string `yaml:"model"`        // COMPSHARE_SSH_OPS_MODEL (default "deepseek-v4-flash")
}

// PlannerConfig holds the intent-router / direct-dispatch knobs.
type PlannerConfig struct {
	RouterMode            string `yaml:"router_mode"`             // COMPSHARE_INTENT_ROUTER_MODE: shadow (else off)
	DirectDispatchIntents string `yaml:"direct_dispatch_intents"` // COMPSHARE_DIRECT_DISPATCH_INTENTS: CSV or "off"
	StructuredOutput      string `yaml:"structured_output"`       // COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT: json_object|json_schema|off
}

// RuntimeGetenv returns a getenv function that overlays the YAML runtime-flag
// fields (agent.features / agent.retrieval / agent.trace / agent.planner /
// agent.ssh_ops) on top of the supplied base getenv (normally os.Getenv). A field
// SET in YAML wins; a field omitted in YAML falls through to base(key). This keeps
// "YAML is the source of truth, env is the fallback" while the cmd/ flag parsers
// continue to read every flag through a single getenv — see cmd/server.go +
// cmd/cli.go for the wiring, and serverTraceGetenv for the same shim shape used
// for MYSQL_DSN.
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
	putBoolEnv(overrides, "COMPSHARE_CONFIRM_FORM", f.ConfirmForm, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_GUIDED_CREATE", f.GuidedCreate, "1", "0")
	putBoolEnv(overrides, "USE_SESSION_FACT_CONTEXT", f.SessionFactContext, "1", "0")
	putBoolEnv(overrides, "USE_REACT_RESULT_PROJECTION", f.ReactResultProjection, "1", "0")
	putBoolEnv(overrides, "USE_REACT_HISTORY_COMPACTION", f.ReactHistoryCompaction, "1", "0")
	putBoolEnv(overrides, "USE_INTENT_SCOPED_REACT_PROMPT", f.IntentScopedReactPrompt, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_CREATE_PREF_EXTRACTOR", f.CreatePreferenceExtractor, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_UNIFIED_CREATE", f.UnifiedCreate, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_CONTEXT_CONTINUATION", f.ContextContinuation, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE", f.AgenticSearchKnowledge, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_RAG_GROUNDED_VALIDATOR", f.GroundedValidator, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_RAG_DOMAIN_MATCH_GUARD", f.DomainMatchGuard, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_FLASH_KNOWLEDGE_ROUTE_GUARD", f.FlashKnowledgeRouteGuard, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP", f.KnowledgeQAAgentLoop, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS", f.KnowledgeQADisciplinedSynthesis, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_KQA_SELF_REVISION", f.KnowledgeQASelfRevision, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_EXTERNAL_KNOWLEDGE", f.ExternalKnowledge, "1", "0")
	if len(f.SkillExecutorDiagnosisPilots) > 0 {
		overrides["USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS"] = strings.Join(f.SkillExecutorDiagnosisPilots, ",")
	}

	r := c.Agent.Retrieval
	putStrEnv(overrides, "USE_KNOWLEDGE_RETRIEVAL", r.KnowledgeRetrieval)
	putStrEnv(overrides, "RAG_RETRIEVAL_MODE", r.Mode)
	putStrEnv(overrides, "USE_GROUNDED_RENDERER", r.GroundedRenderer)
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

	p := c.Agent.Planner
	putStrEnv(overrides, "COMPSHARE_INTENT_ROUTER_MODE", p.RouterMode)
	putStrEnv(overrides, "COMPSHARE_DIRECT_DISPATCH_INTENTS", p.DirectDispatchIntents)
	putStrEnv(overrides, "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT", p.StructuredOutput)

	// The COMPSHARE_SSH_OPS parser (cmd/server.go) accepts "0" as a clean off, so
	// an explicit YAML false encodes to "0" and wins over a set env var — the
	// same shape as confirm_form, not the empty-string form mutating_tools needs.
	s := c.Agent.SSHOps
	putBoolEnv(overrides, "COMPSHARE_SSH_OPS", s.Enabled, "1", "0")
	putStrEnv(overrides, "COMPSHARE_SSH_OPS_PYTHON", s.Python)
	putStrEnv(overrides, "COMPSHARE_SSH_OPS_HARNESS", s.HarnessPath)
	putStrEnv(overrides, "COMPSHARE_SSH_OPS_GATEWAY", s.GatewayURL)
	putStrEnv(overrides, "COMPSHARE_SSH_OPS_MODEL", s.Model)

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
