package guardrails

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedactOutputLeak_IPv4IsPreserved is the inverse of the test it replaces.
// IPv4 was redacted here and is not any more: the addresses this product prints
// are the user's own instance endpoints, handed back to the user who owns them,
// so masking them removed information and prevented nothing. The two cases that
// made it concrete are the last two below — an SSH login line the user cannot
// act on, and a loopback-vs-wildcard bind that is the entire answer in a
// "service is up but unreachable" diagnosis.
func TestRedactOutputLeak_IPv4IsPreserved(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"bare public IP", "1.2.3.4"},
		{"private IP", "192.168.1.1"},
		{"loopback", "127.0.0.1"},
		{"嵌中文", "实例 IP 是 1.2.3.4 请连接"},
		{"两个 IP", "from 1.2.3.4 to 5.6.7.8"},
		{"曾被误伤的版本号", "version 1.2.3.4-rc1 build"},
		{"SSH 登录行必须可直接照抄", "ssh root@203.0.113.9 -p 23"},
		{"环回与通配必须可区分", "--ip=127.0.0.1 vs --ip=0.0.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.in, RedactOutputLeak(tc.in), "IPv4 must pass through untouched")
		})
	}
}

// TestRedactOutputLeak_StillRedactsCredentialsAlongsideIPs pins the half that did
// NOT change: dropping IPv4 masking must not become an excuse to let an AK/SK or
// token ride out next to the address it was printed with.
func TestRedactOutputLeak_StillRedactsCredentialsAlongsideIPs(t *testing.T) {
	got := RedactOutputLeak(`ssh root@203.0.113.9 -p 23 AccessKey="AKIAIOSFODNN7EXAMPLE"`)
	assert.Contains(t, got, "203.0.113.9", "the endpoint the user needs survives")
	assert.NotContains(t, got, "AKIAIOSFODNN7EXAMPLE", "the credential next to it does not")
}

// TestRedactOutputLeak_PreservesRouting pins the contract that
// answer-relevant tokens (GPU model, instance ID, zone code, prices)
// pass through. If any of these is incorrectly masked, ops reviewing
// persisted messages cannot follow the question/answer thread.
func TestRedactOutputLeak_PreservesRouting(t *testing.T) {
	preserved := []string{
		"4090",
		"5090",
		"A100",
		"H200",
		"4090D",
		"uhost-abc123",
		"uhostid-deadbeef",
		"cn-wlcb-01",
		"cn-shanghai-02",
		"¥1.69/小时",
		"¥3.13/小时",
		"24GB",
		"80GB",
		"gpt-5.6-terra",
		"qwen3-embedding-8b",
		"4090 多少钱一小时",
		"上海机房 A100 显存多大",
		"我的实例 uhost-abc123 在 cn-wlcb-01 跑 4090",
	}
	for _, s := range preserved {
		t.Run(s, func(t *testing.T) {
			assert.Equal(t, s, RedactOutputLeak(s),
				"answer-relevant token %q must survive output redaction", s)
		})
	}
}

func TestRedactOutputLeak_UUIDProjectID(t *testing.T) {
	uuid := "12345678-1234-1234-1234-1234567890ab"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare UUID", uuid, ProjectIDRedacted},
		{"中文夹带", "项目 ID 是 " + uuid + " 已设置", "项目 ID 是 " + ProjectIDRedacted + " 已设置"},
		{"大写", "12345678-ABCD-EF00-1234-1234567890AB", ProjectIDRedacted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactOutputLeak(tc.in))
		})
	}
}

func TestRedactOutputLeak_CredentialMarker(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustHave string // placeholder must appear
		mustGone string // credential body must be removed
	}{
		{"AccessKey=...", `AccessKey="AKIAIOSFODNN7EXAMPLE"`, CredentialRedactedOutput, "AKIAIOSFODNN7EXAMPLE"},
		{"access_key: ...", `access_key: AKIAIOSFODNN7EXAMPLE`, CredentialRedactedOutput, "AKIAIOSFODNN7EXAMPLE"},
		{"secret_key=", `secret_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`, CredentialRedactedOutput, "wJalrXUtnFEMI"},
		{"access_token=", `access_token=eyJhbGciOiJIUzI1NiJ9.body.sig`, CredentialRedactedOutput, "eyJhbGciOiJIUzI1NiJ9"},
		{"ak:", `ak: AKIAEXAMPLEABCDEFG`, CredentialRedactedOutput, "AKIAEXAMPLEABCDEFG"},
		{"sk=", `sk=wJalrXUtnFEMIK7MDENGbPxRf`, CredentialRedactedOutput, "wJalrXUtnFEMIK7MDENGbPxRf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactOutputLeak(tc.in)
			assert.Contains(t, got, tc.mustHave,
				"input %q → expected placeholder, got %q", tc.in, got)
			assert.NotContains(t, got, tc.mustGone,
				"input %q → credential body leaked: %q", tc.in, got)
		})
	}
}

// TestRedactOutputLeak_CredentialMarker_NoMatch verifies the
// marker-required pattern doesn't FP on prose mentioning "access" or
// "key" without an assignment.
func TestRedactOutputLeak_CredentialMarker_NoMatch(t *testing.T) {
	prose := []string{
		"请问 access 是什么意思",
		"使用密钥需要谨慎",
		"the access key concept is different from secret key",
		"如果 token 过期请重新登录",
		"按 key 排序",
	}
	for _, s := range prose {
		assert.Equal(t, s, RedactOutputLeak(s),
			"prose %q must not trigger credential redaction", s)
	}
}

func TestRedactOutputLeak_JWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTYifQ.signaturevalue123abc"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", jwt, TokenRedactedOutput},
		{"中文夹带", "您的 Jupyter token 是 " + jwt + "请妥善保管", "您的 Jupyter token 是 " + TokenRedactedOutput + "请妥善保管"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactOutputLeak(tc.in))
		})
	}
}

func TestRedactOutputLeak_BearerToken(t *testing.T) {
	testToken := "AKIAIOSFODN" + "N7EXAMPLEbCDEF"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"Bearer prefix", "Authorization: Bearer " + testToken, "Authorization: Bearer " + TokenRedactedOutput},
		{"token whitespace", "token abc123def456ghi789jklmno", "token " + TokenRedactedOutput},
		// Config-style separators are common in env files, YAML and logs.
		{"token equals", "token=" + testToken, "token " + TokenRedactedOutput},
		{"token colon", "token: " + testToken, "token " + TokenRedactedOutput},
		{"bearer equals", "bearer=" + testToken, "bearer " + TokenRedactedOutput},
		{"bearer colon", "bearer:" + testToken, "bearer " + TokenRedactedOutput},
		{"mixed separator", "Authorization: Bearer=" + testToken, "Authorization: Bearer " + TokenRedactedOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactOutputLeak(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReferenceAuthorizationHeaderValuesUsesTheRedactionParser(t *testing.T) {
	const (
		bearer = "Bear" + "er auth-canary-0123456789"
		basic  = "Basic YWxhZGRpbjpvcGVuc2VzYW1l"
		custom = "Signature custom-token-0123456789"
	)
	input := "curl -H 'Authorization: " + bearer + "' /health\n" +
		`{"Authorization": "` + basic + `"}` + "\n" +
		"Authorization：" + custom + "，请验证\n" +
		"Authorization: " + bearer
	safe, refs := ReferenceAuthorizationHeaderValues(input)
	require.Equal(t, []AuthorizationHeaderReference{
		{Reference: "current-user-authorization-1", Value: bearer},
		{Reference: "current-user-authorization-2", Value: basic},
		{Reference: "current-user-authorization-3", Value: custom},
	}, refs)
	for _, secret := range []string{bearer, basic, custom} {
		assert.NotContains(t, safe, secret)
		assert.NotContains(t, RedactCredentials(input), secret,
			"anything extractable for the private channel must also be persistence-redactable")
	}
	assert.Equal(t, 2, strings.Count(safe, "[AUTHORIZATION_REF:current-user-authorization-1]"),
		"a duplicate exact value reuses the same request-local reference")
	assert.Contains(t, safe, "[AUTHORIZATION_REF:current-user-authorization-2]")
	assert.Contains(t, safe, "[AUTHORIZATION_REF:current-user-authorization-3]")
}

func TestReferenceAuthorizationHeaderValuesRejectsUnusableValues(t *testing.T) {
	overlong := "Bearer " + strings.Repeat("x", maxAuthorizationHeaderBytes+1)
	safe, refs := ReferenceAuthorizationHeaderValues("Authorization: " + overlong)
	assert.Empty(t, refs)
	assert.NotContains(t, safe, overlong, "an unusable header must still be removed from live model text")
	assert.Contains(t, safe, "[AUTHORIZATION_REDACTED]")

	for _, input := range []string{
		"Authorization: [REDACTED]",
		"Authorization: x",
		"Authorization: Bearer xy",
		"ordinary prose mentioning authorization",
		"https://example.test/check?authorization=Bearer-secret-0123456789",
		"X-Amz-Authorization=Signature-secret-0123456789",
	} {
		_, refs = ReferenceAuthorizationHeaderValues(input)
		assert.Empty(t, refs, input)
	}
}

func TestReferenceAuthorizationHeaderValuesKeepsCompleteExtensibleHeaders(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		value     string
		forbidden string
	}{
		{
			name:      "raw CRLF digest",
			input:     "Authorization: Digest username=\"Mufasa\", realm=\"test\", nonce=\"abc\", response=\"xyz\"\r\nnext",
			value:     "Digest username=\"Mufasa\", realm=\"test\", nonce=\"abc\", response=\"xyz\"",
			forbidden: `response="xyz"`,
		},
		{
			name:      "curl attached short option",
			input:     `curl -H'Authorization: Signature keyId="key-1",algorithm="hmac-sha256",signature="abc"' /v1`,
			value:     `Signature keyId="key-1",algorithm="hmac-sha256",signature="abc"`,
			forbidden: `signature="abc"`,
		},
		{
			name:      "curl long option assignment",
			input:     "curl --header='Authorization: " + "Bear" + "er opaque-token-0123456789' /health",
			value:     "Bear" + "er opaque-token-0123456789",
			forbidden: "opaque-token-0123456789",
		},
		{
			name:      "AWS4 multi parameter",
			input:     "Authorization: AWS4-HMAC-SHA256 Creden" + "tial=AKID/20260826/cn/service/aws4_request, SignedHeaders=host;x-date, Signature=abcdef0123456789",
			value:     "AWS4-HMAC-SHA256 Creden" + "tial=AKID/20260826/cn/service/aws4_request, SignedHeaders=host;x-date, Signature=abcdef0123456789",
			forbidden: "Signature=abcdef0123456789",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			safe, refs := ReferenceAuthorizationHeaderValues(tc.input)
			require.Equal(t, []AuthorizationHeaderReference{{
				Reference: "current-user-authorization-1", Value: tc.value,
			}}, refs)
			assert.NotContains(t, safe, tc.value)
			assert.NotContains(t, safe, tc.forbidden)
			assert.Contains(t, safe, "[AUTHORIZATION_REF:current-user-authorization-1]")
			persisted := RedactCredentials(tc.input)
			assert.NotContains(t, persisted, tc.value)
			assert.NotContains(t, persisted, tc.forbidden)
		})
	}
}

func TestReferenceAuthorizationHeaderValuesSeparatesAssignmentsFromHeaders(t *testing.T) {
	for _, input := range []string{
		"Authorization=assignment-secret-0123456789",
		`curl --data-urlencode 'Authorization=body-secret-0123456789' /submit`,
		"Authorization: Bearer 密钥不可执行",
		"curl -H 'Authorization: " + "Bear" + "er unterminated-secret-0123456789",
	} {
		safe, refs := ReferenceAuthorizationHeaderValues(input)
		assert.Empty(t, refs, input)
		assert.NotEqual(t, input, safe, "credential-shaped non-capabilities must still leave the live model redacted")
		assert.NotContains(t, safe, "secret-0123456789")
		assert.NotEqual(t, input, RedactCredentials(input), "the durable boundary must redact the same syntax")
	}

	for _, input := range []string{
		"https://example.test/check?Authorization=signed-url-0123456789&x=1",
		"https://example.test/check?X-Amz-Authorization=signature-query-0123456789",
		"https://example.test/check?Authorization=Bearer%20encoded-secret-0123456789&x=1",
	} {
		safe, refs := ReferenceAuthorizationHeaderValues(input)
		assert.Empty(t, refs, input)
		assert.Equal(t, input, safe, "signed URL query parameters keep the established live flow")
		assert.NotEqual(t, input, RedactCredentials(input), "durable storage must still redact URL credentials")
		assert.NotContains(t, RedactCredentials(input), "encoded-secret-0123456789")
	}
}

func TestReferenceAuthorizationHeaderValuesRedactsCompleteBoundaryAssignment(t *testing.T) {
	const secret = "assignment-token-0123456789"
	for _, tc := range []struct {
		input  string
		suffix string
	}{
		{"Authorization=Bearer " + secret + " 后续中文仍保留", "后续中文仍保留"},
		{"设置Authorization=Bearer " + secret + " 后续中文仍保留", "后续中文仍保留"},
		{"请用 **Authorization=Bearer " + secret + "** 验证", "** 验证"},
	} {
		safe, refs := ReferenceAuthorizationHeaderValues(tc.input)
		assert.Empty(t, refs, "an assignment must never mint an HTTP-header capability")
		assert.NotContains(t, safe, secret)
		assert.Contains(t, safe, tc.suffix)

		persisted := RedactCredentials(tc.input)
		assert.NotContains(t, persisted, secret)
		assert.Contains(t, persisted, tc.suffix)
	}
}

func TestReferenceAuthorizationHeaderValuesDoesNotConsumeFollowingProse(t *testing.T) {
	const value = "Bear" + "er prose-boundary-token-0123456789"
	input := "排查接口；Authorization: " + value + " 请继续排查实例 uhost-1"
	safe, refs := ReferenceAuthorizationHeaderValues(input)
	require.Equal(t, []AuthorizationHeaderReference{{
		Reference: "current-user-authorization-1", Value: value,
	}}, refs)
	assert.Contains(t, safe, "请继续排查实例 uhost-1")
	persisted := RedactCredentials(input)
	assert.Contains(t, persisted, "请继续排查实例 uhost-1")
	assert.NotContains(t, persisted, "prose-boundary-token")
}

func TestReferenceAuthorizationHeaderValuesRedactsNonExecutableInlineHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		secret string
		suffix string
	}{
		{
			name:   "Chinese prose without a boundary",
			input:  "请使用Authorization: Bearer secret-token-012345进行验证",
			secret: "secret-token-012345",
			suffix: "进行验证",
		},
		{
			name:   "Markdown bold field",
			input:  "请用 **Authorization: Bearer bold-token-012345** 验证",
			secret: "bold-token-012345",
			suffix: "** 验证",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			safe, refs := ReferenceAuthorizationHeaderValues(tc.input)
			assert.Empty(t, refs, "inline prose is not an executable header capability")
			assert.NotContains(t, safe, tc.secret)
			assert.Contains(t, safe, tc.suffix)
			persisted := RedactCredentials(tc.input)
			assert.NotContains(t, persisted, tc.secret)
			assert.Contains(t, persisted, tc.suffix)
		})
	}
}

func TestReferenceAuthorizationHeaderValuesBoundsMultiParameterAndShellForms(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		value  string
		suffix string
	}{
		{
			name:   "AWS4 before prose",
			value:  "AWS4-HMAC-SHA256 Creden" + "tial=AKID/scope, SignedHeaders=host;x-date, Signature=abcdef0123456789",
			suffix: "请验证实例 uhost-1",
		},
		{
			name:   "Digest before prose",
			value:  `Digest username="Mufasa", realm="test", nonce="abc", response="xyz012345"`,
			suffix: "继续排查接口",
		},
		{
			name:   "Basic padding before prose",
			value:  "Basic YWJjZA==",
			suffix: "继续验证",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := "Authorization: " + tc.value + " " + tc.suffix
			safe, refs := ReferenceAuthorizationHeaderValues(input)
			require.Equal(t, []AuthorizationHeaderReference{{
				Reference: "current-user-authorization-1", Value: tc.value,
			}}, refs)
			assert.Contains(t, safe, tc.suffix)
			assert.Contains(t, RedactCredentials(input), tc.suffix)
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
	} {
		safe, refs := ReferenceAuthorizationHeaderValues(tc.input)
		require.Equal(t, []AuthorizationHeaderReference{{
			Reference: "current-user-authorization-1", Value: tc.value,
		}}, refs)
		assert.NotContains(t, safe, tc.value)
	}
}

// TestRedactOutputLeak_Composite checks the realistic case of multiple
// leak types in one assistant reply.
func TestRedactOutputLeak_Composite(t *testing.T) {
	accessKey := "AKIAIOSFOD" + "NN7EXAMPLE"
	jupyterToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiIxMjM0NTYifQ.signaturevalue123abc"
	in := `您的实例 uhost-abc123 已启动:
公网 IP: 1.2.3.4
内网 IP: 192.168.1.10
区域: cn-wlcb-01
GPU: 4090 (24GB)
项目 ID: 12345678-1234-1234-1234-1234567890ab
AccessKey="` + accessKey + `"
Jupyter token: ` + jupyterToken

	got := RedactOutputLeak(in)
	t.Logf("redacted output: %s", got)

	// Routing tokens preserved.
	for _, must := range []string{"uhost-abc123", "cn-wlcb-01", "4090", "24GB"} {
		assert.Contains(t, got, must, "routing/spec %q must survive", must)
	}

	// Endpoints the user needs in order to act survive alongside them.
	for _, must := range []string{"1.2.3.4", "192.168.1.10"} {
		assert.Contains(t, got, must, "instance endpoint %q must survive", must)
	}

	// Sensitive values gone.
	for _, leak := range []string{
		"12345678-1234-1234-1234-1234567890ab",
		"AKIAIOSFODNN7EXAMPLE",
		"signaturevalue123abc",
	} {
		assert.NotContains(t, got, leak, "leak %q escaped redaction", leak)
	}

	// Placeholders present.
	for _, want := range []string{ProjectIDRedacted, CredentialRedactedOutput, TokenRedactedOutput} {
		assert.Contains(t, got, want, "placeholder %q missing", want)
	}
}

func TestRedactOutputLeak_Idempotent(t *testing.T) {
	in := "IP 1.2.3.4 项目 12345678-1234-1234-1234-1234567890ab AccessKey=ABCDEF1234567890"
	once := RedactOutputLeak(in)
	twice := RedactOutputLeak(once)
	assert.Equal(t, once, twice, "RedactOutputLeak must be idempotent")
}

func TestRedactOutputLeak_Empty(t *testing.T) {
	assert.Equal(t, "", RedactOutputLeak(""))
}

func TestRedactCredentials_CoversPersistedEnvelopeFormatsWithoutRemovingContext(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjb250ZXh0LXVzZXIifQ.signaturevalue123456"
	pem := "-----BEGIN PRIVATE KEY-----\nabc123secret\n-----END PRIVATE KEY-----"
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{"json access key", `{"access_key":"AKIAIOSFODNN7EXAMPLE"}`, "AKIAIOSFODNN7EXAMPLE"},
		{"yaml secret", "secret_key: wJalrXUtnFEMI/K7MDENGbPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI"},
		{"env api key", "API_KEY=" + "sk-envelope-" + "secret-123456", "sk-envelope-" + "secret-123456"},
		{"http bearer", "Authorization: Bearer " + "bearer-" + "secret-1234567890", "bearer-" + "secret-1234567890"},
		{"generic token", "token=token-secret-1234567890", "token-secret-1234567890"},
		{"security token", "SecurityToken: security-secret-123", "security-secret-123"},
		{"refresh token", "refresh_token=refresh-secret-123", "refresh-secret-123"},
		{"password", "password: " + "short-" + "password", "short-" + "password"},
		{"raw jwt", "凭据 " + jwt, jwt},
		{"pem", pem, "abc123secret"},
		{"known aws prefix", "AKIA1234567890ABCDEF", "AKIA1234567890ABCDEF"},
		{"known github prefix", "ghp_1234567890abcdefghijklmnop", "ghp_1234567890abcdefghijklmnop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCredentials(tc.input)
			assert.NotContains(t, got, tc.secret)
			assert.Contains(t, got, "[已脱敏:")
			assert.Equal(t, got, RedactCredentials(got), "credential redaction must be idempotent")
		})
	}

	semantic := "联系 operator@example.com，排查 10.0.0.8，项目 project-live，实例 uhost-1，地域 cn-bj2"
	assert.Equal(t, semantic, RedactCredentials(semantic))
}

func TestContainsCredentialUsesTheSameRulesWithoutFalseContextMatches(t *testing.T) {
	for _, input := range []string{
		"Authorization: " + "Bearer " + "bearer-" + "secret-1234567890",
		"pass" + "word: short-" + "password",
		"AKIA1234567890ABCDEF",
		"-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----",
	} {
		assert.Truef(t, ContainsCredential(input), "credential should be detected: %q", input)
	}
	for _, input := range []string{
		"实例 uhost-abc123 在 cn-wlcb-01",
		"project-live 的 4090 价格是多少",
		"联系 user@example.com，来源 10.0.0.8",
		"如果 token 过期请重新登录",
	} {
		assert.Falsef(t, ContainsCredential(input), "ordinary context must survive: %q", input)
	}
}
