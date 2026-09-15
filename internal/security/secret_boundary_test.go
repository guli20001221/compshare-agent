package security

import (
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactForTrace_RedactsCredentialFieldsRecursively(t *testing.T) {
	input := map[string]any{
		"PublicKey":       "pub-1234567890",
		"PrivateKey":      "priv-1234567890",
		"api_key":         "llm-key-1234567890",
		"Password":        "secret-password",
		"SSHCommand":      "ssh root@1.2.3.4 -p 22",
		"SshLoginCommand": "ssh root@1.2.3.4 -p 22",
		"JupyterLabToken": "token-abc",
		"RefreshToken":    "refresh-token-value",
		"ClientSecret":    "client-secret-value",
		"Credential":      "credential-value",
		"next_token":      "pagination-cursor",
		"Nested": map[string]any{
			"access_token": "nested-token",
		},
		"Items": []any{
			map[string]any{"SecretKey": "secret-key"},
		},
	}

	redacted := RedactForTrace(input).(map[string]any)

	for _, key := range []string{"PublicKey", "PrivateKey", "api_key", "Password", "SSHCommand", "SshLoginCommand",
		"JupyterLabToken", "RefreshToken", "ClientSecret", "Credential"} {
		assert.Equal(t, "[REDACTED]", redacted[key], key)
	}
	assert.Equal(t, "pagination-cursor", redacted["next_token"])
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
		"IPs":           []any{"10.1.2.3", "not-an-ip"},
		"Password":      "secret-password",
		"next_token":    "pagination-cursor",
	}

	redacted := RedactForTrace(input).(map[string]any)

	assert.Equal(t, "[HASH:4ebc4a141b378980]", redacted["ChargeAmount"])
	assert.Equal(t, "[HASH:093dda9cb5db57a8]", redacted["BillingDetail"])
	assert.Equal(t, "123.45.x.x", redacted["PublicIP"])
	assert.Equal(t, "10.9.x.x", redacted["PrivateIP"])
	assert.Equal(t, []any{"10.1.x.x", "not-an-ip"}, redacted["IPs"])
	assert.Equal(t, "[REDACTED]", redacted["Password"])
	assert.Equal(t, "pagination-cursor", redacted["next_token"])
}

// Only the field name decides. A value is never scanned for credential-looking
// text: a JupyterLab URL, a Bearer header quoted in a description or a shell
// snippet inside a remark are hashed as the platform returned them.
func TestRedactForTraceIsDecidedByFieldNameNotByValueShape(t *testing.T) {
	input := map[string]any{
		"URL":         "http://1.2.3.4:8888?token=UCloud-CompShare-AbCd1234",
		"Header":      "Authorization: Bearer " + "eyJhbGciOiJIUzI1NiIs" + "InR5cCI6IkpXVCJ9.foo.bar",
		"Remark":      `export OPENAI_API_KEY="sk-remark-0123456789"; os.environ["X"]`,
		"Description": "Bearer-Class GPU image is a normal product label",
		"Nested": map[string]any{
			"URL": "http://1.2.3.4:8888/lab?foo=bar&token=plain-token-123",
		},
		"Items": []any{"token: not-a-field-name"},
	}

	redacted := RedactForTrace(input).(map[string]any)

	assert.Equal(t, input["URL"], redacted["URL"])
	assert.Equal(t, input["Header"], redacted["Header"])
	assert.Equal(t, input["Remark"], redacted["Remark"])
	assert.Equal(t, input["Description"], redacted["Description"])
	assert.Equal(t, input["Nested"], redacted["Nested"])
	assert.Equal(t, input["Items"], redacted["Items"])
}

func TestAssistantPersistenceRemovesThePrivateCustomerSupportMarker(t *testing.T) {
	persisted := PersistedAssistantText("先看结论。" + agentprotocol.FeishuCustomerSupportMarker)
	assert.Equal(t, "先看结论。"+agentprotocol.CustomerSupportHistoryCompletion, persisted)
	assert.NotContains(t, persisted, agentprotocol.FeishuCustomerSupportMarker)
	assert.Equal(t, persisted, PersistedAssistantText(persisted), "the persisted form is idempotent across hot/cold replay")
}

func TestPersistedAssistantTextKeepsCredentialShapedProseVerbatim(t *testing.T) {
	const reply = "curl -L 'https://civitai.example/download?Authorization=signed-token-abcdefghijklmnopqrst' -o model.safetensors\n" +
		"os.environ[\"OPENAI_API_KEY\"] = \"sk-example-0123456789\"，密码是 Abc12345 吗？"
	assert.Equal(t, reply, PersistedAssistantText(reply),
		"persistence does not rewrite prose: a reloaded command must be the command the user saw")
}

func TestRedactKnownSecretsInText_RedactsOnlyTheGivenValues(t *testing.T) {
	text := "已为实例重装，root 新密码是 SecurePass123，请用 SecurePass123 登录；token=other-value-0123456789 不变。"

	redacted := RedactKnownSecretsInText(text, []string{"SecurePass123", "", "abc"})

	assert.NotContains(t, redacted, "SecurePass123")
	assert.Contains(t, redacted, "[REDACTED]")
	assert.Contains(t, redacted, "token=other-value-0123456789", "values the caller did not name are not guessed at")
}

func TestContainsToolProtocolMarkup(t *testing.T) {
	assert.True(t, ContainsToolProtocolMarkup(`<｜DSML｜invoke name="RequestResetPassword">`))
	assert.True(t, ContainsToolProtocolMarkup(`<tool_call>{"name":"x"}</tool_call>`))
	assert.False(t, ContainsToolProtocolMarkup("我会先查询实例，再显示确认卡。"))
}

func TestUserAuthorizationHeadersMintsAReferenceWithoutRewritingText(t *testing.T) {
	const (
		authorization = "Bear" + "er auth-canary-0123456789"
		signedURL     = "https://models.example/file?token=signed-url-0123456789"
	)
	refs := UserAuthorizationHeaders("请检查 " + signedURL + "\n-H 'Authorization: " + authorization + "'")
	require.Len(t, refs, 1)
	assert.Equal(t, "current-user-authorization-1", refs[0].Reference)
	assert.Equal(t, authorization, refs[0].Value)
}

func TestUserAuthorizationHeadersNeverMintsAURLQueryCapability(t *testing.T) {
	assert.Empty(t, UserAuthorizationHeaders("https://example.test/check?authorization=Bearer-secret-0123456789"),
		"a signed URL is not an Authorization header")
}

func TestUserAuthorizationHeadersFindsAHeaderAfterASignedURL(t *testing.T) {
	const (
		query  = "https://example.test/check?Authorization=signed-url-0123456789"
		header = "Bear" + "er auth-canary-0123456789"
	)
	refs := UserAuthorizationHeaders("下载地址 " + query + "\nAuthorization: " + header)
	require.Len(t, refs, 1)
	assert.Equal(t, header, refs[0].Value)
}

func TestRedactKnownAuthorizationTextRemovesTheCapturedValueAndItsCredentialPart(t *testing.T) {
	const header = "Bear" + "er auth-canary-0123456789"
	task := "用 Authorization: " + header + " 验证 /v1/models；也试试只带 auth-canary-0123456789；Authorization: Bearer other-0123456789 保留"

	redacted := RedactKnownAuthorizationText(task, []string{header, " ", "abc"})

	assert.NotContains(t, redacted, "auth-canary-0123456789")
	assert.Contains(t, redacted, "Authorization: [REDACTED] 验证 /v1/models")
	assert.Contains(t, redacted, "Authorization: Bearer other-0123456789 保留",
		"a header the request channel did not capture is not guessed at")
}
