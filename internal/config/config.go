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
}

// MySQLConfig holds connection settings for the MySQL backing store.
// DSN is resolved from ${MYSQL_DSN} placeholder if the yaml value uses that form;
// it is optional at Load time so CLI users are not forced to set the env var.
type MySQLConfig struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

// MetaConfig provides the static metadata returned by GetMeta.
type MetaConfig struct {
	Welcome          string        `yaml:"welcome"`
	SuggestedPrompts []string      `yaml:"suggested_prompts"`
	MaxInputLength   int           `yaml:"max_input_length"`
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

	if err := resolveRequiredSecret(&cfg.Agent.PublicKey, "agent.public_key", "COMPSHARE_PUBLIC_KEY"); err != nil {
		return nil, err
	}
	if err := resolveRequiredSecret(&cfg.Agent.PrivateKey, "agent.private_key", "COMPSHARE_PRIVATE_KEY"); err != nil {
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
	resolveOptionalDSN(&cfg.Agent.MySQL.DSN)
	applyHTTPDefaults(&cfg.Agent.HTTP)
	applyMySQLDefaults(&cfg.Agent.MySQL)
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

// resolveOptionalDSN resolves ${MYSQL_DSN} placeholder if present.
// If the field is empty or uses some other form it is left as-is;
// it is the server sub-command's responsibility to validate it.
func resolveOptionalDSN(field *string) {
	raw := strings.TrimSpace(*field)
	if raw == placeholder("MYSQL_DSN") {
		if v := os.Getenv("MYSQL_DSN"); v != "" {
			*field = v
		}
	}
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
