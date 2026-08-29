package main

import (
	"context"
	"database/sql"
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
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		Meta:      config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		Meta:       config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}},
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
		case "COMPSHARE_TRACE_SINK":
			return "mysql"
		default:
			return ""
		}
	}, config.MySQLConfig{DSN: "configured-dsn"})
	require.NoError(t, err)

	require.Equal(t, "configured-dsn", getenv("MYSQL_DSN"))
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

func TestNewServerHandlersAdvertisesInteractionCapabilities(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{Model: "test-model"},
		Meta: config.MetaConfig{
			Welcome: "welcome", SuggestedPrompts: []string{"prompt"},
		},
		HTTP: config.HTTPConfig{MaxInputLength: 4000},
	}}
	handlers := newServerHandlers(cfg, nil, serverTestMessageStore{}, nil, nil, nil)
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
		"server capabilities are fixed; each client still opts in per turn")
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

func TestBuildHTTPServerPoolWiresKnowledgeRetriever(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "gpt-5.6-terra"},
	}}

	pool, err := buildHTTPServerPool(cfg, serverTestMessageStore{}, func(key string) string {
		switch key {
		case "COMPSHARE_KB_MCP_URL":
			return "http://compshare-kb.example/mcp"
		}
		return ""
	}, nil) // nil db: this test does not exercise the SSH-ops lane
	require.NoError(t, err)
	defer pool.Close()

	eng, release, err := pool.Lease(context.Background(), store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "sess")
	require.NoError(t, err)
	release()
	require.IsType(t, &knowledge.MCPRetriever{}, eng.KnowledgeRetrieverPointer())
}

func TestApplySharedDepsDefaultsToKnowledgeMCP(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{
			BaseURL: "http://localhost:1",
			APIKey:  "llm-key",
			Model:   "gpt-5.6-terra",
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
