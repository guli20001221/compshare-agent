package guardrails

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
		"deepseek-v4-flash",
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
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"Bearer prefix", "Authorization: Bearer AKIAIOSFODNN7EXAMPLEbCDEF", "Authorization: Bearer " + TokenRedactedOutput},
		{"token whitespace", "token abc123def456ghi789jklmno", "token " + TokenRedactedOutput},
		// Config-style separators ("=" / ":") — common in env files,
		// YAML, log lines. Codex review (PR #150) caught that the
		// previous \s+ regex silently passed token=AKIA... through.
		{"token equals", "token=AKIAIOSFODNN7EXAMPLEbCDEF", "token " + TokenRedactedOutput},
		{"token colon", "token: AKIAIOSFODNN7EXAMPLEbCDEF", "token " + TokenRedactedOutput},
		{"bearer equals", "bearer=AKIAIOSFODNN7EXAMPLEbCDEF", "bearer " + TokenRedactedOutput},
		{"bearer colon", "bearer:AKIAIOSFODNN7EXAMPLEbCDEF", "bearer " + TokenRedactedOutput},
		{"mixed separator", "Authorization: Bearer=AKIAIOSFODNN7EXAMPLEbCDEF", "Authorization: Bearer " + TokenRedactedOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactOutputLeak(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRedactOutputLeak_Composite checks the realistic case of multiple
// leak types in one assistant reply.
func TestRedactOutputLeak_Composite(t *testing.T) {
	in := `您的实例 uhost-abc123 已启动:
公网 IP: 1.2.3.4
内网 IP: 192.168.1.10
区域: cn-wlcb-01
GPU: 4090 (24GB)
项目 ID: 12345678-1234-1234-1234-1234567890ab
AccessKey="AKIAIOSFODNN7EXAMPLE"
Jupyter token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTYifQ.signaturevalue123abc`

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

func TestRedactCredentials_CoversDurableEnvelopeFormatsWithoutRemovingContext(t *testing.T) {
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
