package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditTaskTextRedactsCredentialButKeepsRepairFacts(t *testing.T) {
	secret := "sshops-e2e-" + "secret-1234567890"
	const facts = "检查实例 cpod-test 的 /v1/models，联系 user@example.com 13800138000，项目 12345678-1234-1234-1234-1234567890ab"
	task := facts + "；Authorization: " + "Bearer " + secret + "，期望 HTTP 200"
	got := auditTaskText(task)

	require.Contains(t, got, facts)
	require.Contains(t, got, "HTTP 200")
	require.Contains(t, got, "Authorization: Bearer")
	require.NotContains(t, got, secret)
}

func TestAuditTaskTextRedactsAPIKeyAssignmentBeforeTruncation(t *testing.T) {
	secret := "sk-audit-" + "secret-abcdefghijklmnopqrstuvwxyz"
	got := auditTaskText("api_key=" + secret + " " + strings.Repeat("诊断", 3000))

	require.NotContains(t, got, secret)
	require.LessOrEqual(t, len([]rune(got)), 4000)
}

func TestAuditTaskTextRedactsNaturalLanguageBearerValue(t *testing.T) {
	secret := "sshops-auth-canary-" + "20260826"
	got := auditTaskText("使用用户给出的 Authorization 值 Bearer " + secret + " 检查接口")

	require.NotContains(t, got, secret)
	require.Contains(t, got, "Bearer")
}
