package engine

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
)

func validLLMCfg(model string) *config.Config {
	return &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{
			BaseURL: "https://example.test/v1",
			APIKey:  "test-key",
			Model:   model,
		},
	}}
}

func TestNewSharedDeps_EmptyBaseModelReturnsError(t *testing.T) {
	deps, err := NewSharedDeps(validLLMCfg(""))
	if err == nil {
		t.Fatalf("NewSharedDeps with empty model returned err=nil, deps=%v", deps)
	}
	if deps != nil {
		t.Fatalf("NewSharedDeps returned non-nil deps on error: %+v", deps)
	}
}

func TestNewSharedDepsBuildsConfiguredLLMClient(t *testing.T) {
	deps, err := NewSharedDeps(validLLMCfg("gpt-5.6-terra"))
	if err != nil {
		t.Fatalf("NewSharedDeps: %v", err)
	}
	if _, ok := deps.LLMClient.(*llm.Client); !ok {
		t.Fatalf("SharedDeps.LLMClient = %T, want *llm.Client", deps.LLMClient)
	}
}
