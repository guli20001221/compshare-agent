package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestBuildFeishuAuthorizationURLUsesOnlyExternalImageScopes(t *testing.T) {
	raw, err := buildFeishuAuthorizationURL("cli_test", "http://127.0.0.1:18765/feishu-oauth/callback", "state-test")
	require.NoError(t, err)
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "accounts.feishu.cn", parsed.Host)
	query := parsed.Query()
	require.Equal(t, "cli_test", query.Get("client_id"))
	require.Equal(t, "state-test", query.Get("state"))
	require.Equal(t, "im:message:readonly im:message.group_msg:get_as_user offline_access", query.Get("scope"))
}

func TestValidateLoopbackRedirectURLRejectsPublicEndpoint(t *testing.T) {
	_, err := validateLoopbackRedirectURL("https://example.com/callback")
	require.ErrorContains(t, err, "loopback")
	parsed, err := validateLoopbackRedirectURL("http://127.0.0.1:18765/feishu-oauth/callback")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", parsed.Hostname())
}

func TestWriteFeishuOAuthBootstrapTokenKeepsEnableAsBoolean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`agent:
  feishu:
    external_image_oauth:
      enabled: false
      redirect_url: "http://127.0.0.1:18765/feishu-oauth/callback"
      bootstrap_refresh_token: ""
`), 0o600))
	written, err := writeFeishuOAuthBootstrapToken(path, "test-refresh-token", true)
	require.NoError(t, err)
	require.Equal(t, path, written)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw struct {
		Agent struct {
			Feishu config.FeishuConfig `yaml:"feishu"`
		} `yaml:"agent"`
	}
	require.NoError(t, yaml.Unmarshal(data, &raw))
	require.True(t, raw.Agent.Feishu.ExternalImageOAuth.Enabled)
	require.Equal(t, "test-refresh-token", raw.Agent.Feishu.ExternalImageOAuth.BootstrapRefreshToken)
	require.True(t, strings.Contains(string(data), "bootstrap_refresh_token"))
}
