package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// HTTP / MySQL / Meta config tests
// ---------------------------------------------------------------------------

func baseConfigWithHTTPMySQLMeta(extra string) string {
	return baseConfig("") + extra
}

func TestLoad_HTTPDefaultsAppliedWhenSectionOmitted(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	h := cfg.Agent.HTTP
	assert.Equal(t, "0.0.0.0:8080", h.ListenAddr)
	assert.Equal(t, 30*time.Second, h.ReadTimeout)
	assert.Equal(t, time.Duration(0), h.WriteTimeout) // SSE: must stay 0
	assert.Equal(t, 15*time.Second, h.SSEKeepaliveInterval)
	assert.Equal(t, 4000, h.MaxInputLength)
	assert.Equal(t, 200, h.PoolCapacity)
	assert.Equal(t, 30*time.Minute, h.PoolIdleTTL)
}

func TestLoad_HTTPSectionFromYAML(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  http:
    listen_addr: "127.0.0.1:9090"
    read_timeout: "60s"
    write_timeout: "0s"
    sse_keepalive_interval: "20s"
    max_input_length: 2000
    pool_capacity: 100
    pool_idle_ttl: "15m"
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	h := cfg.Agent.HTTP
	assert.Equal(t, "127.0.0.1:9090", h.ListenAddr)
	assert.Equal(t, 60*time.Second, h.ReadTimeout)
	assert.Equal(t, time.Duration(0), h.WriteTimeout)
	assert.Equal(t, 20*time.Second, h.SSEKeepaliveInterval)
	assert.Equal(t, 2000, h.MaxInputLength)
	assert.Equal(t, 100, h.PoolCapacity)
	assert.Equal(t, 15*time.Minute, h.PoolIdleTTL)
}

func TestLoad_MySQLDefaultsAppliedWhenSectionOmitted(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	m := cfg.Agent.MySQL
	assert.Equal(t, "", m.DSN) // DSN not required by Load
	assert.Equal(t, 20, m.MaxOpenConns)
	assert.Equal(t, 5, m.MaxIdleConns)
	assert.Equal(t, time.Hour, m.ConnMaxLifetime)
}

func TestLoad_MySQLSectionFromYAML(t *testing.T) {
	setRequiredSecretEnv(t)
	t.Setenv("MYSQL_DSN", "user:pass@tcp(db:3306)/compshare_agent?parseTime=true")
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  mysql:
    dsn: "${MYSQL_DSN}"
    max_open_conns: 50
    max_idle_conns: 10
    conn_max_lifetime: "2h"
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	m := cfg.Agent.MySQL
	assert.Equal(t, "user:pass@tcp(db:3306)/compshare_agent?parseTime=true", m.DSN)
	assert.Equal(t, 50, m.MaxOpenConns)
	assert.Equal(t, 10, m.MaxIdleConns)
	assert.Equal(t, 2*time.Hour, m.ConnMaxLifetime)
}

func TestLoad_MissingMySQLDSNStillLoadsForCLICompatibility(t *testing.T) {
	setRequiredSecretEnv(t)
	// No MYSQL_DSN env var set — Load must succeed anyway.
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)

	require.NoError(t, err, "Load must succeed without mysql.dsn for CLI compatibility")
	assert.Equal(t, "", cfg.Agent.MySQL.DSN)
}

func TestLoad_MetaDefaultsInheritHTTPMaxInputLength(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig("")) // no meta section

	cfg, err := Load(path)
	require.NoError(t, err)

	// meta.max_input_length should default to http.max_input_length (4000)
	assert.Equal(t, cfg.Agent.HTTP.MaxInputLength, cfg.Agent.Meta.MaxInputLength)
	assert.Equal(t, 4000, cfg.Agent.Meta.MaxInputLength)
}

func TestLoad_MetaSectionFromYAML(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  meta:
    welcome: "Hello from agent"
    suggested_prompts:
      - "How do I create an instance?"
      - "Show my GPU inventory"
    max_input_length: 3000
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	meta := cfg.Agent.Meta
	assert.Equal(t, "Hello from agent", meta.Welcome)
	assert.Equal(t, []string{"How do I create an instance?", "Show my GPU inventory"}, meta.SuggestedPrompts)
	assert.Equal(t, 3000, meta.MaxInputLength)
}
