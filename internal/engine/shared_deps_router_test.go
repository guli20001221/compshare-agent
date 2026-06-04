package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// validRouterCfg is the minimal cfg that NewSharedDeps accepts after B2a:
// non-empty base.Model so llm.NewRouter does not reject it.
func validRouterCfg(model string) *config.Config {
	return &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{
			BaseURL: "https://example.test/v1",
			APIKey:  "test-key",
			Model:   model,
		},
	}}
}

// TestNewSharedDeps_EmptyBaseModel_ReturnsError pins the B2a allowed-change
// (memory acceptance-invariant-with-allowed-change): SharedDeps now builds its
// LLM client through llm.NewRouter (ADR-002 Acceptance #3), which validates
// base.Model is non-empty. A config with an empty model therefore fails loud at
// NewSharedDeps instead of deferring to the first LLM call (the pre-B2a
// llm.NewClient tolerated an empty model). config.Load always populates a model
// in production; this guards the new construction-time contract so a future
// edit can't silently restore the empty-model-tolerant path.
func TestNewSharedDeps_EmptyBaseModel_ReturnsError(t *testing.T) {
	deps, err := NewSharedDeps(validRouterCfg(""))
	if err == nil {
		t.Fatalf("NewSharedDeps with empty base.Model returned err=nil, deps=%v", deps)
	}
	if deps != nil {
		t.Fatalf("NewSharedDeps returned non-nil deps on error: %+v", deps)
	}
}

// TestNewSharedDeps_LLMClientIsRouterDerived asserts the main client is the
// Router-derived *llm.Client (B2a wiring), not some other implementation that
// could have crept in. The concrete model identity (base model when
// tier_routing is empty) is pinned by
// internal/llm/router_test.go::TestNewRouter_NilOverrides_AllTiersUseBaseModel,
// because *llm.Client deliberately exposes no model accessor — so byte-stability
// is verified at the Router layer, and this test only guards the wiring type.
func TestNewSharedDeps_LLMClientIsRouterDerived(t *testing.T) {
	deps, err := NewSharedDeps(validRouterCfg("deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("NewSharedDeps: %v", err)
	}
	if deps.LLMClient == nil {
		t.Fatal("NewSharedDeps returned nil LLMClient")
	}
	if _, ok := deps.LLMClient.(*llm.Client); !ok {
		t.Fatalf("SharedDeps.LLMClient = %T, want *llm.Client (Router-derived)", deps.LLMClient)
	}
}

func TestNewSharedDeps_TierRoutingAgentOverrideReachesAgentClient(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("parse request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := validRouterCfg("deepseek-v4-flash")
	cfg.Agent.LLM.BaseURL = srv.URL + "/v1"
	cfg.Agent.TierRouting = map[string]config.LLMConfig{
		"agent": {Model: "Qwen/Qwen3-Max"},
	}
	deps, err := NewSharedDeps(cfg)
	if err != nil {
		t.Fatalf("NewSharedDeps: %v", err)
	}

	_, err = deps.AgentLLMClient.Chat(context.Background(), llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("AgentLLMClient.Chat: %v", err)
	}
	if got := captured["model"]; got != "Qwen/Qwen3-Max" {
		t.Fatalf("agent tier model = %#v, want Qwen/Qwen3-Max", got)
	}
}

func TestNewSharedDeps_TierRoutingFastOverrideDrivesToolChoiceCapability(t *testing.T) {
	cfg := validRouterCfg("deepseek-v4-flash")
	cfg.Agent.LLM.BaseURL = "https://api.modelverse.cn/v1"
	cfg.Agent.TierRouting = map[string]config.LLMConfig{
		"fast": {Model: "Qwen/Qwen3-Max"},
	}

	deps, err := NewSharedDeps(cfg)
	if err != nil {
		t.Fatalf("NewSharedDeps: %v", err)
	}
	if !deps.SupportsObjectToolChoice {
		t.Fatal("SupportsObjectToolChoice should follow the effective fast tier model")
	}
}
