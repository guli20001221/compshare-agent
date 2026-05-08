package config

import (
	"fmt"
	"os"
	"strings"

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
}

type LLMConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
}

type RateLimitConfig struct {
	LLMQPS        int `yaml:"llm_qps"`
	LLMDaily      int `yaml:"llm_daily"`
	MutatingQPS   int `yaml:"mutating_qps"`
	MutatingDaily int `yaml:"mutating_daily"`
}

func (c RateLimitConfig) Limits() governance.Limits {
	return governance.Limits{
		LLMQPS:        c.LLMQPS,
		LLMDaily:      c.LLMDaily,
		MutatingQPS:   c.MutatingQPS,
		MutatingDaily: c.MutatingDaily,
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

	return &cfg, nil
}

func applyRateLimitDefaults(rateLimit *RateLimitConfig) error {
	defaults := governance.DefaultLimits()
	if rateLimit.LLMQPS < 0 {
		return fmt.Errorf("agent.rate_limit.llm_qps must be non-negative")
	}
	if rateLimit.LLMDaily < 0 {
		return fmt.Errorf("agent.rate_limit.llm_daily must be non-negative")
	}
	if rateLimit.MutatingQPS < 0 {
		return fmt.Errorf("agent.rate_limit.mutating_qps must be non-negative")
	}
	if rateLimit.MutatingDaily < 0 {
		return fmt.Errorf("agent.rate_limit.mutating_daily must be non-negative")
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
