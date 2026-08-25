package opscontext

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeAuthorizationStaysOutOfContextJSONAndFormatting(t *testing.T) {
	const secret = "Bear" + "er auth-canary-0123456789"
	ctx := Context{
		SchemaVersion: SchemaVersion,
		ProbeAuthorizations: []ProbeAuthorization{{
			Reference: "current-user-authorization-1",
			Value:     secret,
		}},
	}
	raw, err := json.Marshal(ctx)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secret)
	assert.NotContains(t, string(raw), "current-user-authorization")

	rendered := fmt.Sprintf("%+v %#v", ctx, ctx)
	assert.NotContains(t, rendered, secret)
	assert.Contains(t, rendered, "[REDACTED]")
}

func TestProbeAuthorizationPrivateWireStillCarriesTheExactValue(t *testing.T) {
	const secret = "Custom   auth-canary-0123456789"
	raw, err := json.Marshal(ProbeAuthorization{
		Reference: "current-user-authorization-1",
		Value:     secret,
	})
	require.NoError(t, err)
	assert.Contains(t, string(raw), secret,
		"the dedicated supervisor stdin handshake, unlike Context JSON, needs the exact value")
}
