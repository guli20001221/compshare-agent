package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	// RateLimitConfig.Limits returns the governance type directly so engine
	// wiring does not duplicate field mapping.
	"github.com/compshare-agent/internal/governance"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Agent AgentConfig `yaml:"agent"`
}

// STSConfig holds settings for UCloud STS (Security Token Service) credential
// generation. All fields are resolved as optional placeholders at Load time;
// the server sub-command validates the required subset before starting.
type STSConfig struct {
	ServiceAK          string        `yaml:"service_ak"`
	ServiceSK          string        `yaml:"service_sk"`
	URL                string        `yaml:"url"`
	RoleUrnTemplate    string        `yaml:"role_urn_template"`
	DefaultRoleUrn     string        `yaml:"default_role_urn"`
	DefaultSessionName string        `yaml:"default_session_name"`
	DurationSeconds    int           `yaml:"duration_seconds"`
	RefreshBefore      time.Duration `yaml:"refresh_before"`
	// IAMURL is the internal UAccount endpoint used to bootstrap per-tenant
	// service-linked roles on demand when AssumeRole returns RoleNotExist
	// (RetCode 11277). Optional: when empty, RoleNotExist errors are returned
	// to the caller unchanged. Reachable only from the UCloud private network.
	IAMURL string `yaml:"iam_url"`
}

type AgentConfig struct {
	LLM       LLMConfig       `yaml:"llm"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Executor  string          `yaml:"executor"` // "external" or "internal"

	CompShareAPIURL string `yaml:"compshare_api_url"`
	PublicKey       string `yaml:"public_key"`
	PrivateKey      string `yaml:"private_key"`
	Region          string `yaml:"region"`
	// ProjectId is the CompShare project ID required by some APIs
	// (e.g. UpdateCompShareStopScheduler). Optional: if empty, the
	// engine will attempt to discover it via GetProjectList at Init.
	ProjectId string `yaml:"project_id"`

	HTTP  HTTPConfig  `yaml:"http"`
	MySQL MySQLConfig `yaml:"mysql"`
	Meta  MetaConfig  `yaml:"meta"`
	STS   STSConfig   `yaml:"sts"`
	OCR   OCRConfig   `yaml:"ocr"`

	// Runtime feature flags, previously env-only (read in cmd/). These are
	// overlaid on os.Getenv via RuntimeGetenv with "YAML wins, env fallback"
	// precedence — see runtime.go. Omitting any section preserves the prior
	// env-driven behavior unchanged.
	Features  FeaturesConfig  `yaml:"features"`
	Retrieval RetrievalConfig `yaml:"retrieval"`
	Trace     TraceConfig     `yaml:"trace"`
}

// HTTPConfig holds settings for the HTTP server mode (compshare-agent server).
// All duration fields accept Go duration strings (e.g. "30s", "1m").
type HTTPConfig struct {
	ListenAddr           string        `yaml:"listen_addr"`
	ReadTimeout          time.Duration `yaml:"read_timeout"`
	WriteTimeout         time.Duration `yaml:"write_timeout"`
	SSEKeepaliveInterval time.Duration `yaml:"sse_keepalive_interval"`
	MaxInputLength       int           `yaml:"max_input_length"`
	PoolCapacity         int           `yaml:"pool_capacity"`
	PoolIdleTTL          time.Duration `yaml:"pool_idle_ttl"`
	// MaxSessionTurns caps the compatibility chat path only. Durable WebSocket
	// turns deliberately have no conversation-length wall: committed history is
	// paged and rebuilt per turn instead of forcing a context-breaking session
	// rollover. Zero or unset uses DefaultMaxSessionTurns in compatibility mode.
	MaxSessionTurns int `yaml:"max_session_turns"`
	// DisableCORS turns off the permissive CORS middleware. Default false =
	// CORS headers are added (needed for local front-end debug per the
	// front-back-local-debug skill: dev server on localhost:3000 fetches
	// 127.0.0.1:<port> directly). In production the agent sits behind a
	// gateway that already issues CORS headers, so set this to true to
	// avoid duplicate Access-Control-Allow-* headers (which browsers
	// reject as malformed even when the values match).
	DisableCORS bool `yaml:"disable_cors"`
}

// DefaultMaxSessionTurns is the compatibility-path fallback when
// agent.http.max_session_turns is zero or unset. The durable turn coordinator
// never consults it.
const DefaultMaxSessionTurns = 10

// MySQLConfig holds connection settings for the MySQL backing store.
// DSN accepts any ${ENV_VAR} placeholder; if the env var is unset the field is
// set to "" so the server sub-command can validate presence before starting.
// A plain literal DSN is passed through unchanged.
// It is optional at Load time so CLI users are not forced to set the env var.
type MySQLConfig struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

// MetaConfig provides the static metadata returned by GetMeta.
type MetaConfig struct {
	Welcome          string   `yaml:"welcome"`
	SuggestedPrompts []string `yaml:"suggested_prompts"`
	MaxInputLength   int      `yaml:"max_input_length"`
}

type LLMConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

// OCRConfig holds settings for the optional screenshot-understanding feature
// used by the HTTP Chat handler. Despite the name it drives a vision-language
// (Qwen3-VL) call, not plain character OCR. When Model is empty, it falls back
// to MODELVERSE_QWEN_VL_MODEL; if still empty, the feature is disabled. BaseURL
// and APIKey default to the LLM values when empty.
type OCRConfig struct {
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	// Prompt overrides the built-in vision prompt the VL model is asked to
	// follow when reading a screenshot. Empty = use the built-in structured
	// interpretation prompt (ocr.DefaultPrompt). Set it to tune what the model
	// extracts/interprets (e.g. emphasize training-error diagnosis) without a
	// code change. An empty/whitespace value is treated as "use the default";
	// it is never sent as an empty instruction.
	Prompt   string        `yaml:"prompt"`
	Timeout  time.Duration `yaml:"timeout"`
	MaxBytes int           `yaml:"max_bytes"`
}

type RateLimitConfig struct {
	LLMQPS             int `yaml:"llm_qps"`
	LLMDaily           int `yaml:"llm_daily"`
	MutatingQPS        int `yaml:"mutating_qps"`
	MutatingDaily      int `yaml:"mutating_daily"`
	ReadExpensiveQPS   int `yaml:"read_expensive_qps"`
	ReadExpensiveDaily int `yaml:"read_expensive_daily"`

	// UserTurnQPS / UserTurnDaily: per-tenant cap on user-initiated chat
	// turns (one ClientMsgUserMessage frame = 1 turn; confirm responses
	// and pings do NOT count). 0 = disabled. Unlike the other classes
	// these are NOT promoted to a built-in default when zero — operator
	// opts in by setting a positive value.
	//
	// Counts in-memory per process. Single-replica + non-persistent: a
	// pod restart resets every tenant's counter, and N replicas without
	// sticky routing yield an effective cap of N × UserTurnDaily.
	UserTurnQPS   int `yaml:"user_turn_qps"`
	UserTurnDaily int `yaml:"user_turn_daily"`

	// MaxTokensPerTurn caps total LLM tokens (prompt + completion summed
	// across every LLM call) used by a single user turn. 0 = disabled.
	// Engine enforces this at ReAct iteration boundaries — never mid
	// tool_call/tool_result pair — so the WS protocol invariant that
	// every tool_call is followed by a tool_result stays intact.
	MaxTokensPerTurn int `yaml:"max_tokens_per_turn"`
}

func (c RateLimitConfig) Limits() governance.Limits {
	return governance.Limits{
		LLMQPS:             c.LLMQPS,
		LLMDaily:           c.LLMDaily,
		MutatingQPS:        c.MutatingQPS,
		MutatingDaily:      c.MutatingDaily,
		ReadExpensiveQPS:   c.ReadExpensiveQPS,
		ReadExpensiveDaily: c.ReadExpensiveDaily,
		UserTurnQPS:        c.UserTurnQPS,
		UserTurnDaily:      c.UserTurnDaily,
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := resolveOptionalPlaceholder(&cfg.Agent.PublicKey, "agent.public_key"); err != nil {
		return nil, err
	}
	if err := resolveOptionalPlaceholder(&cfg.Agent.PrivateKey, "agent.private_key"); err != nil {
		return nil, err
	}
	if err := resolveRequiredSecret(&cfg.Agent.LLM.APIKey, "agent.llm.api_key", "LLM_API_KEY"); err != nil {
		return nil, err
	}
	if err := resolveOptionalPlaceholder(&cfg.Agent.ProjectId, "agent.project_id"); err != nil {
		return nil, err
	}
	if err := applyRateLimitDefaults(&cfg.Agent.RateLimit); err != nil {
		return nil, err
	}
	// mysql.dsn is optional at Load time: CLI path does not require it.
	// The "server" sub-command must validate DSN presence before starting.
	if err := resolveOptionalDSN(&cfg.Agent.MySQL.DSN); err != nil {
		return nil, err
	}
	if err := validateHTTPConfig(&cfg.Agent.HTTP); err != nil {
		return nil, err
	}
	if err := validateMySQLConfig(&cfg.Agent.MySQL); err != nil {
		return nil, err
	}
	if err := validateMetaConfig(&cfg.Agent.Meta); err != nil {
		return nil, err
	}
	// STS: resolve optional placeholders for service credentials.
	if err := resolveOptionalCredential(&cfg.Agent.STS.ServiceAK, "agent.sts.service_ak"); err != nil {
		return nil, err
	}
	if err := resolveOptionalCredential(&cfg.Agent.STS.ServiceSK, "agent.sts.service_sk"); err != nil {
		return nil, err
	}
	if err := resolveOptionalCredential(&cfg.Agent.STS.DefaultRoleUrn, "agent.sts.default_role_urn"); err != nil {
		return nil, err
	}
	if err := resolveOptionalCredential(&cfg.Agent.STS.IAMURL, "agent.sts.iam_url"); err != nil {
		return nil, err
	}
	if err := validateSTSConfig(&cfg.Agent.STS); err != nil {
		return nil, err
	}
	applySTSDefaults(&cfg.Agent.STS)
	applyHTTPDefaults(&cfg.Agent.HTTP)
	applyMySQLDefaults(&cfg.Agent.MySQL)

	if err := resolveOptionalCredential(&cfg.Agent.OCR.APIKey, "agent.ocr.api_key"); err != nil {
		return nil, err
	}
	if err := validateOCRConfig(&cfg.Agent.OCR); err != nil {
		return nil, err
	}
	applyOCRDefaults(&cfg.Agent.OCR, &cfg.Agent.LLM)

	// Check for explicit mismatch before meta defaults are applied.
	// meta.max_input_length == 0 means "not set"; inheritance happens below.
	if cfg.Agent.Meta.MaxInputLength != 0 && cfg.Agent.HTTP.MaxInputLength != 0 &&
		cfg.Agent.Meta.MaxInputLength != cfg.Agent.HTTP.MaxInputLength {
		return nil, fmt.Errorf(
			"agent.http.max_input_length (%d) and agent.meta.max_input_length (%d) conflict: set only one or make them equal",
			cfg.Agent.HTTP.MaxInputLength, cfg.Agent.Meta.MaxInputLength,
		)
	}

	applyMetaDefaults(&cfg.Agent.Meta, cfg.Agent.HTTP.MaxInputLength)

	return &cfg, nil
}

func applyRateLimitDefaults(rateLimit *RateLimitConfig) error {
	defaults := governance.DefaultLimits()
	if rateLimit.LLMQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.llm_qps")
	}
	if rateLimit.LLMDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.llm_daily")
	}
	if rateLimit.MutatingQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.mutating_qps")
	}
	if rateLimit.MutatingDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.mutating_daily")
	}
	if rateLimit.ReadExpensiveQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.read_expensive_qps")
	}
	if rateLimit.ReadExpensiveDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.read_expensive_daily")
	}
	if rateLimit.UserTurnQPS < 0 {
		return negativeRateLimitError("agent.rate_limit.user_turn_qps")
	}
	if rateLimit.UserTurnDaily < 0 {
		return negativeRateLimitError("agent.rate_limit.user_turn_daily")
	}
	if rateLimit.MaxTokensPerTurn < 0 {
		return negativeRateLimitError("agent.rate_limit.max_tokens_per_turn")
	}
	if rateLimit.LLMQPS == 0 {
		rateLimit.LLMQPS = defaults.LLMQPS
	}
	if rateLimit.LLMDaily == 0 {
		rateLimit.LLMDaily = defaults.LLMDaily
	}
	if rateLimit.MutatingQPS == 0 {
		rateLimit.MutatingQPS = defaults.MutatingQPS
	}
	if rateLimit.MutatingDaily == 0 {
		rateLimit.MutatingDaily = defaults.MutatingDaily
	}
	if rateLimit.ReadExpensiveQPS == 0 {
		rateLimit.ReadExpensiveQPS = defaults.ReadExpensiveQPS
	}
	if rateLimit.ReadExpensiveDaily == 0 {
		rateLimit.ReadExpensiveDaily = defaults.ReadExpensiveDaily
	}
	return nil
}

func negativeRateLimitError(yamlPath string) error {
	return fmt.Errorf("%s must be non-negative (0 or omit to use default)", yamlPath)
}

func negativeValueError(yamlPath string) error {
	return fmt.Errorf("%s must be non-negative (0 or omit to use default)", yamlPath)
}

// validateHTTPConfig rejects any explicitly-set negative numeric values.
// Zero values are allowed (meaning "use default" or intentional zero for SSE write timeout).
func validateHTTPConfig(h *HTTPConfig) error {
	if h.PoolCapacity < 0 {
		return negativeValueError("agent.http.pool_capacity")
	}
	if h.MaxInputLength < 0 {
		return negativeValueError("agent.http.max_input_length")
	}
	if h.PoolIdleTTL < 0 {
		return negativeValueError("agent.http.pool_idle_ttl")
	}
	if h.ReadTimeout < 0 {
		return negativeValueError("agent.http.read_timeout")
	}
	if h.WriteTimeout < 0 {
		return negativeValueError("agent.http.write_timeout")
	}
	if h.SSEKeepaliveInterval < 0 {
		return negativeValueError("agent.http.sse_keepalive_interval")
	}
	if h.MaxSessionTurns < 0 {
		return negativeValueError("agent.http.max_session_turns")
	}
	return nil
}

// validateMySQLConfig rejects any explicitly-set negative numeric values.
func validateMySQLConfig(m *MySQLConfig) error {
	if m.MaxOpenConns < 0 {
		return negativeValueError("agent.mysql.max_open_conns")
	}
	if m.MaxIdleConns < 0 {
		return negativeValueError("agent.mysql.max_idle_conns")
	}
	if m.ConnMaxLifetime < 0 {
		return negativeValueError("agent.mysql.conn_max_lifetime")
	}
	return nil
}

// validateMetaConfig rejects any explicitly-set negative numeric values.
func validateMetaConfig(meta *MetaConfig) error {
	if meta.MaxInputLength < 0 {
		return negativeValueError("agent.meta.max_input_length")
	}
	return nil
}

// resolveRequiredSecret resolves a required secret field. It accepts EITHER a
// ${ENV_VAR} placeholder (resolved from the environment; the named var must be
// non-empty) OR an inline literal value. Inline literals are permitted so a
// production config.yaml can be fully self-contained with no environment
// variables (the YAML-first config migration). envKey names the conventional
// env var for the error message only; any ${...} placeholder is honored for
// compatibility.
func resolveRequiredSecret(field *string, yamlPath, envKey string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return fmt.Errorf("%s is required: use the ${%s} placeholder or an inline value", yamlPath, envKey)
	}
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		key := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if key == "" {
			return fmt.Errorf("%s placeholder must name an environment variable", yamlPath)
		}
		value := os.Getenv(key)
		if value == "" {
			return fmt.Errorf("environment variable %s is required for %s", key, yamlPath)
		}
		*field = value
		return nil
	}
	if strings.HasPrefix(raw, "$") {
		return fmt.Errorf("%s must use ${ENV_VAR} placeholder syntax or an inline value", yamlPath)
	}
	// Inline literal — pass through unchanged.
	*field = raw
	return nil
}

// resolveOptionalPlaceholder resolves an optional credential-ish field
// (public_key / private_key / project_id). Empty is allowed (left unchanged). A
// ${ENV_VAR} placeholder is resolved from the environment (the named var must be
// non-empty, matching the prior contract). An inline literal is now permitted
// too (YAML-first migration), so a self-contained config.yaml needs no env. A
// "$"-prefixed value that is not valid ${...} is rejected as a typo.
func resolveOptionalPlaceholder(field *string, yamlPath string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		envKey := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if envKey == "" {
			return fmt.Errorf("%s placeholder must name an environment variable", yamlPath)
		}
		value := os.Getenv(envKey)
		if value == "" {
			return fmt.Errorf("environment variable %s is required for %s", envKey, yamlPath)
		}
		*field = value
		return nil
	}
	if strings.HasPrefix(raw, "$") {
		return fmt.Errorf("%s must use ${ENV_VAR} placeholder syntax or be a plain literal", yamlPath)
	}
	// Inline literal — pass through unchanged.
	*field = raw
	return nil
}

// resolveOptionalDSN resolves any ${ENV_VAR} placeholder in the DSN field.
// If the field is empty or a plain literal it is left unchanged.
// If the field looks like a placeholder but the env var is unset, the field is
// set to "" so server-mode validators can simply check dsn == "".
// Returns an error only when the value starts with "$" but is not valid ${...}
// syntax, to catch typos like "$MYSQL_DSN".
func resolveOptionalDSN(field *string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	// Detect placeholder-like values that start with "$".
	if strings.HasPrefix(raw, "$") {
		if !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
			return fmt.Errorf("agent.mysql.dsn must use ${ENV_VAR} placeholder syntax or be a plain literal DSN")
		}
		envKey := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if envKey == "" {
			return fmt.Errorf("agent.mysql.dsn placeholder must name an environment variable")
		}
		// Env var unset → blank the field; server sub-command will reject it.
		*field = os.Getenv(envKey)
		return nil
	}
	// Plain literal DSN — pass through unchanged.
	return nil
}

// applyHTTPDefaults fills zero-value fields with documented defaults.
func applyHTTPDefaults(h *HTTPConfig) {
	if h.ListenAddr == "" {
		h.ListenAddr = "0.0.0.0:7429"
	}
	if h.ReadTimeout == 0 {
		h.ReadTimeout = 30 * time.Second
	}
	// WriteTimeout == 0 is intentional for SSE; keep it.
	if h.SSEKeepaliveInterval == 0 {
		h.SSEKeepaliveInterval = 15 * time.Second
	}
	if h.MaxInputLength == 0 {
		h.MaxInputLength = 4000
	}
	if h.PoolCapacity == 0 {
		h.PoolCapacity = 200
	}
	if h.PoolIdleTTL == 0 {
		h.PoolIdleTTL = 30 * time.Minute
	}
}

// applyMySQLDefaults fills zero-value connection pool fields with documented defaults.
// DSN is not defaulted here; it is optional at Load time.
func applyMySQLDefaults(m *MySQLConfig) {
	if m.MaxOpenConns == 0 {
		m.MaxOpenConns = 20
	}
	if m.MaxIdleConns == 0 {
		m.MaxIdleConns = 5
	}
	if m.ConnMaxLifetime == 0 {
		m.ConnMaxLifetime = time.Hour
	}
}

// applyMetaDefaults fills the meta section. MaxInputLength inherits from the
// http section when omitted so both are always consistent.
func applyMetaDefaults(meta *MetaConfig, httpMaxInputLength int) {
	if meta.MaxInputLength == 0 {
		meta.MaxInputLength = httpMaxInputLength
	}
}

// resolveOptionalCredential resolves any ${ENV_VAR} placeholder in an
// optional credential field (e.g. STS service_ak, service_sk).
// If the field is empty it is left unchanged.
// If the field looks like a ${...} placeholder but the env var is unset, the
// field is cleared to "" so callers (server/cli sub-commands) can validate
// presence at startup time.
// Returns an error only when the value starts with "$" but uses invalid
// syntax (e.g. "$COMPSHARE_SERVICE_PUBLIC_KEY" without braces).
func resolveOptionalCredential(field *string, yamlPath string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "$") {
		if !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
			return fmt.Errorf("%s must use ${ENV_VAR} placeholder syntax or be a plain literal", yamlPath)
		}
		envKey := strings.TrimSuffix(strings.TrimPrefix(raw, "${"), "}")
		if envKey == "" {
			return fmt.Errorf("%s placeholder must name an environment variable", yamlPath)
		}
		// Env var unset → blank the field; sub-command validates before use.
		*field = os.Getenv(envKey)
		return nil
	}
	// Plain literal — pass through unchanged.
	return nil
}

// validateSTSConfig rejects explicitly negative numeric or duration values.
func validateSTSConfig(s *STSConfig) error {
	if s.DurationSeconds < 0 {
		return negativeValueError("agent.sts.duration_seconds")
	}
	if s.RefreshBefore < 0 {
		return negativeValueError("agent.sts.refresh_before")
	}
	return nil
}

// applySTSDefaults fills zero-value STS fields with documented defaults.
func applySTSDefaults(s *STSConfig) {
	if s.DurationSeconds == 0 {
		s.DurationSeconds = 3600
	}
	if s.RefreshBefore == 0 {
		s.RefreshBefore = 5 * time.Minute
	}
	if s.DefaultSessionName == "" {
		s.DefaultSessionName = "agent-default"
	}
	if s.URL == "" {
		s.URL = "https://api.ucloud.cn/"
	}
}

func validateOCRConfig(ocr *OCRConfig) error {
	if ocr.Timeout < 0 {
		return negativeValueError("agent.ocr.timeout")
	}
	if ocr.MaxBytes < 0 {
		return negativeValueError("agent.ocr.max_bytes")
	}
	return nil
}

func applyOCRDefaults(ocr *OCRConfig, llmCfg *LLMConfig) {
	if ocr.Model == "" {
		ocr.Model = strings.TrimSpace(os.Getenv("MODELVERSE_QWEN_VL_MODEL"))
	}
	if ocr.Timeout == 0 {
		ocr.Timeout = 15 * time.Second
	}
	if ocr.MaxBytes == 0 {
		ocr.MaxBytes = 5 * 1024 * 1024
	}
	if ocr.BaseURL == "" && llmCfg != nil {
		ocr.BaseURL = llmCfg.BaseURL
	}
	if ocr.APIKey == "" && llmCfg != nil {
		ocr.APIKey = llmCfg.APIKey
	}
}
