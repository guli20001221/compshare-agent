package main

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/assert"
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
	// Empty STS should cause an error mentioning service_ak
	cfg := &config.Config{Agent: config.AgentConfig{
		MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
		Meta:  config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
		HTTP:  config.HTTPConfig{MaxInputLength: 4000},
		// STS left at zero-value — all fields empty
	}}

	err := validateServerConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "service_ak")
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
