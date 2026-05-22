package guardrails

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactOutputLeak_IPv4(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare public IP", "1.2.3.4", IPRedacted},
		{"private IP", "192.168.1.1", IPRedacted},
		{"loopback", "127.0.0.1", IPRedacted},
		{"嵌中文", "实例 IP 是 1.2.3.4 请连接", "实例 IP 是 " + IPRedacted + " 请连接"},
		{"两个 IP", "from 1.2.3.4 to 5.6.7.8", "from " + IPRedacted + " to " + IPRedacted},
		{"非法 octet >255 跳过", "300.0.0.1", "300.0.0.1"},
		{"leading zero 跳过", "01.02.03.04", "01.02.03.04"},
		{"非 IPv4 数字串", "version 1.2.3.4-rc1 build", "version " + IPRedacted + "-rc1 build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactOutputLeak(tc.in))
		})
	}
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

	// Sensitive values gone.
	for _, leak := range []string{
		"1.2.3.4", "192.168.1.10",
		"12345678-1234-1234-1234-1234567890ab",
		"AKIAIOSFODNN7EXAMPLE",
		"signaturevalue123abc",
	} {
		assert.NotContains(t, got, leak, "leak %q escaped redaction", leak)
	}

	// Placeholders present.
	for _, want := range []string{IPRedacted, ProjectIDRedacted, CredentialRedactedOutput, TokenRedactedOutput} {
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
