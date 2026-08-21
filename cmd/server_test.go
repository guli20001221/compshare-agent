package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/websearch"
	"github.com/gin-gonic/gin"
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
	require.Equal(t, "gpt-5.6-terra", cfg.Agent.LLM.Model)
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
	getenv, err := serverTraceGetenv(func(key string) string {
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
	}, config.MySQLConfig{DSN: "configured-dsn"})
	require.NoError(t, err)

	require.Equal(t, "configured-dsn", getenv("MYSQL_DSN"))
	require.Equal(t, "1", getenv("COMPSHARE_TRACE_ENABLED"))
	require.True(t, traceMySQLSinkEnabled(getenv))
}

func TestServerTraceGetenvAppliesProductionHostOverride(t *testing.T) {
	getenv, err := serverTraceGetenv(func(string) string { return "" }, config.MySQLConfig{
		DSN:          "postgresql://user:password@117.50.198.43:5432/postgres?sslmode=disable",
		HostOverride: "2003:da8:2004:1000:0a3c:7623:2712:f9c0",
	})
	require.NoError(t, err)

	parsed, err := url.Parse(getenv("MYSQL_DSN"))
	require.NoError(t, err)
	require.Equal(t, "2003:da8:2004:1000:0a3c:7623:2712:f9c0", parsed.Hostname())
	require.Equal(t, "5432", parsed.Port())
	require.Equal(t, "/postgres", parsed.Path)
	require.Equal(t, "disable", parsed.Query().Get("sslmode"))
}

func TestServerDurableCoordinatorReceivesProductionTraceWriter(t *testing.T) {
	writer := &captureAppendWriter{}
	secretKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	opts, err := serverTurnCoordinatorOptions(func(key string) string {
		if key == "COMPSHARE_ENABLE_MUTATING_TOOLS" {
			return "1"
		}
		if key == "COMPSHARE_TURN_SECRET_KEY" {
			return secretKey
		}
		return ""
	}, writer)
	require.NoError(t, err)
	require.Same(t, writer, opts.TraceWriter)
	require.True(t, opts.MutatingToolsEnabled)
	require.NotEmpty(t, opts.ReplicaID)
	require.Len(t, opts.SecretKey, 32)
}

func TestServerDurableCoordinatorRejectsMissingSecretKey(t *testing.T) {
	_, err := serverTurnCoordinatorOptions(func(string) string { return "" }, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "COMPSHARE_TURN_SECRET_KEY")
}

type recordingInteractionFeatures struct {
	confirmForm  bool
	guidedCreate bool
}

func (r *recordingInteractionFeatures) SetConfirmFormEnabled(value bool) {
	r.confirmForm = value
}

func (r *recordingInteractionFeatures) SetGuidedCreateEnabled(value bool) {
	r.guidedCreate = value
}

func TestConfigureInteractionFeaturesEnablesTheDurableHandlerCapabilities(t *testing.T) {
	tests := []struct {
		name                    string
		values                  map[string]string
		wantConfirm, wantGuided bool
	}{
		{name: "both enabled", values: map[string]string{
			"COMPSHARE_CONFIRM_FORM": "1", "COMPSHARE_GUIDED_CREATE": "1",
		}, wantConfirm: true, wantGuided: true},
		{name: "form only", values: map[string]string{
			"COMPSHARE_CONFIRM_FORM": "1", "COMPSHARE_GUIDED_CREATE": "0",
		}, wantConfirm: true},
		{name: "guided requires form", values: map[string]string{
			"COMPSHARE_CONFIRM_FORM": "0", "COMPSHARE_GUIDED_CREATE": "1",
		}},
		{name: "unknown values fail closed", values: map[string]string{
			"COMPSHARE_CONFIRM_FORM": "maybe", "COMPSHARE_GUIDED_CREATE": "maybe",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := &recordingInteractionFeatures{}
			configureInteractionFeatures(got, func(key string) string { return tc.values[key] })
			assert.Equal(t, tc.wantConfirm, got.confirmForm)
			assert.Equal(t, tc.wantGuided, got.guidedCreate)
		})
	}
}

func TestNewServerHandlersActuallyAdvertisesConfiguredInteractionFeatures(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{Model: "test-model"},
		Meta: config.MetaConfig{
			Welcome: "welcome", SuggestedPrompts: []string{"prompt"}, MaxInputLength: 4000,
		},
	}}
	handlers := newServerHandlers(
		cfg, nil, serverTestMessageStore{}, nil, nil, nil,
		func(key string) string {
			switch key {
			case "COMPSHARE_CONFIRM_FORM", "COMPSHARE_GUIDED_CREATE":
				return "1"
			default:
				return ""
			}
		},
	)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"Action":"GetCSAgentMeta","top_organization_id":1,"organization_id":2}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handlers.Dispatch(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Features []string `json:"Features"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, []string{"confirm_form_v1", "guided_create_v1"}, response.Features,
		"removing the server construction wiring must make this gate fail")
}

type serverTestMessageStore struct{}

func (serverTestMessageStore) Append(context.Context, store.Message) error { return nil }
func (serverTestMessageStore) UpdateAssistant(context.Context, store.Owner, string, store.AssistantPatch) error {
	return nil
}
func (serverTestMessageStore) ListBySession(context.Context, string, int, string) ([]store.Message, string, error) {
	return nil, "", nil
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
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "off"
		}
		return ""
	}, nil) // nil db: this test does not exercise the SSH-ops lane
	require.NoError(t, err)
	defer pool.Close()

	eng, err := pool.Get(context.Background(), store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "sess")
	require.NoError(t, err)
	require.Nil(t, eng.KnowledgeRetrieverPointer(), "disabled retrieval must stay disabled in pooled sessions")
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
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.True(t, deps.ReactResultProjectionEnabled)
}

func TestApplySharedDepsDefaultsToKnowledgeMCP(t *testing.T) {
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
		case "COMPSHARE_KB_MCP_URL":
			return "http://compshare-kb.example/mcp"
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.IsType(t, &knowledge.MCPRetriever{}, deps.KnowledgeRetriever)
}

func TestApplySharedDepsWebSearchIsOffByDefaultAndFailsClosedWhenMisconfigured(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{LLM: config.LLMConfig{
		BaseURL: "http://localhost:1", APIKey: "llm-key", Model: "deepseek-v4-flash",
	}}}
	deps := &engine.SharedDeps{}
	err := applySharedDepsFromEnv(deps, cfg, func(key string) string {
		if key == "COMPSHARE_KB_MCP_URL" {
			return "http://compshare-kb.example/mcp"
		}
		return ""
	})
	require.NoError(t, err)
	require.Nil(t, deps.WebSearcher, "no setting must not silently enable an external provider")

	err = applySharedDepsFromEnv(&engine.SharedDeps{}, cfg, func(key string) string {
		switch key {
		case "COMPSHARE_KB_MCP_URL":
			return "http://compshare-kb.example/mcp"
		case "COMPSHARE_WEB_SEARCH_ENABLED":
			return "1"
		default:
			return ""
		}
	})
	require.ErrorContains(t, err, "COMPSHARE_WEB_SEARCH_MCP_URL")

	deps = &engine.SharedDeps{}
	err = applySharedDepsFromEnv(deps, cfg, func(key string) string {
		switch key {
		case "COMPSHARE_KB_MCP_URL":
			return "http://compshare-kb.example/mcp"
		case "COMPSHARE_WEB_SEARCH_ENABLED":
			return "1"
		case "COMPSHARE_WEB_SEARCH_MCP_URL":
			return "https://search.example/mcp"
		default:
			return ""
		}
	})
	require.NoError(t, err)
	require.IsType(t, &websearch.MCPClient{}, deps.WebSearcher)
}

func TestRootCommandDoesNotExposeWebSocketServe(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		require.NotEqual(t, "serve", cmd.Name())
	}
}
