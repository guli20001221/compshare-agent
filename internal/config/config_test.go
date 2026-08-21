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
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestDeployConfigStagesDurableExecutionOffForSafeClusterCutover(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.prod.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent.Features.DurableTurns)
	assert.False(t, *cfg.Agent.Features.DurableTurns,
		"the tracked production config must not activate durable execution during a rolling binary deploy")
}

func TestDeployConfigPinsContainerSSHOpsRuntime(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.prod.yaml"))
	require.NoError(t, err)
	assert.Equal(t,
		"/opt/compshare-agent/deploy/ssh_ops_harness/harness.py",
		cfg.Agent.SSHOps.HarnessPath,
	)
	assert.Equal(t, "/opt/miniforge3/envs/py313/bin/python", cfg.Agent.SSHOps.Python)
}

func TestProductionConfigUsesProductionKnowledgeService(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.prod.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "http://compshare-kb.prj-ucompshare-prod.svc.c5.uae/mcp", cfg.Agent.Retrieval.MCPURL)
	assert.Equal(t, "2003:da8:2004:1000:0a3c:7623:2712:f9c0", cfg.Agent.MySQL.HostOverride)
}

func TestProductionConfigOnlyAutoRepliesToAllowlistedTopicRoots(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.prod.yaml"))
	require.NoError(t, err)
	assert.True(t, cfg.Agent.Feishu.AutoReplyNewTopics)
	assert.False(t, cfg.Agent.Feishu.AutoReplyAllMessages)
}

func TestLoad_ExtendsBaseConfigAndOverridesNestedValues(t *testing.T) {
	setRequiredSecretEnv(t)
	dir := t.TempDir()
	basePath := filepath.Join(dir, "config.local.yaml")
	prodPath := filepath.Join(dir, "config.prod.yaml")
	require.NoError(t, os.WriteFile(basePath, []byte(baseConfig(`
  retrieval:
    mcp_url: "http://local-kb/mcp"
  mysql:
    host_override: "127.0.0.1"
`)), 0o600))
	require.NoError(t, os.WriteFile(prodPath, []byte(`
extends: config.local.yaml
agent:
  retrieval:
    mcp_url: "http://prod-kb/mcp"
  mysql:
    host_override: "2001:db8::1"
`), 0o600))

	cfg, err := Load(prodPath)
	require.NoError(t, err)
	assert.Equal(t, "external", cfg.Agent.Executor, "base fields must survive the overlay")
	assert.Equal(t, "http://prod-kb/mcp", cfg.Agent.Retrieval.MCPURL)
	assert.Equal(t, "2001:db8::1", cfg.Agent.MySQL.HostOverride)
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

func TestLoad_AcceptsInlineLiteralSecretValuesInYAML(t *testing.T) {
	// YAML-first config migration: a self-contained config.yaml may inline
	// secrets so a deployment needs no env file at all. The committed
	// deploy/conf/config.local.yaml now carries the shared deploy literals directly.
	path := writeConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "ak-inline"
  private_key: "sk-inline"
  region: "cn-wlcb"
  project_id: "org-inline"
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "llm-inline"
    model: "deepseek-v4-flash"
`)

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "ak-inline", cfg.Agent.PublicKey)
	assert.Equal(t, "sk-inline", cfg.Agent.PrivateKey)
	assert.Equal(t, "llm-inline", cfg.Agent.LLM.APIKey)
	assert.Equal(t, "org-inline", cfg.Agent.ProjectId)
}

func TestLoad_RejectsMalformedDollarSecretPlaceholder(t *testing.T) {
	// A "$"-prefixed value that is not a valid ${...} placeholder is a typo
	// (e.g. "$COMPSHARE_PUBLIC_KEY") and must fail loud, not be taken as a
	// literal secret.
	path := writeConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "$COMPSHARE_PUBLIC_KEY"
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
	assert.Contains(t, err.Error(), "${ENV_VAR}")
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

// The SSH-ops lane is configured under agent.ssh_ops. This loads a real config through Load (not a
// hand-filled struct) so the whole YAML→SSHOpsConfig mapping is exercised: the tri-state Enabled *bool,
// the harness strings, the time.Duration timeout, and the RuntimeGetenv precedence where enabled: true
// wins as COMPSHARE_SSH_OPS=1 over the env fallback.
func TestLoad_SSHOpsConfigParses(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  ssh_ops:
    enabled: true
    harness_path: /opt/harness.py
    base_url: https://api.modelverse.cn
    api_key: ssh-ops-test-key
    python: python3
    model: gpt-5.6-terra
    timeout: "5m"
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.Agent.SSHOps.Enabled)
	assert.True(t, *cfg.Agent.SSHOps.Enabled)
	assert.Equal(t, "/opt/harness.py", cfg.Agent.SSHOps.HarnessPath)
	assert.Equal(t, "https://api.modelverse.cn", cfg.Agent.SSHOps.BaseURL)
	assert.Equal(t, "ssh-ops-test-key", cfg.Agent.SSHOps.APIKey)
	assert.Equal(t, "gpt-5.6-terra", cfg.Agent.SSHOps.Model)
	assert.Equal(t, 5*time.Minute, cfg.Agent.SSHOps.Timeout)
	// enabled: true must win over the env fallback (COMPSHARE_SSH_OPS unset here).
	assert.Equal(t, "1", cfg.RuntimeGetenv(func(string) string { return "" })("COMPSHARE_SSH_OPS"))
}

// Omitted agent.ssh_ops leaves Enabled nil (tri-state), so RuntimeGetenv falls through to the env,
// then to the built-in default (off) — the lane stays off for an unconfigured deploy.
func TestLoad_SSHOpsOmittedFallsThroughToEnv(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Nil(t, cfg.Agent.SSHOps.Enabled, "omitted ssh_ops must stay tri-state nil")
	// with the field omitted, RuntimeGetenv does not override — the env fallback is returned verbatim.
	assert.Equal(t, "envval", cfg.RuntimeGetenv(func(k string) string {
		if k == "COMPSHARE_SSH_OPS" {
			return "envval"
		}
		return ""
	})("COMPSHARE_SSH_OPS"))
}

func TestLoad_OCRModelDefaultsFromModelVerseEnv(t *testing.T) {
	setRequiredSecretEnv(t)
	t.Setenv("MODELVERSE_QWEN_VL_MODEL", "qwen3-vl-flash")
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "qwen3-vl-flash", cfg.Agent.OCR.Model)
	assert.Equal(t, cfg.Agent.LLM.BaseURL, cfg.Agent.OCR.BaseURL)
	assert.Equal(t, cfg.Agent.LLM.APIKey, cfg.Agent.OCR.APIKey)
}

func TestLoad_OCRMaxBytesDefaultsToFiveMB(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 5*1024*1024, cfg.Agent.OCR.MaxBytes)
}

func TestLoad_OCRStaysDisabledWhenModelUnset(t *testing.T) {
	setRequiredSecretEnv(t)
	t.Setenv("MODELVERSE_QWEN_VL_MODEL", "")
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Empty(t, cfg.Agent.OCR.Model)
	assert.Equal(t, cfg.Agent.LLM.BaseURL, cfg.Agent.OCR.BaseURL)
	assert.Equal(t, cfg.Agent.LLM.APIKey, cfg.Agent.OCR.APIKey)
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
		SSHExecQPS:         governance.DefaultSSHExecQPS,
		SSHExecDaily:       governance.DefaultSSHExecDaily,
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
	assert.Equal(t, "0.0.0.0:7429", h.ListenAddr)
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
	// Both http and meta max_input_length must agree to avoid the mismatch error.
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  http:
    max_input_length: 3000
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

// ---------------------------------------------------------------------------
// resolveOptionalDSN — accepts any ${ENV_VAR} placeholder (item 1)
// ---------------------------------------------------------------------------

func TestLoad_DSNPlaceholderUnsetReturnsEmpty(t *testing.T) {
	setRequiredSecretEnv(t)
	// MYSQL_DSN is not set; DSN field must be "" after Load (no error)
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  mysql:
    dsn: "${MYSQL_DSN}"
`))

	cfg, err := Load(path)
	require.NoError(t, err, "Load must succeed when ${MYSQL_DSN} is unset")
	assert.Equal(t, "", cfg.Agent.MySQL.DSN)
}

func TestLoad_DSNLiteralPassesThrough(t *testing.T) {
	setRequiredSecretEnv(t)
	const literal = "literal:dsn@tcp(db:3306)/mydb?parseTime=true"
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  mysql:
    dsn: "`+literal+`"
`))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, literal, cfg.Agent.MySQL.DSN)
}

func TestLoad_DSNArbitraryPlaceholderResolvesWhenSet(t *testing.T) {
	setRequiredSecretEnv(t)
	t.Setenv("DATABASE_URL", "user:pass@tcp(other:3306)/db?parseTime=true")
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  mysql:
    dsn: "${DATABASE_URL}"
`))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "user:pass@tcp(other:3306)/db?parseTime=true", cfg.Agent.MySQL.DSN)
}

func TestLoad_DSNBadPlaceholderFormatReturnsError(t *testing.T) {
	setRequiredSecretEnv(t)
	// "$MYSQL_DSN" without braces must be rejected
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  mysql:
    dsn: "$MYSQL_DSN"
`))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mysql.dsn")
	assert.Contains(t, err.Error(), "${ENV_VAR}")
}

// ---------------------------------------------------------------------------
// Negative numeric values — HTTPConfig / MySQLConfig / MetaConfig (item 3)
// ---------------------------------------------------------------------------

func TestLoad_RejectsNegativeHTTPValues(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "pool_capacity",
			yaml: `
  http:
    pool_capacity: -1
`,
			wantErr: "agent.http.pool_capacity",
		},
		{
			name: "max_input_length",
			yaml: `
  http:
    max_input_length: -1
`,
			wantErr: "agent.http.max_input_length",
		},
		{
			name: "pool_idle_ttl",
			yaml: `
  http:
    pool_idle_ttl: "-1s"
`,
			wantErr: "agent.http.pool_idle_ttl",
		},
		{
			name: "read_timeout",
			yaml: `
  http:
    read_timeout: "-1s"
`,
			wantErr: "agent.http.read_timeout",
		},
		{
			name: "write_timeout",
			yaml: `
  http:
    write_timeout: "-1s"
`,
			wantErr: "agent.http.write_timeout",
		},
		{
			name: "sse_keepalive_interval",
			yaml: `
  http:
    sse_keepalive_interval: "-1s"
`,
			wantErr: "agent.http.sse_keepalive_interval",
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
		})
	}
}

func TestLoad_RejectsNegativeMySQLValues(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "max_open_conns",
			yaml: `
  mysql:
    max_open_conns: -1
`,
			wantErr: "agent.mysql.max_open_conns",
		},
		{
			name: "max_idle_conns",
			yaml: `
  mysql:
    max_idle_conns: -1
`,
			wantErr: "agent.mysql.max_idle_conns",
		},
		{
			name: "conn_max_lifetime",
			yaml: `
  mysql:
    conn_max_lifetime: "-1s"
`,
			wantErr: "agent.mysql.conn_max_lifetime",
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
		})
	}
}

func TestLoad_RejectsNegativeMetaValues(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  meta:
    max_input_length: -1
`))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent.meta.max_input_length")
	assert.Contains(t, err.Error(), "must be non-negative")
}

// ---------------------------------------------------------------------------
// max_input_length mismatch between http and meta (item 4)
// ---------------------------------------------------------------------------

func TestLoad_RejectsMaxInputLengthMismatch(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  http:
    max_input_length: 4000
  meta:
    max_input_length: 2000
`))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_input_length")
	assert.Contains(t, err.Error(), "conflict")
}

func TestLoad_NoErrorWhenMetaMaxInputLengthInheritsDefault(t *testing.T) {
	setRequiredSecretEnv(t)
	// meta section is absent — it inherits from http; no mismatch error expected
	path := writeConfig(t, baseConfig(`
  http:
    max_input_length: 4000
`))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 4000, cfg.Agent.Meta.MaxInputLength)
}

func TestLoad_NoErrorWhenBothMaxInputLengthMatch(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  http:
    max_input_length: 2000
  meta:
    max_input_length: 2000
`))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 2000, cfg.Agent.Meta.MaxInputLength)
}

// ---------------------------------------------------------------------------
// STS config tests
// ---------------------------------------------------------------------------

func baseConfigWithSTS(stsYAML string) string {
	return baseConfig("") + stsYAML
}

func TestLoadSTSConfigFromYAML(t *testing.T) {
	setRequiredSecretEnv(t)
	t.Setenv("COMPSHARE_SERVICE_PUBLIC_KEY", "svc-ak-from-env")
	t.Setenv("COMPSHARE_SERVICE_PRIVATE_KEY", "svc-sk-from-env")
	t.Setenv("COMPSHARE_DEFAULT_ROLE_URN", "ucs:iam::12345:role/ucs-service-role/ServiceRoleForCompshare")

	path := writeConfig(t, baseConfigWithSTS(`
  sts:
    service_ak: "${COMPSHARE_SERVICE_PUBLIC_KEY}"
    service_sk: "${COMPSHARE_SERVICE_PRIVATE_KEY}"
    url: "https://api.ucloud.cn/"
    role_urn_template: "ucs:iam::%d:role/ucs-service-role/ServiceRoleForCompshare"
    default_role_urn: "${COMPSHARE_DEFAULT_ROLE_URN}"
    default_session_name: "agent-cli"
    duration_seconds: 7200
    refresh_before: "10m"
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	s := cfg.Agent.STS
	assert.Equal(t, "svc-ak-from-env", s.ServiceAK)
	assert.Equal(t, "svc-sk-from-env", s.ServiceSK)
	assert.Equal(t, "https://api.ucloud.cn/", s.URL)
	assert.Equal(t, "ucs:iam::%d:role/ucs-service-role/ServiceRoleForCompshare", s.RoleUrnTemplate)
	assert.Equal(t, "ucs:iam::12345:role/ucs-service-role/ServiceRoleForCompshare", s.DefaultRoleUrn)
	assert.Equal(t, "agent-cli", s.DefaultSessionName)
	assert.Equal(t, 7200, s.DurationSeconds)
	assert.Equal(t, 10*time.Minute, s.RefreshBefore)
}

func TestLoadSTSConfigDefaults(t *testing.T) {
	setRequiredSecretEnv(t)
	// No sts section at all — defaults must be applied.
	path := writeConfig(t, baseConfig(""))

	cfg, err := Load(path)
	require.NoError(t, err)

	s := cfg.Agent.STS
	assert.Equal(t, 3600, s.DurationSeconds)
	assert.Equal(t, 5*time.Minute, s.RefreshBefore)
	assert.Equal(t, "agent-default", s.DefaultSessionName)
	assert.Equal(t, "https://api.ucloud.cn/", s.URL)
}

func TestLoadWithoutPublicPrivateKeySucceeds(t *testing.T) {
	// Unset the public/private key env vars — Load must still succeed.
	t.Setenv("COMPSHARE_PUBLIC_KEY", "")
	t.Setenv("COMPSHARE_PRIVATE_KEY", "")
	t.Setenv("LLM_API_KEY", "llm-key")

	path := writeConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  region: "cn-wlcb"
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    model: "deepseek-v4-flash"
`)

	cfg, err := Load(path)
	require.NoError(t, err, "Load must succeed without public_key/private_key")
	assert.Equal(t, "", cfg.Agent.PublicKey)
	assert.Equal(t, "", cfg.Agent.PrivateKey)
}

func TestValidateSTSConfigRejectsNegativeDuration(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfigWithSTS(`
  sts:
    duration_seconds: -1
`))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent.sts.duration_seconds")
	assert.Contains(t, err.Error(), "must be non-negative")
}

func TestValidateSTSConfigRejectsNegativeRefreshBefore(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfigWithSTS(`
  sts:
    refresh_before: "-1s"
`))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent.sts.refresh_before")
	assert.Contains(t, err.Error(), "must be non-negative")
}

// ---------------------------------------------------------------------------
// Runtime feature flags — YAML sections + RuntimeGetenv overlay
// ---------------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

func TestLoad_RuntimeSectionsFromYAML(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  features:
    mutating_tools: true
    confirm_form: true
    guided_create: true
    react_result_projection: true
  retrieval:
    knowledge_retrieval: curated
    mcp_url: http://compshare-kb.example/mcp
    mcp_bearer_token: read-only-test-token
    mcp_timeout_ms: 12000
  web_search:
    enabled: true
    provider: exa
    mcp_url: https://mcp.exa.ai/mcp
    mcp_api_key: test-key
    mcp_timeout_ms: 7000
  trace:
    enabled: true
    sink: mysql
`))

	cfg, err := Load(path)
	require.NoError(t, err)

	f := cfg.Agent.Features
	require.NotNil(t, f.MutatingTools)
	assert.True(t, *f.MutatingTools)

	assert.Equal(t, "http://compshare-kb.example/mcp", cfg.Agent.Retrieval.MCPURL)
	assert.Equal(t, "read-only-test-token", cfg.Agent.Retrieval.MCPBearerToken)
	assert.Equal(t, 12000, cfg.Agent.Retrieval.MCPTimeoutMS)
	require.NotNil(t, cfg.Agent.WebSearch.Enabled)
	assert.True(t, *cfg.Agent.WebSearch.Enabled)
	assert.Equal(t, "exa", cfg.Agent.WebSearch.Provider)
	assert.Equal(t, "https://mcp.exa.ai/mcp", cfg.Agent.WebSearch.MCPURL)
	assert.Equal(t, "test-key", cfg.Agent.WebSearch.MCPAPIKey)
	assert.Equal(t, 7000, cfg.Agent.WebSearch.MCPTimeoutMS)
	require.NotNil(t, cfg.Agent.Trace.Enabled)
	assert.True(t, *cfg.Agent.Trace.Enabled)
	assert.Equal(t, "mysql", cfg.Agent.Trace.Sink)
}

func TestRuntimeGetenv_YAMLWinsWithEnvFallback(t *testing.T) {
	cfg := &Config{Agent: AgentConfig{
		LLM: LLMConfig{APIKey: "resolved-llm-key"},
		Features: FeaturesConfig{
			MutatingTools: boolPtr(true), // YAML true → "1"
			DurableTurns:  boolPtr(true),
		},
		Retrieval: RetrievalConfig{
			MCPURL:         "http://kb.example/mcp",
			MCPBearerToken: "read-only-test-token",
			MCPTimeoutMS:   9000,
			// KnowledgeRetrieval omitted → base env fallback
		},
		WebSearch: WebSearchConfig{
			Enabled:      boolPtr(true),
			Provider:     "exa",
			MCPURL:       "https://mcp.exa.ai/mcp",
			MCPAPIKey:    "test-key",
			MCPTimeoutMS: 7000,
		},
		Trace: TraceConfig{Sink: "file"},
	}}

	base := func(key string) string {
		switch key {
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "off"
		case "SOME_UNMAPPED_VAR":
			return "passthrough"
		}
		return ""
	}
	getenv := cfg.RuntimeGetenv(base)

	assert.Equal(t, "1", getenv("COMPSHARE_ENABLE_MUTATING_TOOLS"), "YAML true wins")
	assert.Equal(t, "1", getenv("COMPSHARE_DURABLE_TURNS"), "production durable-turn switch is sourced from YAML")
	assert.Equal(t, "http://kb.example/mcp", getenv("COMPSHARE_KB_MCP_URL"), "remote knowledge endpoint comes from YAML")
	assert.Equal(t, "read-only-test-token", getenv("COMPSHARE_KB_MCP_BEARER_TOKEN"), "read-only MCP token comes from YAML")
	assert.Equal(t, "9000", getenv("COMPSHARE_KB_MCP_TIMEOUT_MS"), "remote knowledge timeout comes from YAML")
	assert.Equal(t, "1", getenv("COMPSHARE_WEB_SEARCH_ENABLED"), "web search remains an explicit opt-in")
	assert.Equal(t, "exa", getenv("COMPSHARE_WEB_SEARCH_PROVIDER"))
	assert.Equal(t, "https://mcp.exa.ai/mcp", getenv("COMPSHARE_WEB_SEARCH_MCP_URL"))
	assert.Equal(t, "test-key", getenv("COMPSHARE_WEB_SEARCH_MCP_API_KEY"))
	assert.Equal(t, "7000", getenv("COMPSHARE_WEB_SEARCH_MCP_TIMEOUT_MS"))
	assert.Equal(t, "off", getenv("USE_KNOWLEDGE_RETRIEVAL"), "omitted string → env fallback")
	assert.Equal(t, "file", getenv("COMPSHARE_TRACE_SINK"))
	assert.Equal(t, "resolved-llm-key", getenv("LLM_API_KEY"), "resolved answer-model secret is exposed")
	assert.Equal(t, "passthrough", getenv("SOME_UNMAPPED_VAR"), "unmapped key → base passthrough")
}

func TestRuntimeGetenv_NilConfigReturnsBase(t *testing.T) {
	var cfg *Config
	base := func(string) string { return "from-base" }
	getenv := cfg.RuntimeGetenv(base)
	assert.Equal(t, "from-base", getenv("ANYTHING"))
}

// The quota is enforced at load, so a deployment's YAML cannot exceed it even
// though the repo's own YAML does not. It used to be enforced for a stronger
// reason — a session outliving the model's replay window forgot its own opening
// with no error and no log line — and the message said so. The engine has no
// count window any more, so the message must no longer claim one: a wrong reason
// in a boot error sends the next operator looking at the engine.
func TestLoadRejectsSessionTurnCapAboveTheQuota(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  http:
    max_session_turns: 21
`))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent.http.max_session_turns=21")
	assert.Contains(t, err.Error(), "session quota")
	assert.NotContains(t, err.Error(), "replayed-history window",
		"the engine no longer has a replay window to exceed")
}

func TestLoadAcceptsSessionTurnCapAtTheQuota(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfigWithHTTPMySQLMeta(`
  http:
    max_session_turns: 20
`))

	cfg, err := Load(path)
	require.NoError(t, err, "the boundary value itself must remain loadable")
	assert.Equal(t, MaxSessionTurnsCeiling, cfg.Agent.HTTP.MaxSessionTurns)
}

// The fallback used when max_session_turns is omitted is subject to the same
// quota, but never passes through validateHTTPConfig.
func TestDefaultTurnCapFitsSessionQuota(t *testing.T) {
	assert.LessOrEqual(t, DefaultMaxSessionTurns, MaxSessionTurnsCeiling)
}
