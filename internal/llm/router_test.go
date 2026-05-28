package llm

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
)

func baseConfig() config.LLMConfig {
	return config.LLMConfig{
		BaseURL: "https://api.modelverse.cn/v1",
		APIKey:  "test-key",
		Model:   "deepseek-v4-flash",
	}
}

// Backward compat: nil/empty tier_routing means every tier uses the base
// model. Legacy configs must continue to work — this is the ADR-002
// acceptance #5 invariant ("tier_routing 为空时 N=10 回归确认行为跟改造前
// 一致").
func TestNewRouter_NilOverrides_AllTiersUseBaseModel(t *testing.T) {
	r, err := NewRouter(baseConfig(), nil)
	if err != nil {
		t.Fatalf("NewRouter returned error: %v", err)
	}
	for _, tier := range AllTiers {
		if got := r.Model(tier); got != "deepseek-v4-flash" {
			t.Errorf("tier %q: expected base model deepseek-v4-flash, got %q", tier, got)
		}
	}
}

func TestNewRouter_EmptyOverrides_AllTiersUseBaseModel(t *testing.T) {
	r, err := NewRouter(baseConfig(), map[Tier]config.LLMConfig{})
	if err != nil {
		t.Fatalf("NewRouter returned error: %v", err)
	}
	for _, tier := range AllTiers {
		if got := r.Model(tier); got != "deepseek-v4-flash" {
			t.Errorf("tier %q: expected base model deepseek-v4-flash, got %q", tier, got)
		}
	}
}

// Per-tier override only changes the specified tier; others stay on base.
// This is the ADR-002 Decision config example (fast=flash, knowledge=flash,
// agent=pro).
func TestNewRouter_AgentTierOverride_OnlyAgentSwitches(t *testing.T) {
	overrides := map[Tier]config.LLMConfig{
		TierAgent: {Model: "deepseek-v4-pro"},
	}
	r, err := NewRouter(baseConfig(), overrides)
	if err != nil {
		t.Fatalf("NewRouter returned error: %v", err)
	}
	if got := r.Model(TierFast); got != "deepseek-v4-flash" {
		t.Errorf("TierFast: expected deepseek-v4-flash, got %q", got)
	}
	if got := r.Model(TierKnowledge); got != "deepseek-v4-flash" {
		t.Errorf("TierKnowledge: expected deepseek-v4-flash, got %q", got)
	}
	if got := r.Model(TierAgent); got != "deepseek-v4-pro" {
		t.Errorf("TierAgent: expected deepseek-v4-pro, got %q", got)
	}
}

// Override with only Model specified inherits BaseURL + APIKey from base.
// Real-world use: ds-v4-pro lives on the same ModelVerse endpoint as flash,
// so writing `tier_routing.agent.model: "deepseek-v4-pro"` alone should
// suffice — no need to repeat base_url/api_key.
func TestNewRouter_ModelOnlyOverride_InheritsBaseURLAndAPIKey(t *testing.T) {
	overrides := map[Tier]config.LLMConfig{
		TierAgent: {Model: "deepseek-v4-pro"},
	}
	r, err := NewRouter(baseConfig(), overrides)
	if err != nil {
		t.Fatalf("NewRouter returned error: %v", err)
	}
	cfg := r.configs[TierAgent]
	if cfg.BaseURL != "https://api.modelverse.cn/v1" {
		t.Errorf("TierAgent should inherit BaseURL, got %q", cfg.BaseURL)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("TierAgent should inherit APIKey, got %q", cfg.APIKey)
	}
}

// Override with BaseURL+Model both set switches endpoint cleanly. This
// matches the "secondary provider for one tier" pattern (e.g., point
// agent tier at Anthropic-compat endpoint while keeping fast/knowledge
// on ModelVerse).
func TestNewRouter_FullOverride_ReplacesAllFields(t *testing.T) {
	overrides := map[Tier]config.LLMConfig{
		TierAgent: {
			BaseURL: "https://api.alt-provider.example/v1",
			APIKey:  "alt-key",
			Model:   "claude-opus-4-7",
		},
	}
	r, err := NewRouter(baseConfig(), overrides)
	if err != nil {
		t.Fatalf("NewRouter returned error: %v", err)
	}
	cfg := r.configs[TierAgent]
	if cfg.BaseURL != "https://api.alt-provider.example/v1" {
		t.Errorf("BaseURL not overridden: %q", cfg.BaseURL)
	}
	if cfg.APIKey != "alt-key" {
		t.Errorf("APIKey not overridden: %q", cfg.APIKey)
	}
	if cfg.Model != "claude-opus-4-7" {
		t.Errorf("Model not overridden: %q", cfg.Model)
	}
	// fast/knowledge unaffected
	if r.configs[TierFast].BaseURL != "https://api.modelverse.cn/v1" {
		t.Errorf("TierFast BaseURL leaked: %q", r.configs[TierFast].BaseURL)
	}
}

// Empty base.Model is a config error — must reject at boot, not silently
// fall back. Memory `disclaimer-misfire-3class-bucketing` / surface
// failure loud, don't paper over.
func TestNewRouter_EmptyBaseModel_ReturnsError(t *testing.T) {
	base := baseConfig()
	base.Model = ""
	_, err := NewRouter(base, nil)
	if err == nil {
		t.Fatal("expected error for empty base.Model, got nil")
	}
	if !strings.Contains(err.Error(), "base.Model") {
		t.Errorf("error message should mention base.Model, got: %v", err)
	}
}

// Unknown tier is a programmer bug — panic so callers can't silently
// drift past the misroute. ADR-002 "Router 不做兜底, misrouting 是
// planner bug 不是 router bug".
func TestRouter_For_UnknownTier_Panics(t *testing.T) {
	r, err := NewRouter(baseConfig(), nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic for unknown tier, got nil")
		}
	}()
	r.For(Tier("nonexistent"))
}

func TestRouter_Model_UnknownTier_Panics(t *testing.T) {
	r, err := NewRouter(baseConfig(), nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic for unknown tier, got nil")
		}
	}()
	r.Model(Tier("nonexistent"))
}

// Router.Capability delegates to LookupCapability with the tier's
// effective (BaseURL, Model) tuple — proves Router is wired into the
// existing capability matrix, not a parallel store.
func TestRouter_Capability_MatchesLookupCapability(t *testing.T) {
	r, err := NewRouter(baseConfig(), nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	got := r.Capability(TierFast)
	want := LookupCapability("https://api.modelverse.cn/v1", "deepseek-v4-flash")
	if got.IsThinkingMode != want.IsThinkingMode {
		t.Errorf("Capability.IsThinkingMode: got %v, want %v", got.IsThinkingMode, want.IsThinkingMode)
	}
	if got.SupportsObjectToolChoice != want.SupportsObjectToolChoice {
		t.Errorf("Capability.SupportsObjectToolChoice: got %v, want %v", got.SupportsObjectToolChoice, want.SupportsObjectToolChoice)
	}
}

// AllTiers must stay in fast→knowledge→agent order — downstream code
// (e.g., observability iteration) may depend on this.
func TestAllTiers_OrderInvariant(t *testing.T) {
	want := []Tier{TierFast, TierKnowledge, TierAgent}
	if len(AllTiers) != len(want) {
		t.Fatalf("AllTiers length: got %d, want %d", len(AllTiers), len(want))
	}
	for i, tier := range want {
		if AllTiers[i] != tier {
			t.Errorf("AllTiers[%d]: got %q, want %q", i, AllTiers[i], tier)
		}
	}
}
