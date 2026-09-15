package guardrails

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthorizationHeaderValuesExtractsEachDistinctHeaderOnce(t *testing.T) {
	const (
		bearer = "Bear" + "er auth-canary-0123456789"
		basic  = "Basic YWxhZGRpbjpvcGVuc2VzYW1l"
		custom = "Signature custom-token-0123456789"
	)
	input := "curl -H 'Authorization: " + bearer + "' /health\n" +
		`{"Authorization": "` + basic + `"}` + "\n" +
		"Authorization：" + custom + "，请验证\n" +
		"Authorization: " + bearer
	require.Equal(t, []AuthorizationHeaderReference{
		{Reference: "current-user-authorization-1", Value: bearer},
		{Reference: "current-user-authorization-2", Value: basic},
		{Reference: "current-user-authorization-3", Value: custom},
	}, AuthorizationHeaderValues(input), "a duplicate exact value reuses the same request-local reference")
}

func TestAuthorizationHeaderValuesRejectsUnusableValues(t *testing.T) {
	overlong := "Bearer " + strings.Repeat("x", maxAuthorizationHeaderBytes+1)
	assert.Empty(t, AuthorizationHeaderValues("Authorization: "+overlong))

	for _, input := range []string{
		"Authorization: [REDACTED]",
		"Authorization: x",
		"Authorization: Bearer xy",
		"Authorization: Bearer 密钥不可执行",
		"ordinary prose mentioning authorization",
		"https://example.test/check?authorization=Bearer-secret-0123456789",
		"X-Amz-Authorization=Signature-secret-0123456789",
	} {
		assert.Empty(t, AuthorizationHeaderValues(input), input)
	}
}

func TestAuthorizationHeaderValuesCapsDistinctValuesAtFour(t *testing.T) {
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, "Authorization: Bearer distinct-token-"+strings.Repeat(string(rune('a'+i)), 12))
	}
	refs := AuthorizationHeaderValues(strings.Join(lines, "\n"))
	require.Len(t, refs, maxAuthorizationHeaderValues)
	assert.Equal(t, "current-user-authorization-4", refs[3].Reference)
}

func TestAuthorizationHeaderValuesKeepsCompleteExtensibleHeaders(t *testing.T) {
	cases := []struct {
		name  string
		input string
		value string
	}{
		{
			name:  "raw CRLF digest",
			input: "Authorization: Digest username=\"Mufasa\", realm=\"test\", nonce=\"abc\", response=\"xyz\"\r\nnext",
			value: "Digest username=\"Mufasa\", realm=\"test\", nonce=\"abc\", response=\"xyz\"",
		},
		{
			name:  "curl attached short option",
			input: `curl -H'Authorization: Signature keyId="key-1",algorithm="hmac-sha256",signature="abc"' /v1`,
			value: `Signature keyId="key-1",algorithm="hmac-sha256",signature="abc"`,
		},
		{
			name:  "curl long option assignment",
			input: "curl --header='Authorization: " + "Bear" + "er opaque-token-0123456789' /health",
			value: "Bear" + "er opaque-token-0123456789",
		},
		{
			name:  "AWS4 multi parameter",
			input: "Authorization: AWS4-HMAC-SHA256 Creden" + "tial=AKID/20260826/cn/service/aws4_request, SignedHeaders=host;x-date, Signature=abcdef0123456789",
			value: "AWS4-HMAC-SHA256 Creden" + "tial=AKID/20260826/cn/service/aws4_request, SignedHeaders=host;x-date, Signature=abcdef0123456789",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, []AuthorizationHeaderReference{{
				Reference: "current-user-authorization-1", Value: tc.value,
			}}, AuthorizationHeaderValues(tc.input))
		})
	}
}

func TestAuthorizationHeaderValuesNeverMintsFromAssignmentsOrURLQueries(t *testing.T) {
	const assigned = "Bear" + "er assignment-token-0123456789"
	for _, input := range []string{
		"Authorization=assignment-secret-0123456789",
		`curl --data-urlencode 'Authorization=body-secret-0123456789' /submit`,
		"curl -H 'Authorization: " + "Bear" + "er unterminated-secret-0123456789",
		"Authorization=" + assigned + " 后续中文仍保留",
		"设置Authorization=" + assigned + " 后续中文仍保留",
		"请用 **Authorization=" + assigned + "** 验证",
		"https://example.test/check?Authorization=signed-url-0123456789&x=1",
		"https://example.test/check?X-Amz-Authorization=signature-query-0123456789",
		"https://example.test/check?Authorization=Bearer%20encoded-secret-0123456789&x=1",
	} {
		assert.Empty(t, AuthorizationHeaderValues(input), input)
	}
}

func TestAuthorizationHeaderValuesIgnoresInlineProseWithoutABoundary(t *testing.T) {
	for _, input := range []string{
		"请使用Authorization: Bearer secret-token-012345进行验证",
		"请用 **Authorization: Bearer bold-token-012345** 验证",
	} {
		assert.Empty(t, AuthorizationHeaderValues(input), input)
	}
}

func TestAuthorizationHeaderValuesBoundsMultiParameterAndShellForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"AWS4 before prose", "AWS4-HMAC-SHA256 Creden" + "tial=AKID/scope, SignedHeaders=host;x-date, Signature=abcdef0123456789"},
		{"Digest before prose", `Digest username="Mufasa", realm="test", nonce="abc", response="xyz012345"`},
		{"Basic padding before prose", "Basic YWJjZA=="},
		{"Bearer before prose", "Bear" + "er prose-boundary-token-0123456789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "排查接口；Authorization: " + tc.value + " 请继续排查实例 uhost-1"
			require.Equal(t, []AuthorizationHeaderReference{{
				Reference: "current-user-authorization-1", Value: tc.value,
			}}, AuthorizationHeaderValues(input), "the value must end before the user's following prose")
		})
	}

	for _, tc := range []struct {
		input string
		value string
	}{
		{`curl -HAuthorization:Bearer-attached-token-0123456789 https://example.test/health`, "Bearer-attached-token-0123456789"},
		{`curl --header=Authorization:Basic-attached-token-0123456789 https://example.test/health`, "Basic-attached-token-0123456789"},
		{"请用 `Authorization: " + "Bear" + "er markdown-token-0123456789` 验证", "Bear" + "er markdown-token-0123456789"},
		{"请用 **Authorization**: " + "Bear" + "er bold-key-token-0123456789 验证", "Bear" + "er bold-key-token-0123456789"},
		{"请用 **Authorization:** " + "Bear" + "er bold-colon-token-0123456789 验证", "Bear" + "er bold-colon-token-0123456789"},
		{"下载地址 https://example.test/check?Authorization=signed-url-0123456789\nAuthorization: " + "Bear" + "er after-url-token-0123456789", "Bear" + "er after-url-token-0123456789"},
	} {
		require.Equal(t, []AuthorizationHeaderReference{{
			Reference: "current-user-authorization-1", Value: tc.value,
		}}, AuthorizationHeaderValues(tc.input), tc.input)
	}
}

func TestIsCredentialKeyMatchesNamesNotPaginationCursors(t *testing.T) {
	for _, key := range []string{"Password", "login_password", "AccessKey", "SecretKey", "api-key", "JupyterLabToken", "SshLoginCommand", "Authorization", "ClientSecret"} {
		assert.Truef(t, IsCredentialKey(key), "%s names a credential", key)
	}
	for _, key := range []string{"next_token", "UHostId", "Name", "PublicIP", "authorized_zones", "TokenCount"} {
		assert.Falsef(t, IsCredentialKey(key), "%s does not name a credential", key)
	}
}
