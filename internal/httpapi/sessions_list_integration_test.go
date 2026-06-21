package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/store"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const httpITSessionsDDL = `
CREATE TABLE IF NOT EXISTS sessions (
  id                   CHAR(36)     NOT NULL PRIMARY KEY,
  top_organization_id  INT UNSIGNED NOT NULL,
  organization_id      INT UNSIGNED NOT NULL,
  title                VARCHAR(255) NULL,
  context              JSON         NULL,
  context_version      INT          NOT NULL DEFAULT 0,
  message_count        INT          NOT NULL DEFAULT 0,
  pinned               TINYINT(1)   NOT NULL DEFAULT 0,
  created_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at           DATETIME(3)  NULL,
  KEY idx_owner_updated (top_organization_id, organization_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

func openHTTPTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPSHARE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COMPSHARE_TEST_MYSQL_DSN not set — skipping HTTP real-DB integration test")
	}
	if strings.Contains(dsn, "117.50.121.245") {
		t.Fatal("refusing to run the integration test against the production TiDB host")
	}
	parsed, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	parsed.ParseTime = true
	parsed.Loc = time.UTC
	db, err := sql.Open("mysql", parsed.FormatDSN())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	return db
}

// TestListSessionsHTTP_Integration drives the real HTTP path end to end against a
// live database: an actual CreateCSAgentSession gateway request persists a row,
// a ListCSAgentSessions gateway request reads it back through the production
// handler + store SQL, and a raw SELECT confirms the row truly landed in MySQL.
// This is the "does it really persist over HTTP" proof. Gated on
// COMPSHARE_TEST_MYSQL_DSN; skipped in the normal suite.
//
//	COMPSHARE_TEST_MYSQL_DSN='root:pw@tcp(127.0.0.1:13306)/compshare_test' \
//	  go test ./internal/httpapi -run TestListSessionsHTTP_Integration -count=1 -v
func TestListSessionsHTTP_Integration(t *testing.T) {
	db := openHTTPTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, err := db.ExecContext(ctx, httpITSessionsDDL)
	require.NoError(t, err, "create sessions table")

	const topOrg, org = 4294967100, 4294967101
	cleanup := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM sessions WHERE top_organization_id = ?", topOrg)
	}
	cleanup()
	t.Cleanup(cleanup)

	// Real stores wired to the real DB — no mocks.
	h := NewHandlers(
		&config.Config{},
		store.NewSessionStore(db),
		store.NewMessageStore(db),
		store.NewFeedbackStore(db),
		nil,
		nil,
	)

	// 1) CreateCSAgentSession over HTTP persists a row.
	recCreate := performGateway(h, `{"Action":"CreateCSAgentSession","Title":"HTTP 真实落库测试","top_organization_id":4294967100,"organization_id":4294967101}`)
	require.Equal(t, 200, recCreate.Code, "CreateCSAgentSession HTTP status")
	var created map[string]any
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &created))
	require.EqualValues(t, 0, created["RetCode"])
	sessionID, _ := created["SessionId"].(string)
	require.NotEmpty(t, sessionID, "CreateCSAgentSession must return a SessionId")

	// 2) Raw SELECT proves the row actually landed in MySQL (independent of the API).
	var dbTitle string
	var dbTopOrg, dbOrg uint32
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT title, top_organization_id, organization_id FROM sessions WHERE id = ?", sessionID).
		Scan(&dbTitle, &dbTopOrg, &dbOrg))
	assert.Equal(t, "HTTP 真实落库测试", dbTitle, "title must be persisted in the DB")
	assert.EqualValues(t, topOrg, dbTopOrg)
	assert.EqualValues(t, org, dbOrg)

	// 3) ListCSAgentSessions over HTTP reads it back through the real handler+SQL.
	recList := performGateway(h, `{"Action":"ListCSAgentSessions","top_organization_id":4294967100,"organization_id":4294967101}`)
	require.Equal(t, 200, recList.Code, "ListCSAgentSessions HTTP status")
	body := recList.Body.String()
	assert.Contains(t, body, `"RetCode":0`)
	assert.Contains(t, body, sessionID, "the created session must appear in the list")
	assert.Contains(t, body, "HTTP 真实落库测试", "the persisted title must surface in the list")

	// 4) A different owner must NOT see this session (cross-tenant isolation over HTTP).
	recOther := performGateway(h, `{"Action":"ListCSAgentSessions","top_organization_id":4294967100,"organization_id":999999}`)
	require.Equal(t, 200, recOther.Code)
	assert.NotContains(t, recOther.Body.String(), sessionID,
		"another organization_id must not see this owner's session")
}
