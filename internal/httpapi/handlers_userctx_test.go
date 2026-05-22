package httpapi

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildUserContext(t *testing.T) {
	h := &Handlers{cfg: &config.Config{Agent: config.AgentConfig{
		STS:    config.STSConfig{ServiceAK: "ak", ServiceSK: "sk", RoleUrnTemplate: "ucs:iam::%d:role/test"},
		Region: "cn-wlcb",
	}}}
	base := BaseRequest{Owner: store.Owner{TopOrganizationID: 123, OrganizationID: 456}}
	u, err := h.buildUserContext(base)
	require.NoError(t, err)
	assert.Equal(t, "ucs:iam::123:role/test", u.RoleUrn)
	assert.Equal(t, "123-456", u.SessionName)
	assert.Equal(t, "456", u.ProjectId)
	assert.Equal(t, "cn-wlcb", u.Region)
	assert.Equal(t, uint32(123), u.TopOrganizationID)
	assert.Equal(t, uint32(456), u.OrganizationID)
}

func TestBuildUserContextUsesRequestProjectID(t *testing.T) {
	h := &Handlers{cfg: &config.Config{Agent: config.AgentConfig{
		STS:    config.STSConfig{ServiceAK: "ak", ServiceSK: "sk", RoleUrnTemplate: "ucs:iam::%d:role/test"},
		Region: "cn-wlcb",
	}}}
	base := BaseRequest{
		Owner:     store.Owner{TopOrganizationID: 123, OrganizationID: 456},
		ProjectID: "org-cwy2qk",
	}
	u, err := h.buildUserContext(base)
	require.NoError(t, err)
	assert.Equal(t, "org-cwy2qk", u.ProjectId)
}

func TestBuildUserContextLegacyCredentialModeAllowsEmptyRole(t *testing.T) {
	h := &Handlers{cfg: &config.Config{Agent: config.AgentConfig{
		Region: "cn-wlcb",
	}}}
	base := BaseRequest{
		Owner:     store.Owner{TopOrganizationID: 123, OrganizationID: 456},
		ProjectID: "org-cwy2qk",
	}

	u, err := h.buildUserContext(base)
	require.NoError(t, err)
	assert.Empty(t, u.RoleUrn)
	assert.Equal(t, "123-456", u.SessionName)
	assert.Equal(t, "org-cwy2qk", u.ProjectId)
	assert.Equal(t, "cn-wlcb", u.Region)
}

func TestBuildUserContextUsesDefaultRoleUrnWhenConfigured(t *testing.T) {
	h := &Handlers{cfg: &config.Config{Agent: config.AgentConfig{
		STS: config.STSConfig{
			ServiceAK:      "ak",
			ServiceSK:      "sk",
			DefaultRoleUrn: "ucs:iam::999:role/demo",
		},
		Region: "cn-wlcb",
	}}}
	base := BaseRequest{Owner: store.Owner{TopOrganizationID: 123, OrganizationID: 456}}

	u, err := h.buildUserContext(base)
	require.NoError(t, err)
	assert.Equal(t, "ucs:iam::999:role/demo", u.RoleUrn)
}

func TestBuildUserContextZeroTopOrg(t *testing.T) {
	h := &Handlers{cfg: &config.Config{Agent: config.AgentConfig{
		STS:    config.STSConfig{ServiceAK: "ak", ServiceSK: "sk", RoleUrnTemplate: "ucs:iam::%d:role/test"},
		Region: "cn-wlcb",
	}}}
	// TopOrganizationID = 0 should return ErrInvalidParam
	base := BaseRequest{Owner: store.Owner{TopOrganizationID: 0, OrganizationID: 456}}
	_, err := h.buildUserContext(base)
	require.Error(t, err)
	apiErr, ok := err.(*APIError)
	require.True(t, ok, "expected *APIError")
	assert.Equal(t, ErrInvalidParam.Code, apiErr.Code)
}
