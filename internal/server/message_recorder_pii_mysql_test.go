//go:build mysql_integration

// Build tag isolates this test from the default `go test ./...` path so
// CI doesn't require a live MySQL. Run manually with:
//
//   go test -tags=mysql_integration ./internal/server/... \
//     -run TestMessageRecorder_MySQLPersistedRowIsRedacted_BothSides -count=1 -v
//
// Default DSN points at the local Docker MySQL used during PR #1
// verification; override via TEST_MYSQL_DSN if you start the container
// on a different host or credentials.

package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const defaultTestDSN = "root:devonly@tcp(127.0.0.1:3307)/compshare_agent?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai"

// TestMessageRecorder_MySQLPersistedRowIsRedacted_BothSides is the live-MySQL
// integration test pinning the Guardrails A contract end-to-end: when
// Record fires a PII-laden user message, the row that lands in
// agent_messages.user_message has phone/ID/email/bank-card placeholders
// in place of the raw tokens, while routing-relevant tokens (instance
// ID, GPU model, zone) survive unchanged.
//
// Encodes WHY: unit-test coverage at the in-memory queue boundary
// proves the redaction call is wired, but does not prove that nothing
// between Record and the INSERT statement could re-introduce raw
// values (e.g. a future logging interceptor adding raw entry.Original
// or a request-id mismatch overwriting the column). End-to-end SELECT
// of the persisted row closes that gap.
func TestMessageRecorder_MySQLPersistedRowIsRedacted_BothSides(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	r, err := NewMessageRecorder(dsn, MessageRecorderOptions{})
	if err != nil {
		t.Fatalf("NewMessageRecorder: %v (set TEST_MYSQL_DSN to override)", err)
	}

	requestUUID := fmt.Sprintf("pii-it-%d", time.Now().UnixNano())
	rawMsg := "我叫张三,手机 13800138000,邮箱 user@example.com,身份证 110101199003078888,卡号 4532015112830366,实例 uhost-abc123 在 cn-wlcb-01 跑 4090"
	rawAssistantMsg := `您的实例 uhost-abc123 已启动:
公网 IP: 1.2.3.4
区域: cn-wlcb-01
GPU: 4090 (24GB)
项目 ID: 12345678-1234-1234-1234-1234567890ab
AccessKey="AKIAIOSFODNN7EXAMPLE"
Jupyter token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTYifQ.signaturevalue123abc`
	entry := MessageEntry{
		RequestUUID:      requestUUID,
		TopOrgID:         9001,
		OrgID:            9101,
		ConnectionID:     "pii-it-conn",
		CreatedAt:        time.Now(),
		UserMessage:      rawMsg,
		AssistantMessage: rawAssistantMsg,
		Status:           "success",
		Model:            "deepseek-v4-flash",
		LatencyMS:        100,
	}
	if err := r.Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(closeCtx); err != nil {
		t.Fatalf("Close (drain): %v", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var persistedUser, persistedAssistant string
	err = db.QueryRow(
		"SELECT user_message, assistant_message FROM agent_messages WHERE request_uuid = ?",
		requestUUID,
	).Scan(&persistedUser, &persistedAssistant)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM agent_messages WHERE request_uuid = ?", requestUUID)
	})

	t.Logf("persisted user_message: %s", persistedUser)
	t.Logf("persisted assistant_message: %s", persistedAssistant)

	// Guardrails A: user_message PII redaction.
	userMustHave := []string{
		"[已脱敏:手机号]",
		"[已脱敏:邮箱]",
		"[已脱敏:身份证]",
		"[已脱敏:银行卡]",
		"uhost-abc123",
		"cn-wlcb-01",
		"4090",
	}
	for _, needle := range userMustHave {
		if !strings.Contains(persistedUser, needle) {
			t.Errorf("persisted user_message missing %q: %q", needle, persistedUser)
		}
	}
	userMustNotHave := []string{
		"13800138000",
		"user@example.com",
		"110101199003078888",
		"4532015112830366",
	}
	for _, needle := range userMustNotHave {
		if strings.Contains(persistedUser, needle) {
			t.Errorf("raw PII %q leaked into persisted user_message: %q", needle, persistedUser)
		}
	}

	// Guardrails B: assistant_message output leak redaction.
	assistantMustHave := []string{
		"[已脱敏:IP]",
		"[已脱敏:项目ID]",
		"[已脱敏:凭据]",
		"[已脱敏:令牌]",
		"uhost-abc123",
		"cn-wlcb-01",
		"4090",
		"24GB",
	}
	for _, needle := range assistantMustHave {
		if !strings.Contains(persistedAssistant, needle) {
			t.Errorf("persisted assistant_message missing %q: %q", needle, persistedAssistant)
		}
	}
	assistantMustNotHave := []string{
		"1.2.3.4",
		"12345678-1234-1234-1234-1234567890ab",
		"AKIAIOSFODNN7EXAMPLE",
		"signaturevalue123abc",
	}
	for _, needle := range assistantMustNotHave {
		if strings.Contains(persistedAssistant, needle) {
			t.Errorf("output leak %q escaped redaction: %q", needle, persistedAssistant)
		}
	}
}
