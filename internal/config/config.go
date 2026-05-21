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
	// MaxSessionTurns caps how many user-assistant question-answer pairs a
	// single session may produce. Zero or unset falls back to
	// DefaultMaxSessionTurns. Enforced in handleChat: once a session has
	// reached the cap, further Chat requests return SessionTurnLimitExceeded
	// and the caller must open a new session.
	MaxSessionTurns int `yaml:"max_session_turns"`
}

// DefaultMaxSessionTurns is the fallback cap when agent.http.max_session_turns
// is zero or unset. 10 question-answer pairs per session = 20 stored messages.
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

type RateLimitConfig struct {
	LLMQPS             int `yaml:"llm_qps"`
	LLMDaily           int `yaml:"llm_daily"`
	MutatingQPS        int `yaml:"mutating_qps"`
	MutatingDaily      int `yaml:"mutating_daily"`
	ReadExpensiveQPS   int `yaml:"read_expensive_qps"`
	ReadExpensiveDaily int `yaml:"read_expensive_daily"`
}

func (c RateLimitConfig) Limits() governance.Limits {
	return governance.Limits{
		LLMQPS:             c.LLMQPS,
		LLMDaily:           c.LLMDaily,
		MutatingQPS:        c.MutatingQPS,
		MutatingDaily:      c.MutatingDaily,
		ReadExpensiveQPS:   c.ReadExpensiveQPS,
		ReadExpensiveDaily: c.ReadExpensiveDaily,
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
	if err := validateSTSConfig(&cfg.Agent.STS); err != nil {
		return nil, err
	}
	applySTSDefaults(&cfg.Agent.STS)
	applyHTTPDefaults(&cfg.Agent.HTTP)
	applyMySQLDefaults(&cfg.Agent.MySQL)

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

func resolveRequiredSecret(field *string, yamlPath, envKey string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return fmt.Errorf("%s must use ${%s} placeholder; literal or empty secrets are not allowed", yamlPath, envKey)
	}
	if raw != placeholder(envKey) {
		return fmt.Errorf("%s must use ${%s} placeholder; literal secrets are not allowed", yamlPath, envKey)
	}
	value := os.Getenv(envKey)
	if value == "" {
		return fmt.Errorf("environment variable %s is required for %s", envKey, yamlPath)
	}
	*field = value
	return nil
}

func resolveOptionalPlaceholder(field *string, yamlPath string) error {
	raw := strings.TrimSpace(*field)
	if raw == "" {
		return nil
	}
	if !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
		return fmt.Errorf("%s must use ${ENV_VAR} placeholder or be empty", yamlPath)
	}
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

func placeholder(envKey string) string {
	return "${" + envKey + "}"
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
		h.ListenAddr = "0.0.0.0:8080"
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
