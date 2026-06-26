package main

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/assert"
)

func boolPtr(b bool) *bool { return &b }

// emptyBase is a getenv that returns "" for everything — the "no env set" case.
func emptyBase(string) string { return "" }

// TestRuntimeOverlayFeedsParsersWithoutWarnings verifies the encoding chosen by
// config.RuntimeGetenv (on="1"; off="0" except mutating_tools/skill_executor
// whose off=="") is consumed cleanly by the cmd/ *FromEnv parsers — i.e. an
// explicit YAML value resolves to the right bool AND never produces the
// non-empty "unknown value" warning string. This is the contract that lets the
// whole flag layer keep reading through a single getenv after the YAML migration.
func TestRuntimeOverlayFeedsParsersWithoutWarnings(t *testing.T) {
	t.Run("mutating off encodes to empty string, no warn", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{MutatingTools: boolPtr(false)}}}
		enabled, unknown := mutatingToolsEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.False(t, enabled)
		assert.Empty(t, unknown, "off must not be flagged as unknown")
	})
	t.Run("mutating on", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{MutatingTools: boolPtr(true)}}}
		enabled, unknown := mutatingToolsEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.True(t, enabled)
		assert.Empty(t, unknown)
	})
	t.Run("skill executor off encodes to empty string, no warn", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{SkillExecutor: boolPtr(false)}}}
		enabled, unknown := useSkillExecutorFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.False(t, enabled)
		assert.Empty(t, unknown)
	})
	t.Run("default-on flag explicit false encodes to 0, no warn", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{AgenticSearchKnowledge: boolPtr(false)}}}
		enabled, unknown := agenticSearchKnowledgeEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.False(t, enabled, "explicit YAML false turns the default-on flag off")
		assert.Empty(t, unknown, "0 must be a clean off, not unknown")
	})
	t.Run("default-on flag omitted stays on via default", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{}} // nothing set
		enabled, unknown := agenticSearchKnowledgeEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.True(t, enabled, "omitted + no env → built-in default ON preserved")
		assert.Empty(t, unknown)
	})
	t.Run("create preference extractor explicit true", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{CreatePreferenceExtractor: boolPtr(true)}}}
		enabled, unknown := createPreferenceExtractorEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.True(t, enabled)
		assert.Empty(t, unknown)
	})
	t.Run("create preference extractor explicit false wins over env", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{CreatePreferenceExtractor: boolPtr(false)}}}
		base := func(key string) string {
			if key == "COMPSHARE_CREATE_PREF_EXTRACTOR" {
				return "1"
			}
			return ""
		}
		enabled, unknown := createPreferenceExtractorEnabledFromEnv(cfg.RuntimeGetenv(base))
		assert.False(t, enabled)
		assert.Empty(t, unknown)
	})
	t.Run("knowledge_qa agent loop omitted stays on", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{}}
		enabled, unknown := knowledgeQAAgentLoopEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.True(t, enabled)
		assert.Empty(t, unknown)
	})
	t.Run("omitted bool falls through to env", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{}}
		base := func(key string) string {
			if key == "COMPSHARE_ENABLE_MUTATING_TOOLS" {
				return "1"
			}
			return ""
		}
		enabled, _ := mutatingToolsEnabledFromEnv(cfg.RuntimeGetenv(base))
		assert.True(t, enabled, "YAML omitted → env fallback applies")
	})
}
