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
//     knowledge verification default ON).
//   - &true / &false → explicit value; it WINS over any env var.
type FeaturesConfig struct {
	MutatingTools         *bool `yaml:"mutating_tools"`          // COMPSHARE_ENABLE_MUTATING_TOOLS (default off)
	DurableTurns          *bool `yaml:"durable_turns"`           // COMPSHARE_DURABLE_TURNS (server-only, default off)
	ConfirmForm           *bool `yaml:"confirm_form"`            // COMPSHARE_CONFIRM_FORM (server-only, default off)
	GuidedCreate          *bool `yaml:"guided_create"`           // COMPSHARE_GUIDED_CREATE (server-only, default off)
	CanonicalTranscript   *bool `yaml:"canonical_transcript"`    // COMPSHARE_CANONICAL_TRANSCRIPT (Go default off; deploy config enables it)
	ReactResultProjection *bool `yaml:"react_result_projection"` // USE_REACT_RESULT_PROJECTION (Go default off; deploy on)
}

// RetrievalConfig holds the remote knowledge retrieval knobs.
// Empty string / zero int means "omitted — fall through to env, then default".
type RetrievalConfig struct {
	KnowledgeRetrieval string `yaml:"knowledge_retrieval"` // USE_KNOWLEDGE_RETRIEVAL: curated|off
	MCPURL             string `yaml:"mcp_url"`             // COMPSHARE_KB_MCP_URL (complete remote compshare-kb /mcp URL)
	MCPBearerToken     string `yaml:"mcp_bearer_token"`    // COMPSHARE_KB_MCP_BEARER_TOKEN (read-only token; optional in trusted cluster)
	MCPTimeoutMS       int    `yaml:"mcp_timeout_ms"`      // COMPSHARE_KB_MCP_TIMEOUT_MS
}

// WebSearchConfig configures an optional, read-only external-search MCP
// fallback. It is deliberately separate from RetrievalConfig: the curated KB is
// an on-by-default product dependency, while web search defaults off and is only
// exposed after that KB returned no substantive evidence.
type WebSearchConfig struct {
	Enabled        *bool  `yaml:"enabled"`          // COMPSHARE_WEB_SEARCH_ENABLED (default off)
	MCPURL         string `yaml:"mcp_url"`          // COMPSHARE_WEB_SEARCH_MCP_URL
	MCPBearerToken string `yaml:"mcp_bearer_token"` // COMPSHARE_WEB_SEARCH_MCP_BEARER_TOKEN
	MCPTimeoutMS   int    `yaml:"mcp_timeout_ms"`   // COMPSHARE_WEB_SEARCH_MCP_TIMEOUT_MS
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
// accepts WITHOUT logging an "unknown value" warning. Strings/ints pass through verbatim and only override when
// non-empty / positive, so an omitted YAML value never masks the env fallback.
func (c *Config) RuntimeGetenv(base func(string) string) func(string) string {
	if c == nil {
		return base
	}
	overrides := map[string]string{}

	f := c.Agent.Features
	// mutating_tools treats "0" as an unknown value (warn); its clean "off" is
	// the empty string. Every other bool parser
	// accepts "0" as off, and the default-ON flags REQUIRE "0" for off ("" =
	// on for those), so they must not use the empty-string off form.
	putBoolEnv(overrides, "COMPSHARE_ENABLE_MUTATING_TOOLS", f.MutatingTools, "1", "")
	putBoolEnv(overrides, "COMPSHARE_DURABLE_TURNS", f.DurableTurns, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_CONFIRM_FORM", f.ConfirmForm, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_GUIDED_CREATE", f.GuidedCreate, "1", "0")
	putBoolEnv(overrides, "USE_REACT_RESULT_PROJECTION", f.ReactResultProjection, "1", "0")
	putBoolEnv(overrides, "COMPSHARE_CANONICAL_TRANSCRIPT", f.CanonicalTranscript, "1", "0")

	r := c.Agent.Retrieval
	putStrEnv(overrides, "USE_KNOWLEDGE_RETRIEVAL", r.KnowledgeRetrieval)
	putStrEnv(overrides, "COMPSHARE_KB_MCP_URL", r.MCPURL)
	putStrEnv(overrides, "COMPSHARE_KB_MCP_BEARER_TOKEN", r.MCPBearerToken)
	putIntEnv(overrides, "COMPSHARE_KB_MCP_TIMEOUT_MS", r.MCPTimeoutMS)

	w := c.Agent.WebSearch
	putBoolEnv(overrides, "COMPSHARE_WEB_SEARCH_ENABLED", w.Enabled, "1", "0")
	putStrEnv(overrides, "COMPSHARE_WEB_SEARCH_MCP_URL", w.MCPURL)
	putStrEnv(overrides, "COMPSHARE_WEB_SEARCH_MCP_BEARER_TOKEN", w.MCPBearerToken)
	putIntEnv(overrides, "COMPSHARE_WEB_SEARCH_MCP_TIMEOUT_MS", w.MCPTimeoutMS)

	t := c.Agent.Trace
	putBoolEnv(overrides, "COMPSHARE_TRACE_ENABLED", t.Enabled, "1", "0")
	putStrEnv(overrides, "COMPSHARE_TRACE_SINK", t.Sink)
	putStrEnv(overrides, "COMPSHARE_TRACE_DIR", t.Dir)

	// ssh_ops.enabled is tri-state like the feature bools; a YAML "false" WINS over
	// COMPSHARE_SSH_OPS=1 in the env (P2 gate 3). The non-bool harness settings are read
	// straight off cfg.Agent.SSHOps by the cmd wiring, not through getenv.
	putBoolEnv(overrides, "COMPSHARE_SSH_OPS", c.Agent.SSHOps.Enabled, "1", "0")

	// Expose the resolved answer-model key to legacy getenv-based callers.
	// MYSQL_DSN stays handled by serverTraceGetenv.
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
