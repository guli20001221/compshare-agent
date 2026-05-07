package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoad_ResolvesSecretPlaceholdersFromEnvironment(t *testing.T) {
	t.Setenv("COMPSHARE_PUBLIC_KEY", "public-from-env")
	t.Setenv("COMPSHARE_PRIVATE_KEY", "private-from-env")
	t.Setenv("LLM_API_KEY", "llm-from-env")
	t.Setenv("COMPSHARE_PROJECT_ID", "project-from-env")
	path := writeConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "${COMPSHARE_PUBLIC_KEY}"
  private_key: "${COMPSHARE_PRIVATE_KEY}"
  region: "cn-wlcb"
  project_id: "${COMPSHARE_PROJECT_ID}"
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    model: "deepseek-v4-flash"
`)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "public-from-env", cfg.Agent.PublicKey)
	assert.Equal(t, "private-from-env", cfg.Agent.PrivateKey)
	assert.Equal(t, "llm-from-env", cfg.Agent.LLM.APIKey)
	assert.Equal(t, "project-from-env", cfg.Agent.ProjectId)
}

func TestLoad_FailsFastWhenRequiredSecretPlaceholderEnvMissing(t *testing.T) {
	t.Setenv("COMPSHARE_PUBLIC_KEY", "")
	path := writeConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "${COMPSHARE_PUBLIC_KEY}"
  private_key: "${COMPSHARE_PRIVATE_KEY}"
  region: "cn-wlcb"
  project_id: ""
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    model: "deepseek-v4-flash"
`)

	_, err := Load(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "COMPSHARE_PUBLIC_KEY")
	assert.Contains(t, err.Error(), "environment variable")
}

func TestLoad_RejectsLiteralSecretValuesInYAML(t *testing.T) {
	path := writeConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "literal"
  private_key: "${COMPSHARE_PRIVATE_KEY}"
  region: "cn-wlcb"
  project_id: ""
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    model: "deepseek-v4-flash"
`)

	_, err := Load(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "public_key")
	assert.Contains(t, err.Error(), "must use ${")
}
