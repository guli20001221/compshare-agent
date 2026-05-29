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

// TestLoad_TierRouting_Empty_BackwardCompat verifies that legacy configs
// without a tier_routing block continue to Load without error — the
// backward-compat invariant called out in ADR-002 Acceptance #5.
func TestLoad_TierRouting_Empty_BackwardCompat(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(""))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Agent.TierRouting, "TierRouting should be empty when block omitted")
}

// TestLoad_TierRouting_ValidKeys parses a full ADR-002 example tier_routing
// block and verifies all three tier overrides land in cfg.Agent.TierRouting.
func TestLoad_TierRouting_ValidKeys(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  tier_routing:
    fast:
      model: "deepseek-v4-flash"
    knowledge:
      model: "deepseek-v4-flash"
    agent:
      model: "deepseek-v4-pro"
`))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Agent.TierRouting, 3)
	assert.Equal(t, "deepseek-v4-flash", cfg.Agent.TierRouting["fast"].Model)
	assert.Equal(t, "deepseek-v4-flash", cfg.Agent.TierRouting["knowledge"].Model)
	assert.Equal(t, "deepseek-v4-pro", cfg.Agent.TierRouting["agent"].Model)
}

// TestLoad_TierRouting_UnknownKey_FailsLoud catches the silent-typo
// regression — "knowlege" (missing d) must reject at boot, not no-op.
// Fail-loud invariant: unknown key in a routing map silently no-ops the
// override, which masks a real misconfig until first runtime mis-route.
func TestLoad_TierRouting_UnknownKey_FailsLoud(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  tier_routing:
    knowlege:
      model: "deepseek-v4-flash"
`))
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent.tier_routing.knowlege")
	assert.Contains(t, err.Error(), "unknown tier")
}

// TestLoad_TierRouting_APIKey_PlaceholderResolved verifies that a tier
// override api_key written as ${ENV_VAR} gets resolved from the
// environment, matching the base agent.llm.api_key behavior. Without
// this, operators writing tier-specific keys would carry literal env
// var syntax into runtime LLM calls (silent breakage).
func TestLoad_TierRouting_APIKey_PlaceholderResolved(t *testing.T) {
	setRequiredSecretEnv(t)
	t.Setenv("ALT_LLM_KEY", "resolved-from-env")
	path := writeConfig(t, baseConfig(`
  tier_routing:
    agent:
      model: "deepseek-v4-pro"
      api_key: "${ALT_LLM_KEY}"
`))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "resolved-from-env", cfg.Agent.TierRouting["agent"].APIKey)
}

// TestLoad_TierRouting_APIKey_LiteralRejected catches the security gap
// the B1 reviewer flagged: tier override api_key MUST use ${ENV_VAR}
// placeholder like the base path. Literal secrets in YAML are rejected
// to match resolveRequiredSecret's contract for agent.llm.api_key.
func TestLoad_TierRouting_APIKey_LiteralRejected(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  tier_routing:
    agent:
      model: "deepseek-v4-pro"
      api_key: "sk-literal-leaked"
`))
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent.tier_routing.agent.api_key")
}

// TestLoad_TierRouting_APIKey_EmptyInheritsFromBase verifies the common
// case where a tier override only sets model and inherits api_key from
// the base agent.llm config (no explicit api_key in the override).
func TestLoad_TierRouting_APIKey_EmptyInheritsFromBase(t *testing.T) {
	setRequiredSecretEnv(t)
	path := writeConfig(t, baseConfig(`
  tier_routing:
    agent:
      model: "deepseek-v4-pro"
`))
	cfg, err := Load(path)
	require.NoError(t, err)
	// Override APIKey stays empty; Router merge logic falls back to base.
	assert.Empty(t, cfg.Agent.TierRouting["agent"].APIKey)
	// Base APIKey is still resolved.
	assert.Equal(t, "llm-from-env", cfg.Agent.LLM.APIKey)
}
