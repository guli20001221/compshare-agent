package main

import (
	"context"
	"database/sql"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		if key == "USE_INTENT_PLANNER_FOR" {
			return "resource"
		}
		return ""
	})
	require.NoError(t, err)
	defer pool.Close()

	eng, err := pool.Get(context.Background(), store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "sess")
	require.NoError(t, err)
	require.NotNil(t, eng.IntentPlannerPointer(), "HTTP server pool should inherit intent planner env wiring")
}

func TestRootCommandDoesNotExposeWebSocketServe(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		require.NotEqual(t, "serve", cmd.Name())
	}
}
