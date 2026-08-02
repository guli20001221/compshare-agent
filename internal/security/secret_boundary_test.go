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
		"SshLoginCommand": "ssh root@1.2.3.4 -p 22", // real upstream field name (B8.3 deploy surfaces it into traces)
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
	// Reversed deliberately. This used to assert that the ssh+command key rule
	// caught the login-infix name too — it does, and that was the defect: the
	// authoritative SSH field reached the model blanked, so the agent could not
	// answer the question that field exists for. A plain login line carries no
	// credential (the instance Password is a separate upstream field), and the
	// exception fails closed on anything that is not one. SSHCommand above stays
	// redacted, which is what keeps this narrow — see
	// ssh_connection_visibility_test.go.
	assert.Equal(t, "ssh root@1.2.3.4 -p 22", redacted["SshLoginCommand"],
		"the authoritative SSH login line must reach the model intact")
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

func TestRedactForLLM_RedactsBearerTokensInStringValues(t *testing.T) {
	token := "eyJhbGciOiJIUzI1NiIs" + "InR5cCI6IkpXVCJ9.foo.bar"
	input := map[string]any{
		"Header":      "Authorization: " + "Bearer " + token,
		"Description": "Bearer-Class GPU image is a normal product label",
	}

	redacted := RedactForLLM(input).(map[string]any)

	assert.Equal(t, "Authorization: Bearer [REDACTED]", redacted["Header"])
	assert.Equal(t, "Bearer-Class GPU image is a normal product label", redacted["Description"])
}

func TestRedactForLLM_RedactsOperationalTokensInStringValues(t *testing.T) {
	input := map[string]any{
		"URL":         "http://1.2.3.4:8888?token=UCloud-CompShare-AbCd1234",
		"Description": "use UCloud-CompShare-AbCd1234 as a one-time access value",
		"Nested": map[string]any{
			"URL": "http://1.2.3.4:8888/lab?foo=bar&token=plain-token-123",
		},
	}

	redacted := RedactForLLM(input).(map[string]any)

	assert.Equal(t, "http://1.2.3.4:8888?token=[REDACTED]", redacted["URL"])
	assert.Equal(t, "use UCloud-CompShare-[REDACTED] as a one-time access value", redacted["Description"])
	nested := redacted["Nested"].(map[string]any)
	assert.Equal(t, "http://1.2.3.4:8888/lab?foo=bar&token=[REDACTED]", nested["URL"])
}

func TestRedactForLLM_RedactsOAuthStyleSecretKeys(t *testing.T) {
	input := map[string]any{
		"RefreshToken":  "refresh-token-value",
		"IDToken":       "id-token-value",
		"ClientSecret":  "client-secret-value",
		"WebhookSecret": "webhook-secret-value",
		"Credential":    "credential-value",
		"next_token":    "pagination-cursor",
	}

	redacted := RedactForLLM(input).(map[string]any)

	assert.Equal(t, "[REDACTED]", redacted["RefreshToken"])
	assert.Equal(t, "[REDACTED]", redacted["IDToken"])
	assert.Equal(t, "[REDACTED]", redacted["ClientSecret"])
	assert.Equal(t, "[REDACTED]", redacted["WebhookSecret"])
	assert.Equal(t, "[REDACTED]", redacted["Credential"])
	assert.Equal(t, "pagination-cursor", redacted["next_token"])
}

func TestRedactKnownSecretsInText_RedactsWorkflowPasswords(t *testing.T) {
	text := "已为实例重装，root 新密码是 SecurePass123，请用 SecurePass123 登录。"

	redacted := RedactKnownSecretsInText(text, []string{"SecurePass123", ""})

	assert.NotContains(t, redacted, "SecurePass123")
	assert.Contains(t, redacted, "[REDACTED]")
}

func TestContainsToolProtocolMarkup(t *testing.T) {
	assert.True(t, ContainsToolProtocolMarkup(`<｜DSML｜invoke name="RequestResetPassword">`))
	assert.True(t, ContainsToolProtocolMarkup(`<tool_call>{"name":"x"}</tool_call>`))
	assert.False(t, ContainsToolProtocolMarkup("我会先查询实例，再显示确认卡。"))
}
