package config

import (
	"strconv"
	"strings"
)

// AuthorizationConfig controls whether account-changing product tools may be
// exposed. It does not select an alternate Agent implementation.
type AuthorizationConfig struct {
	MutatingTools *bool `yaml:"mutating_tools"` // COMPSHARE_ENABLE_MUTATING_TOOLS (default off)
}

// RetrievalConfig holds the remote knowledge retrieval knobs.
// Empty string / zero int means "omitted — fall through to env, then default".
type RetrievalConfig struct {
	MCPURL         string `yaml:"mcp_url"`          // COMPSHARE_KB_MCP_URL (complete remote compshare-kb /mcp URL)
	MCPBearerToken string `yaml:"mcp_bearer_token"` // COMPSHARE_KB_MCP_BEARER_TOKEN (read-only token; optional in trusted cluster)
	MCPTimeoutMS   int    `yaml:"mcp_timeout_ms"`   // COMPSHARE_KB_MCP_TIMEOUT_MS
}

// TraceConfig holds the per-turn JSONL/DB trace sink settings.
type TraceConfig struct {
	Sink string `yaml:"sink"` // COMPSHARE_TRACE_SINK: file|mysql; empty disables tracing
	Dir  string `yaml:"dir"`  // COMPSHARE_TRACE_DIR (file only)
}

// RuntimeGetenv overlays typed YAML operational settings on environment
// fallbacks. An explicit YAML value wins; an omitted value falls through.
func (c *Config) RuntimeGetenv(base func(string) string) func(string) string {
	if c == nil {
		return base
	}
	overrides := map[string]string{}

	a := c.Agent.Authorization
	// The mutating-tools parser uses an empty string for the disabled state.
	putBoolEnv(overrides, "COMPSHARE_ENABLE_MUTATING_TOOLS", a.MutatingTools, "1", "")

	r := c.Agent.Retrieval
	putStrEnv(overrides, "COMPSHARE_KB_MCP_URL", r.MCPURL)
	putStrEnv(overrides, "COMPSHARE_KB_MCP_BEARER_TOKEN", r.MCPBearerToken)
	putIntEnv(overrides, "COMPSHARE_KB_MCP_TIMEOUT_MS", r.MCPTimeoutMS)

	t := c.Agent.Trace
	putStrEnv(overrides, "COMPSHARE_TRACE_SINK", t.Sink)
	putStrEnv(overrides, "COMPSHARE_TRACE_DIR", t.Dir)

	// Expose the resolved answer-model key to components configured via getenv.
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
