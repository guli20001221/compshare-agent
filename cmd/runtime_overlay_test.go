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
// config.RuntimeGetenv (on="1"; off="0" except mutating_tools, whose off=="")
// is consumed cleanly by the cmd/ *FromEnv parsers: an explicit YAML value
// resolves to the right bool and never produces an "unknown value" warning.
func TestRuntimeOverlayFeedsParsersWithoutWarnings(t *testing.T) {
	t.Run("mutating off encodes to empty string, no warn", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Authorization: config.AuthorizationConfig{MutatingTools: boolPtr(false)}}}
		enabled, unknown := mutatingToolsEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
		assert.False(t, enabled)
		assert.Empty(t, unknown, "off must not be flagged as unknown")
	})
	t.Run("mutating on", func(t *testing.T) {
		cfg := &config.Config{Agent: config.AgentConfig{Authorization: config.AuthorizationConfig{MutatingTools: boolPtr(true)}}}
		enabled, unknown := mutatingToolsEnabledFromEnv(cfg.RuntimeGetenv(emptyBase))
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
