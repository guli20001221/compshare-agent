package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigUsesTrackedConfigForDefaultPath(t *testing.T) {
	oldConfigPath := configPath
	configPath = defaultConfigPath
	t.Cleanup(func() { configPath = oldConfigPath })

	t.Setenv("COMPSHARE_PUBLIC_KEY", "legacy-ak")
	t.Setenv("COMPSHARE_PRIVATE_KEY", "legacy-sk")
	t.Setenv("LLM_API_KEY", "llm-key")

	cfg, err := loadConfig()
	require.NoError(t, err)
	require.Equal(t, "deepseek-v4-flash", cfg.Agent.LLM.Model)
}

func TestValidateServerConfigRequiresMySQLDSN(t *testing.T) {
	cfg := &config.Config{}
	err := validateServerConfig(cfg)
	assert.Error(t, err)
}

func TestValidateServerConfigAcceptsRequiredFields(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:  config.HTTPConfig{MaxInputLength: 4000},
		STS: config.STSConfig{
			ServiceAK:       "test-ak",
			ServiceSK:       "test-sk",
			URL:             "https://api.ucloud.cn/",
			RoleUrnTemplate: "ucs:iam::%d:role/ucs-service-role/ServiceRoleForCompshare",
		},
	}}
	err := validateServerConfig(cfg)
	assert.NoError(t, err)
}

func TestValidateServerConfigRequiresSTSFields(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL:     config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:      config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:      config.HTTPConfig{MaxInputLength: 4000},
		PublicKey: "legacy-ak",
	}}

	err := validateServerConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "agent.private_key")
}

func TestValidateServerConfigAcceptsLegacyCredentialsWhenSTSAbsent(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL:      config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:       config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:       config.HTTPConfig{MaxInputLength: 4000},
		PublicKey:  "legacy-ak",
		PrivateKey: "legacy-sk",
	}}

	err := validateServerConfig(cfg)
	assert.NoError(t, err)
}

func TestValidateServerConfigRequiresSTSServiceSK(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:  config.HTTPConfig{MaxInputLength: 4000},
		STS:   config.STSConfig{ServiceAK: "ak", ServiceSK: "", URL: "https://api.ucloud.cn/", RoleUrnTemplate: "tpl"},
	}}
	err := validateServerConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "service_sk")
}

func TestValidateServerConfigRequiresSTSURL(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:  config.HTTPConfig{MaxInputLength: 4000},
		STS:   config.STSConfig{ServiceAK: "ak", ServiceSK: "sk", URL: "", RoleUrnTemplate: "tpl"},
	}}
	err := validateServerConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "url")
}

func TestValidateServerConfigRequiresSTSRoleUrnTemplate(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:  config.HTTPConfig{MaxInputLength: 4000},
		STS:   config.STSConfig{ServiceAK: "ak", ServiceSK: "sk", URL: "https://api.ucloud.cn/", RoleUrnTemplate: ""},
	}}
	err := validateServerConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "role_urn_template")
}

func TestValidateServerConfigAcceptsSTSDefaultRoleUrnWithoutTemplate(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:  config.HTTPConfig{MaxInputLength: 4000},
		STS: config.STSConfig{
			ServiceAK:      "ak",
			ServiceSK:      "sk",
			URL:            "https://api.ucloud.cn/",
			DefaultRoleUrn: "ucs:iam::123:role/demo",
		},
	}}

	err := validateServerConfig(cfg)
	assert.NoError(t, err)
}

func TestServerTraceGetenvUsesConfiguredMySQLDSN(t *testing.T) {
	getenv := serverTraceGetenv(func(key string) string {
		switch key {
		case "MYSQL_DSN":
			return "env-dsn"
		case "COMPSHARE_TRACE_ENABLED":
			return "1"
		case "COMPSHARE_TRACE_SINK":
			return "mysql"
		default:
			return ""
		}
	}, "configured-dsn")

	require.Equal(t, "configured-dsn", getenv("MYSQL_DSN"))
	require.Equal(t, "1", getenv("COMPSHARE_TRACE_ENABLED"))
	require.True(t, traceMySQLSinkEnabled(getenv))
}

type serverTestMessageStore struct{}

func (serverTestMessageStore) Append(context.Context, store.Message) error { return nil }
func (serverTestMessageStore) UpdateAssistant(context.Context, store.Owner, string, store.AssistantPatch) error {
	return nil
}
func (serverTestMessageStore) ListBySession(context.Context, string, int, string) ([]store.Message, string, error) {
	return nil, "", nil
}
func (serverTestMessageStore) MarkAssistantOutcome(context.Context, store.Owner, string, string, *string, *int, *int) error {
	return nil
}
func (serverTestMessageStore) GetWithOwnerCheck(context.Context, store.Owner, string) (store.Message, error) {
	return store.Message{}, sql.ErrNoRows
}

func TestBuildHTTPServerPoolAppliesSharedDepsEnv(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "deepseek-v4-flash"},
	}}

	pool, err := buildHTTPServerPool(cfg, serverTestMessageStore{}, func(key string) string {
		switch key {
		case "COMPSHARE_DIRECT_DISPATCH_INTENTS":
			return "resource"
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "off"
		}
		return ""
	})
	require.NoError(t, err)
	defer pool.Close()

	eng, err := pool.Get(context.Background(), store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "sess")
	require.NoError(t, err)
	require.NotNil(t, eng.IntentPlannerPointer(), "HTTP server pool should inherit intent planner env wiring")
}

func TestConfigureSharedDepsUnifiedCreateReachesServerPlanner(t *testing.T) {
	engine.SetUnifiedCreateEnabled(false)
	t.Cleanup(func() { engine.SetUnifiedCreateEnabled(true) })

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"schema_version\\\":\\\"1.0\\\",\\\"intent\\\":\\\"unknown\\\",\\\"slots\\\":{\\\"target_refs\\\":[],\\\"metrics\\\":[],\\\"time_window\\\":null},\\\"confidence\\\":0.1}\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	capabilityPath := filepath.Join(t.TempDir(), "capabilities.yaml")
	require.NoError(t, os.WriteFile(capabilityPath, []byte(`capabilities:
- base_url: "`+srv.URL+`/v1"
  model: "deepseek-v4-flash"
  supports_json_schema: true
  supports_json_object: true
`), 0o644))
	t.Setenv("COMPSHARE_LLM_CAPABILITY_FILE", capabilityPath)

	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "deepseek-v4-flash"},
	}}
	deps, _, err := configureSharedDepsFromEnv(cfg, func(key string) string {
		switch key {
		case "COMPSHARE_DIRECT_DISPATCH_INTENTS":
			return "resource"
		case "COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT":
			return "json_schema"
		case "USE_KNOWLEDGE_RETRIEVAL", "USE_GROUNDED_RENDERER":
			return "off"
		}
		return ""
	})
	require.NoError(t, err)
	require.NotNil(t, deps.IntentPlanner)

	_, err = deps.IntentPlanner.Plan(context.Background(), intent.IntentRouterInput{UserText: "创建一台4090"})
	require.NoError(t, err)

	messages, ok := captured["messages"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, messages)
	system, ok := messages[0].(map[string]any)
	require.True(t, ok)
	require.Contains(t, system["content"], "create_instance")

	responseFormat, ok := captured["response_format"].(map[string]any)
	require.True(t, ok)
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	require.True(t, ok)
	schema, ok := jsonSchema["schema"].(map[string]any)
	require.True(t, ok)
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	intentProp, ok := props["intent"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, intentProp["enum"], "create_instance")
}

func TestApplySharedDepsSessionFactContextFromEnv(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "deepseek-v4-flash"},
	}}
	deps := &engine.SharedDeps{}

	err := applySharedDepsFromEnv(deps, cfg, func(key string) string {
		switch key {
		case "USE_SESSION_FACT_CONTEXT":
			return "1"
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "off"
		case "USE_GROUNDED_RENDERER":
			return "off"
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.True(t, deps.SessionFactContextEnabled)
}

func TestApplySharedDepsReactResultProjectionFromEnv(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "deepseek-v4-flash"},
	}}
	deps := &engine.SharedDeps{}

	err := applySharedDepsFromEnv(deps, cfg, func(key string) string {
		switch key {
		case "USE_REACT_RESULT_PROJECTION":
			return "1"
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "off"
		case "USE_GROUNDED_RENDERER":
			return "off"
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.True(t, deps.ReactResultProjectionEnabled)
}

func TestApplySharedDepsReactHistoryCompactionFromEnv(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "deepseek-v4-flash"},
	}}
	deps := &engine.SharedDeps{}

	err := applySharedDepsFromEnv(deps, cfg, func(key string) string {
		switch key {
		case "USE_REACT_HISTORY_COMPACTION":
			return "1"
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "off"
		case "USE_GROUNDED_RENDERER":
			return "off"
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.True(t, deps.ReactHistoryCompactionEnabled)
}

func TestApplySharedDepsDefaultsToQwenRRFAndRenderer(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{
			BaseURL: "http://localhost:1",
			APIKey:  "llm-key",
			Model:   "deepseek-v4-flash",
		},
	}}
	deps := &engine.SharedDeps{}

	err := applySharedDepsFromEnv(deps, cfg, func(key string) string {
		switch key {
		case "LLM_API_KEY":
			return "llm-key"
		case "COMPSHARE_KNOWLEDGE_CORPUS":
			return filepath.Join("..", "deploy", "kb", "stage2b_w0.jsonl")
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.NotNil(t, deps.KnowledgeRetriever, "default runtime should enable qwen3_rrf retrieval")
	require.NotNil(t, deps.IntentPlanner, "default retrieval needs the intent planner")
	require.NotNil(t, deps.GroundedGenerator, "default runtime should enable LLM grounded renderer")
	require.Equal(t, "deepseek-v4-flash", deps.GroundedGeneratorModel)
	require.Equal(t, "deepseek-v4-flash", deps.IntentPlannerModel)
	require.Contains(t, deps.IntentRouteIntents, intent.IntentPricingQuery, "default runtime should cut over pricing queries")
}

func TestRootCommandDoesNotExposeWebSocketServe(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		require.NotEqual(t, "serve", cmd.Name())
	}
}
