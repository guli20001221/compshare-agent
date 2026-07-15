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
	// The flag it replaces was opt-in, so "omitted → ON" is the whole change: a
	// deploy that says nothing must now get the deterministic table. Without this
	// assertion the flip is invisible — config.yaml carries no such key today.
	t.Run("agent deterministic render omitted uses default on", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{}}
		enabled, unknown := agentDeterministicRenderEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.True(t, enabled, "omitted + no env → default ON; a silent-off here reinstates the invented-instance bug")
		assert.Empty(t, unknown)
	})
	t.Run("agent deterministic render explicit false is the rollback", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Features: config.FeaturesConfig{AgentDeterministicRender: boolPtr(false)}}}
		enabled, unknown := agentDeterministicRenderEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.False(t, enabled)
		assert.Empty(t, unknown)
	})
	t.Run("agent deterministic render unknown value is reported, not coerced", func(t *testing.T) {
		enabled, unknown := agentDeterministicRenderEnabledFromEnv(func(k string) string {
			if k == "COMPSHARE_AGENT_DETERMINISTIC_RENDER" {
				return "maybe"
			}
			return ""
		})
		assert.False(t, enabled)
		assert.Equal(t, "maybe", unknown, "an unknown value must surface as a warning, never silently pass as on")
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
