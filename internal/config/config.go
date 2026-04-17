package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Agent AgentConfig `yaml:"agent"`
}

type AgentConfig struct {
	LLM      LLMConfig `yaml:"llm"`
	Executor string    `yaml:"executor"` // "external" or "internal"

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

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Environment variables override YAML values (secrets should not be in config files)
	envOverride(&cfg.Agent.PublicKey, "COMPSHARE_PUBLIC_KEY")
	envOverride(&cfg.Agent.PrivateKey, "COMPSHARE_PRIVATE_KEY")
	envOverride(&cfg.Agent.ProjectId, "COMPSHARE_PROJECT_ID")
	envOverride(&cfg.Agent.LLM.APIKey, "LLM_API_KEY")

	return &cfg, nil
}

func envOverride(field *string, envKey string) {
	if v := os.Getenv(envKey); v != "" {
		*field = v
	}
}
