package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func setRequiredSecretEnv(t *testing.T) {
	t.Helper()
	t.Setenv("COMPSHARE_PUBLIC_KEY", "public-from-env")
	t.Setenv("COMPSHARE_PRIVATE_KEY", "private-from-env")
	t.Setenv("LLM_API_KEY", "llm-from-env")
}

func baseConfig(rateLimitYAML string) string {
	return `
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
` + rateLimitYAML
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

func TestLoad_OmittedRateLimitUsesDefaults(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, governance.DefaultLLMQPS, cfg.Agent.RateLimit.LLMQPS)
	assert.Equal(t, governance.DefaultLLMDaily, cfg.Agent.RateLimit.LLMDaily)
	assert.Equal(t, governance.DefaultMutatingQPS, cfg.Agent.RateLimit.MutatingQPS)
	assert.Equal(t, governance.DefaultMutatingDaily, cfg.Agent.RateLimit.MutatingDaily)
	assert.Equal(t, governance.DefaultReadExpensiveQPS, cfg.Agent.RateLimit.ReadExpensiveQPS)
	assert.Equal(t, governance.DefaultReadExpensiveDaily, cfg.Agent.RateLimit.ReadExpensiveDaily)
	assert.Equal(t, governance.DefaultLimits(), cfg.Agent.RateLimit.Limits())
}

func TestLoad_RateLimitPartialOverridesMergeWithDefaults(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  rate_limit:
    llm_qps: 9
    mutating_daily: 7
    read_expensive_qps: 2
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 9, cfg.Agent.RateLimit.LLMQPS)
	assert.Equal(t, governance.DefaultLLMDaily, cfg.Agent.RateLimit.LLMDaily)
	assert.Equal(t, governance.DefaultMutatingQPS, cfg.Agent.RateLimit.MutatingQPS)
	assert.Equal(t, 7, cfg.Agent.RateLimit.MutatingDaily)
	assert.Equal(t, 2, cfg.Agent.RateLimit.ReadExpensiveQPS)
	assert.Equal(t, governance.DefaultReadExpensiveDaily, cfg.Agent.RateLimit.ReadExpensiveDaily)
	assert.Equal(t, governance.Limits{
		LLMQPS:             9,
		LLMDaily:           governance.DefaultLLMDaily,
		MutatingQPS:        governance.DefaultMutatingQPS,
		MutatingDaily:      7,
		ReadExpensiveQPS:   2,
		ReadExpensiveDaily: governance.DefaultReadExpensiveDaily,
	}, cfg.Agent.RateLimit.Limits())
}

func TestLoad_RejectsNegativeRateLimitValues(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "llm qps",
			yaml: `
  rate_limit:
    llm_qps: -1
`,
			wantErr: "agent.rate_limit.llm_qps",
		},
		{
			name: "llm daily",
			yaml: `
  rate_limit:
    llm_daily: -1
`,
			wantErr: "agent.rate_limit.llm_daily",
		},
		{
			name: "mutating qps",
			yaml: `
  rate_limit:
    mutating_qps: -1
`,
			wantErr: "agent.rate_limit.mutating_qps",
		},
		{
			name: "mutating daily",
			yaml: `
  rate_limit:
    mutating_daily: -1
`,
			wantErr: "agent.rate_limit.mutating_daily",
		},
		{
			name: "read expensive qps",
			yaml: `
  rate_limit:
    read_expensive_qps: -1
`,
			wantErr: "agent.rate_limit.read_expensive_qps",
		},
		{
			name: "read expensive daily",
			yaml: `
  rate_limit:
    read_expensive_daily: -1
`,
			wantErr: "agent.rate_limit.read_expensive_daily",
		},
		{
			name: "user turn qps",
			yaml: `
  rate_limit:
    user_turn_qps: -1
`,
			wantErr: "agent.rate_limit.user_turn_qps",
		},
		{
			name: "user turn daily",
			yaml: `
  rate_limit:
    user_turn_daily: -1
`,
			wantErr: "agent.rate_limit.user_turn_daily",
		},
		{
			name: "max tokens per turn",
			yaml: `
  rate_limit:
    max_tokens_per_turn: -1
`,
			wantErr: "agent.rate_limit.max_tokens_per_turn",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredSecretEnv(t)
			path := writeConfig(t, baseConfig(tc.yaml))

			_, err := Load(path)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "must be non-negative")
			assert.Contains(t, err.Error(), "0 or omit to use default")
		})
	}
}
