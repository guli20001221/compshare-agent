package main

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIUserContextFromConfigDefaultsToNoContext(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		Region:    "cn-wlcb",
		ProjectId: "org-cwy2qk",
	}}

	_, ok := cliUserContextFromConfig(cfg, func(string) string { return "" })

	assert.False(t, ok)
}

func TestCLIUserContextFromConfigKeepsDefaultRoleBehavior(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		Region:    "cn-wlcb",
		ProjectId: "org-cwy2qk",
		STS: config.STSConfig{
			DefaultRoleUrn:     "ucs:iam::123:role/demo",
			DefaultSessionName: "cli-smoke",
		},
	}}

	u, ok := cliUserContextFromConfig(cfg, func(string) string { return "" })

	require.True(t, ok)
	assert.Equal(t, "ucs:iam::123:role/demo", u.RoleUrn)
	assert.Equal(t, "cli-smoke", u.SessionName)
	assert.Equal(t, "org-cwy2qk", u.ProjectId)
	assert.Equal(t, "cn-wlcb", u.Region)
	assert.Empty(t, u.UserEmail)
}

func TestCLIUserContextFromConfigInjectsSmokeUserEmail(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		Region:    "cn-wlcb",
		ProjectId: "org-cwy2qk",
	}}

	u, ok := cliUserContextFromConfig(cfg, func(key string) string {
		if key == "COMPSHARE_USER_EMAIL" {
			return " operator@example.com "
		}
		return ""
	})

	require.True(t, ok)
	assert.Empty(t, u.RoleUrn)
	assert.Equal(t, "org-cwy2qk", u.ProjectId)
	assert.Equal(t, "cn-wlcb", u.Region)
	assert.Equal(t, "operator@example.com", u.UserEmail)
}
