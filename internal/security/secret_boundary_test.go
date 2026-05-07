package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactForLLM_RedactsSecretsRecursively(t *testing.T) {
	input := map[string]any{
		"PublicKey":       "pub-1234567890",
		"PrivateKey":      "priv-1234567890",
		"api_key":         "llm-key-1234567890",
		"Password":        "secret-password",
		"SSHCommand":      "ssh root@1.2.3.4 -p 22",
		"JupyterLabToken": "token-abc",
		"PublicIP":        "1.2.3.4",
		"Nested": map[string]any{
			"access_token": "nested-token",
		},
		"Items": []any{
			map[string]any{"SecretKey": "secret-key"},
		},
	}

	redacted := RedactForLLM(input).(map[string]any)

	assert.Equal(t, "[REDACTED]", redacted["PublicKey"])
	assert.Equal(t, "[REDACTED]", redacted["PrivateKey"])
	assert.Equal(t, "[REDACTED]", redacted["api_key"])
	assert.Equal(t, "[REDACTED]", redacted["Password"])
	assert.Equal(t, "[REDACTED]", redacted["SSHCommand"])
	assert.Equal(t, "[REDACTED]", redacted["JupyterLabToken"])
	assert.Equal(t, "1.2.3.4", redacted["PublicIP"], "IP is not hidden from LLM context by default")
	assert.Equal(t, "[REDACTED]", redacted["Nested"].(map[string]any)["access_token"])
	assert.Equal(t, "[REDACTED]", redacted["Items"].([]any)[0].(map[string]any)["SecretKey"])

	assert.Equal(t, "priv-1234567890", input["PrivateKey"], "redaction must not mutate original input")
}

func TestRedactForTrace_HashesBillingAndMasksIP(t *testing.T) {
	input := map[string]any{
		"ChargeAmount":  "123.45",
		"BillingDetail": "gpu hourly charge",
		"PublicIP":      "123.45.67.89",
		"PrivateIP":     "10.9.8.7",
		"Password":      "secret-password",
		"next_token":    "pagination-cursor",
	}

	redacted := RedactForTrace(input).(map[string]any)

	assert.Equal(t, "[HASH:4ebc4a141b378980]", redacted["ChargeAmount"])
	assert.Equal(t, "[HASH:093dda9cb5db57a8]", redacted["BillingDetail"])
	assert.Equal(t, "123.45.x.x", redacted["PublicIP"])
	assert.Equal(t, "10.9.x.x", redacted["PrivateIP"])
	assert.Equal(t, "[REDACTED]", redacted["Password"])
	assert.Equal(t, "pagination-cursor", redacted["next_token"])
}
