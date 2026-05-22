# Console AI Assistant HTTP Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `compshare-agent server` HTTP service that implements phase-1 console AI assistant APIs (`GetMeta`, `CreateSession`, `GetSession`, `Chat` SSE, `Feedback`, `/healthz`) while preserving the existing CLI behavior.

**Architecture:** Keep the existing engine/LLM/tool core as shared code. Add a Gin HTTP layer, MySQL-backed session/message/feedback stores, a per-session engine LRU pool, and a per-token SSE path from `llm.Client` through `engine.ChatWithOptions` to HTTP. Request identity comes from gateway-injected request-body fields `top_organization_id`, `organization_id`, and optional `request_uuid`.

**Tech Stack:** Go 1.22, Cobra, Gin, `database/sql`, `github.com/go-sql-driver/mysql`, `github.com/google/uuid`, MySQL 8, SSE over `net/http`.

---

## File structure map

### Existing files to modify

- `go.mod`, `go.sum` — add Gin, MySQL driver, uuid dependencies.
- `cmd/agent.go` — keep `main()` and move CLI command setup/helpers out to focused files.
- `cmd/trace.go` — no functional changes expected; stays CLI/runtime feature wiring.
- `internal/config/config.go` — add `HTTPConfig`, `MySQLConfig`, `MetaConfig`; keep CLI compatibility by validating MySQL only for server startup, not in `Load()`.
- `deploy/conf/agent.yaml.example` — add `http`, `mysql`, `meta` sections.
- `internal/llm/client.go` — add optional `ChatRequest.OnTextDelta` callback and invoke it for streamed text deltas.
- `internal/llm/client_test.go` — add callback behavior tests.
- `internal/engine/engine.go` — add `HistoryMessage`, `ChatOptions`, `RehydrateHistory`, `ChatWithOptions`; make old `Chat` delegate.
- `internal/engine/engine_test.go` or new focused test file — add rehydration and streaming-option tests.
- `CLAUDE.md` — add HTTP-service guidance after implementation is in place.

### New files to create

- `cmd/root.go` — root cobra command and global `--config` flag.
- `cmd/cli.go` — existing CLI command and helper functions moved from `cmd/agent.go`.
- `cmd/server.go` — HTTP server cobra command, MySQL open, handler wiring, graceful shutdown.
- `.env.example` — safe tracked environment template.
- `deploy/migrations/0001_init.sql` — MySQL schema.
- `internal/store/types.go` — `Owner`, `Session`, `Message`, patches, interfaces.
- `internal/store/mysql.go` — `OpenMySQL`, connection tuning, schema verification.
- `internal/store/sessions.go` — MySQL session store.
- `internal/store/messages.go` — MySQL message store.
- `internal/store/feedback.go` — MySQL feedback store.
- `internal/store/cursor.go` — cursor encode/decode.
- `internal/store/*_test.go` — cursor tests and SQL-query tests with `sqlmock` if added; otherwise use thin unit tests around cursor and argument validation.
- `internal/agentpool/pool.go` — LRU pool.
- `internal/agentpool/rehydrate.go` — engine construction and history rehydration.
- `internal/agentpool/pool_test.go` — LRU/rehydrate tests with mock store.
- `internal/httpapi/types.go` — response/data request structs shared by handlers.
- `internal/httpapi/errors.go` — `APIError`, code/status mapping, error helpers.
- `internal/httpapi/baserequest.go` — parse JSON/form and build base request.
- `internal/httpapi/dispatch.go` — Action router and non-SSE response wrapping.
- `internal/httpapi/healthz.go` — `/healthz` handler.
- `internal/httpapi/sse/writer.go` — SSE writer.
- `internal/httpapi/handlers.go` — handler dependencies container.
- `internal/httpapi/handlers_meta.go` — `GetMeta`.
- `internal/httpapi/handlers_session.go` — `CreateSession`, `GetSession`.
- `internal/httpapi/handlers_feedback.go` — `Feedback`.
- `internal/httpapi/handlers_chat.go` — `Chat` SSE.
- `internal/httpapi/*_test.go`, `internal/httpapi/sse/writer_test.go` — handler and writer tests with mock stores/pool.

---

## Task 1: Add HTTP/MySQL dependencies and configuration structs

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `deploy/conf/agent.yaml.example`
- Create: `.env.example`

- [ ] **Step 1: Add dependencies**

Run:

```bash
go get github.com/gin-gonic/gin github.com/go-sql-driver/mysql github.com/google/uuid
```

Expected: `go.mod` contains the new direct dependencies and `go.sum` is updated.

- [ ] **Step 2: Write config tests first**

Append tests to `internal/config/config_test.go` covering the new sections and CLI-safe load behavior:

```go
func TestLoadHTTPMySQLMetaConfig(t *testing.T) {
    t.Setenv("COMPSHARE_PUBLIC_KEY", "pub")
    t.Setenv("COMPSHARE_PRIVATE_KEY", "priv")
    t.Setenv("LLM_API_KEY", "llm")
    t.Setenv("MYSQL_DSN", "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true&loc=Local&charset=utf8mb4")

    path := writeTempConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "${COMPSHARE_PUBLIC_KEY}"
  private_key: "${COMPSHARE_PRIVATE_KEY}"
  region: "cn-wlcb"
  project_id: ""
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    model: "deepseek-v4-flash"
  rate_limit:
    llm_qps: 5
    llm_daily: 5000
    mutating_qps: 1
    mutating_daily: 50
    read_expensive_qps: 6
    read_expensive_daily: 500
  http:
    listen_addr: "127.0.0.1:18080"
    read_timeout: "30s"
    write_timeout: "0s"
    sse_keepalive_interval: "15s"
    max_input_length: 4000
    pool_capacity: 200
    pool_idle_ttl: "30m"
  mysql:
    dsn: "${MYSQL_DSN}"
    max_open_conns: 20
    max_idle_conns: 5
    conn_max_lifetime: "1h"
  meta:
    welcome: "我是优云算力共享平台 AI 助手，可以问我控制台相关问题。"
    suggested_prompts:
      - "我有哪些实例"
      - "4090 现在有库存吗"
    max_input_length: 4000
`)

    cfg, err := Load(path)
    require.NoError(t, err)
    assert.Equal(t, "127.0.0.1:18080", cfg.Agent.HTTP.ListenAddr)
    assert.Equal(t, 30*time.Second, cfg.Agent.HTTP.ReadTimeout)
    assert.Equal(t, time.Duration(0), cfg.Agent.HTTP.WriteTimeout)
    assert.Equal(t, 15*time.Second, cfg.Agent.HTTP.SSEKeepaliveInterval)
    assert.Equal(t, 4000, cfg.Agent.HTTP.MaxInputLength)
    assert.Equal(t, 200, cfg.Agent.HTTP.PoolCapacity)
    assert.Equal(t, 30*time.Minute, cfg.Agent.HTTP.PoolIdleTTL)
    assert.Equal(t, "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true&loc=Local&charset=utf8mb4", cfg.Agent.MySQL.DSN)
    assert.Equal(t, 20, cfg.Agent.MySQL.MaxOpenConns)
    assert.Equal(t, 5, cfg.Agent.MySQL.MaxIdleConns)
    assert.Equal(t, time.Hour, cfg.Agent.MySQL.ConnMaxLifetime)
    assert.Equal(t, "我是优云算力共享平台 AI 助手，可以问我控制台相关问题。", cfg.Agent.Meta.Welcome)
    assert.Equal(t, []string{"我有哪些实例", "4090 现在有库存吗"}, cfg.Agent.Meta.SuggestedPrompts)
}

func TestLoadAllowsMissingMySQLForCLICompatibility(t *testing.T) {
    t.Setenv("COMPSHARE_PUBLIC_KEY", "pub")
    t.Setenv("COMPSHARE_PRIVATE_KEY", "priv")
    t.Setenv("LLM_API_KEY", "llm")

    path := writeTempConfig(t, `
agent:
  executor: external
  compshare_api_url: "https://api.compshare.cn/"
  public_key: "${COMPSHARE_PUBLIC_KEY}"
  private_key: "${COMPSHARE_PRIVATE_KEY}"
  region: "cn-wlcb"
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    model: "deepseek-v4-flash"
`)

    cfg, err := Load(path)
    require.NoError(t, err)
    assert.Empty(t, cfg.Agent.MySQL.DSN)
}
```

If `writeTempConfig` does not exist in `config_test.go`, add it:

```go
func writeTempConfig(t *testing.T, content string) string {
    t.Helper()
    path := filepath.Join(t.TempDir(), "agent.yaml")
    require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
    return path
}
```

Add imports if missing:

```go
import (
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)
```

- [ ] **Step 3: Run tests and verify failure**

Run:

```bash
go test ./internal/config -run 'TestLoadHTTPMySQLMetaConfig|TestLoadAllowsMissingMySQLForCLICompatibility' -count=1
```

Expected: FAIL because `AgentConfig` has no `HTTP`, `MySQL`, or `Meta` fields.

- [ ] **Step 4: Implement config structs**

In `internal/config/config.go`, add fields to `AgentConfig`:

```go
type AgentConfig struct {
    LLM       LLMConfig       `yaml:"llm"`
    RateLimit RateLimitConfig `yaml:"rate_limit"`
    Executor  string          `yaml:"executor"`

    CompShareAPIURL string `yaml:"compshare_api_url"`
    PublicKey       string `yaml:"public_key"`
    PrivateKey      string `yaml:"private_key"`
    Region          string `yaml:"region"`
    ProjectId       string `yaml:"project_id"`

    HTTP  HTTPConfig  `yaml:"http"`
    MySQL MySQLConfig `yaml:"mysql"`
    Meta  MetaConfig  `yaml:"meta"`
}
```

Add new structs:

```go
type HTTPConfig struct {
    ListenAddr           string        `yaml:"listen_addr"`
    ReadTimeout          time.Duration `yaml:"read_timeout"`
    WriteTimeout         time.Duration `yaml:"write_timeout"`
    SSEKeepaliveInterval time.Duration `yaml:"sse_keepalive_interval"`
    MaxInputLength       int           `yaml:"max_input_length"`
    PoolCapacity         int           `yaml:"pool_capacity"`
    PoolIdleTTL          time.Duration `yaml:"pool_idle_ttl"`
}

type MySQLConfig struct {
    DSN             string        `yaml:"dsn"`
    MaxOpenConns    int           `yaml:"max_open_conns"`
    MaxIdleConns    int           `yaml:"max_idle_conns"`
    ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

type MetaConfig struct {
    Welcome          string   `yaml:"welcome"`
    SuggestedPrompts []string `yaml:"suggested_prompts"`
    MaxInputLength   int      `yaml:"max_input_length"`
}
```

Add `time` import to `config.go`.

Set safe defaults after unmarshalling so old CLI configs still load:

```go
applyDefaults(&cfg)
```

Add helper:

```go
func applyDefaults(cfg *Config) {
    if cfg.Agent.HTTP.ListenAddr == "" {
        cfg.Agent.HTTP.ListenAddr = "0.0.0.0:8080"
    }
    if cfg.Agent.HTTP.ReadTimeout == 0 {
        cfg.Agent.HTTP.ReadTimeout = 30 * time.Second
    }
    if cfg.Agent.HTTP.SSEKeepaliveInterval == 0 {
        cfg.Agent.HTTP.SSEKeepaliveInterval = 15 * time.Second
    }
    if cfg.Agent.HTTP.MaxInputLength == 0 {
        cfg.Agent.HTTP.MaxInputLength = 4000
    }
    if cfg.Agent.HTTP.PoolCapacity == 0 {
        cfg.Agent.HTTP.PoolCapacity = 200
    }
    if cfg.Agent.HTTP.PoolIdleTTL == 0 {
        cfg.Agent.HTTP.PoolIdleTTL = 30 * time.Minute
    }
    if cfg.Agent.MySQL.MaxOpenConns == 0 {
        cfg.Agent.MySQL.MaxOpenConns = 20
    }
    if cfg.Agent.MySQL.MaxIdleConns == 0 {
        cfg.Agent.MySQL.MaxIdleConns = 5
    }
    if cfg.Agent.MySQL.ConnMaxLifetime == 0 {
        cfg.Agent.MySQL.ConnMaxLifetime = time.Hour
    }
    if cfg.Agent.Meta.MaxInputLength == 0 {
        cfg.Agent.Meta.MaxInputLength = cfg.Agent.HTTP.MaxInputLength
    }
}
```

Do not make MySQL DSN required in `Load`; server startup will validate it.

- [ ] **Step 5: Update example config**

Append to `deploy/conf/agent.yaml.example` under `agent:`:

```yaml
  http:
    listen_addr: "0.0.0.0:8080"
    read_timeout: "30s"
    write_timeout: "0s"
    sse_keepalive_interval: "15s"
    max_input_length: 4000
    pool_capacity: 200
    pool_idle_ttl: "30m"

  mysql:
    dsn: "${MYSQL_DSN}"
    max_open_conns: 20
    max_idle_conns: 5
    conn_max_lifetime: "1h"

  meta:
    welcome: "我是优云算力共享平台 AI 助手，可以问我控制台相关问题。"
    suggested_prompts:
      - "我有哪些实例"
      - "4090 现在有库存吗"
      - "创建实例的操作步骤"
    max_input_length: 4000
```

- [ ] **Step 6: Create `.env.example`**

Create `.env.example`:

```dotenv
COMPSHARE_PUBLIC_KEY=
COMPSHARE_PRIVATE_KEY=
LLM_API_KEY=
MYSQL_DSN=user:pass@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Local&charset=utf8mb4
```

- [ ] **Step 7: Run tests**

Run:

```bash
go test ./internal/config -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum internal/config/config.go internal/config/config_test.go deploy/conf/agent.yaml.example .env.example
git commit -m "feat(config): add HTTP service settings"
```

---

## Task 2: Add MySQL schema and store layer

**Files:**
- Create: `deploy/migrations/0001_init.sql`
- Create: `internal/store/types.go`
- Create: `internal/store/cursor.go`
- Create: `internal/store/cursor_test.go`
- Create: `internal/store/mysql.go`
- Create: `internal/store/sessions.go`
- Create: `internal/store/messages.go`
- Create: `internal/store/feedback.go`

- [ ] **Step 1: Write cursor tests first**

Create `internal/store/cursor_test.go`:

```go
package store

import (
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestEncodeDecodeCursorRoundTrip(t *testing.T) {
    ts := time.Date(2026, 5, 21, 10, 23, 48, 123_000_000, time.UTC)

    cursor, err := EncodeCursor(ts, "msg-aaa")
    require.NoError(t, err)
    require.NotEmpty(t, cursor)

    gotTS, gotID, err := DecodeCursor(cursor)
    require.NoError(t, err)
    assert.True(t, gotTS.Equal(ts), gotTS)
    assert.Equal(t, "msg-aaa", gotID)
}

func TestDecodeCursorRejectsInvalidBase64(t *testing.T) {
    _, _, err := DecodeCursor("not base64")
    require.Error(t, err)
}

func TestDecodeCursorRejectsMissingFields(t *testing.T) {
    cursor, err := encodeCursorPayload(cursorPayload{ID: "msg-aaa"})
    require.NoError(t, err)

    _, _, err = DecodeCursor(cursor)
    require.Error(t, err)
}
```

- [ ] **Step 2: Run cursor tests and verify failure**

Run:

```bash
go test ./internal/store -run TestEncodeDecodeCursor -count=1
```

Expected: FAIL because `internal/store` and cursor functions do not exist.

- [ ] **Step 3: Implement schema file**

Create `deploy/migrations/0001_init.sql`:

```sql
CREATE TABLE sessions (
  id                   CHAR(36)     NOT NULL PRIMARY KEY,
  top_organization_id  INT UNSIGNED NOT NULL,
  organization_id      INT UNSIGNED NOT NULL,
  title                VARCHAR(255) NULL,
  context              JSON         NULL,
  message_count        INT          NOT NULL DEFAULT 0,
  pinned               TINYINT(1)   NOT NULL DEFAULT 0,
  created_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at           DATETIME(3)  NULL,
  KEY idx_owner_updated (top_organization_id, organization_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE messages (
  id            CHAR(36)     NOT NULL PRIMARY KEY,
  session_id    CHAR(36)     NOT NULL,
  request_uuid  VARCHAR(64)  NULL,
  role          VARCHAR(16)  NOT NULL,
  content       MEDIUMTEXT   NOT NULL,
  status        VARCHAR(16)  NOT NULL,
  error_code    VARCHAR(64)  NULL,
  model         VARCHAR(64)  NULL,
  input_tokens  INT          NULL,
  output_tokens INT          NULL,
  ttft_ms       INT          NULL,
  latency_ms    INT          NULL,
  metadata      JSON         NULL,
  created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_session_created (session_id, created_at),
  KEY idx_request_uuid    (request_uuid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE message_feedback (
  id          CHAR(36)    NOT NULL PRIMARY KEY,
  message_id  CHAR(36)    NOT NULL,
  rating      VARCHAR(8)  NOT NULL,
  comment     TEXT        NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_message (message_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [ ] **Step 4: Implement store types**

Create `internal/store/types.go`:

```go
package store

import (
    "context"
    "encoding/json"
    "time"
)

type Owner struct {
    TopOrganizationID uint32
    OrganizationID    uint32
}

type Session struct {
    ID                string
    TopOrganizationID uint32
    OrganizationID    uint32
    Title             *string
    Context           json.RawMessage
    MessageCount      int
    Pinned            bool
    CreatedAt         time.Time
    UpdatedAt         time.Time
}

type Message struct {
    ID           string
    SessionID    string
    RequestUUID  string
    Role         string
    Content      string
    Status       string
    ErrorCode    *string
    Model        *string
    InputTokens  *int
    OutputTokens *int
    TTFTMs       *int
    LatencyMs    *int
    Metadata     json.RawMessage
    CreatedAt    time.Time
}

type AssistantPatch struct {
    Content      string
    Status       string
    ErrorCode    *string
    InputTokens  *int
    OutputTokens *int
    TTFTMs       *int
    LatencyMs    *int
}

type SessionStore interface {
    Create(ctx context.Context, owner Owner, title *string, ctxJSON json.RawMessage) (Session, error)
    GetByID(ctx context.Context, owner Owner, sessionID string) (Session, error)
    BumpUpdatedAtAndIncCount(ctx context.Context, sessionID string, delta int) error
}

type MessageStore interface {
    Append(ctx context.Context, m Message) error
    UpdateAssistant(ctx context.Context, msgID string, patch AssistantPatch) error
    ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error)
    GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error)
}

type FeedbackStore interface {
    Insert(ctx context.Context, msgID, rating, comment string) (string, error)
}
```

- [ ] **Step 5: Implement cursor functions**

Create `internal/store/cursor.go`:

```go
package store

import (
    "encoding/base64"
    "encoding/json"
    "fmt"
    "time"
)

type cursorPayload struct {
    CreatedAt string `json:"created_at"`
    ID        string `json:"id"`
}

func EncodeCursor(createdAt time.Time, id string) (string, error) {
    return encodeCursorPayload(cursorPayload{
        CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
        ID:        id,
    })
}

func encodeCursorPayload(payload cursorPayload) (string, error) {
    raw, err := json.Marshal(payload)
    if err != nil {
        return "", err
    }
    return base64.StdEncoding.EncodeToString(raw), nil
}

func DecodeCursor(cursor string) (time.Time, string, error) {
    raw, err := base64.StdEncoding.DecodeString(cursor)
    if err != nil {
        return time.Time{}, "", fmt.Errorf("decode cursor: %w", err)
    }
    var payload cursorPayload
    if err := json.Unmarshal(raw, &payload); err != nil {
        return time.Time{}, "", fmt.Errorf("parse cursor: %w", err)
    }
    if payload.CreatedAt == "" || payload.ID == "" {
        return time.Time{}, "", fmt.Errorf("invalid cursor")
    }
    ts, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
    if err != nil {
        return time.Time{}, "", fmt.Errorf("parse cursor created_at: %w", err)
    }
    return ts, payload.ID, nil
}
```

- [ ] **Step 6: Implement MySQL opener and schema verification**

Create `internal/store/mysql.go`:

```go
package store

import (
    "context"
    "database/sql"
    "fmt"
    "time"

    "github.com/compshare-agent/internal/config"
    _ "github.com/go-sql-driver/mysql"
)

func OpenMySQL(cfg config.MySQLConfig) (*sql.DB, error) {
    if cfg.DSN == "" {
        return nil, fmt.Errorf("mysql dsn is required")
    }
    db, err := sql.Open("mysql", cfg.DSN)
    if err != nil {
        return nil, err
    }
    db.SetMaxOpenConns(cfg.MaxOpenConns)
    db.SetMaxIdleConns(cfg.MaxIdleConns)
    db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := db.PingContext(ctx); err != nil {
        _ = db.Close()
        return nil, err
    }
    if err := VerifySchema(ctx, db); err != nil {
        _ = db.Close()
        return nil, err
    }
    return db, nil
}

func VerifySchema(ctx context.Context, db *sql.DB) error {
    for _, table := range []string{"sessions", "messages", "message_feedback"} {
        query := fmt.Sprintf("SELECT 1 FROM %s LIMIT 1", table)
        if _, err := db.ExecContext(ctx, query); err != nil {
            return fmt.Errorf("verify schema table %s: %w", table, err)
        }
    }
    return nil
}
```

- [ ] **Step 7: Implement session store**

Create `internal/store/sessions.go`:

```go
package store

import (
    "context"
    "database/sql"
    "encoding/json"

    "github.com/google/uuid"
)

type MySQLSessionStore struct {
    db *sql.DB
}

func NewSessionStore(db *sql.DB) *MySQLSessionStore {
    return &MySQLSessionStore{db: db}
}

func (s *MySQLSessionStore) Create(ctx context.Context, owner Owner, title *string, ctxJSON json.RawMessage) (Session, error) {
    id := uuid.NewString()
    _, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (id, top_organization_id, organization_id, title, context)
VALUES (?, ?, ?, ?, ?)
`, id, owner.TopOrganizationID, owner.OrganizationID, title, nullableJSON(ctxJSON))
    if err != nil {
        return Session{}, err
    }
    return s.GetByID(ctx, owner, id)
}

func (s *MySQLSessionStore) GetByID(ctx context.Context, owner Owner, sessionID string) (Session, error) {
    var out Session
    var title sql.NullString
    var ctxRaw sql.NullString
    err := s.db.QueryRowContext(ctx, `
SELECT id, top_organization_id, organization_id, title, context, message_count, pinned, created_at, updated_at
FROM sessions
WHERE id = ? AND top_organization_id = ? AND organization_id = ? AND deleted_at IS NULL
`, sessionID, owner.TopOrganizationID, owner.OrganizationID).Scan(
        &out.ID,
        &out.TopOrganizationID,
        &out.OrganizationID,
        &title,
        &ctxRaw,
        &out.MessageCount,
        &out.Pinned,
        &out.CreatedAt,
        &out.UpdatedAt,
    )
    if err != nil {
        return Session{}, err
    }
    if title.Valid {
        out.Title = &title.String
    }
    if ctxRaw.Valid {
        out.Context = json.RawMessage(ctxRaw.String)
    }
    return out, nil
}

func (s *MySQLSessionStore) BumpUpdatedAtAndIncCount(ctx context.Context, sessionID string, delta int) error {
    _, err := s.db.ExecContext(ctx, `
UPDATE sessions SET message_count = message_count + ?, updated_at = NOW(3)
WHERE id = ? AND deleted_at IS NULL
`, delta, sessionID)
    return err
}

func nullableJSON(raw json.RawMessage) any {
    if len(raw) == 0 {
        return nil
    }
    return string(raw)
}
```

- [ ] **Step 8: Implement message store**

Create `internal/store/messages.go`:

```go
package store

import (
    "context"
    "database/sql"
    "encoding/json"
)

type MySQLMessageStore struct {
    db *sql.DB
}

func NewMessageStore(db *sql.DB) *MySQLMessageStore {
    return &MySQLMessageStore{db: db}
}

func (s *MySQLMessageStore) Append(ctx context.Context, m Message) error {
    _, err := s.db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, m.ID, m.SessionID, m.RequestUUID, m.Role, m.Content, m.Status, nullableStringPtr(m.ErrorCode), nullableStringPtr(m.Model), nullableIntPtr(m.InputTokens), nullableIntPtr(m.OutputTokens), nullableIntPtr(m.TTFTMs), nullableIntPtr(m.LatencyMs), nullableJSON(m.Metadata))
    return err
}

func (s *MySQLMessageStore) UpdateAssistant(ctx context.Context, msgID string, patch AssistantPatch) error {
    _, err := s.db.ExecContext(ctx, `
UPDATE messages
SET content = ?, status = ?, error_code = ?, input_tokens = ?, output_tokens = ?, ttft_ms = ?, latency_ms = ?
WHERE id = ? AND role = 'assistant'
`, patch.Content, patch.Status, nullableStringPtr(patch.ErrorCode), nullableIntPtr(patch.InputTokens), nullableIntPtr(patch.OutputTokens), nullableIntPtr(patch.TTFTMs), nullableIntPtr(patch.LatencyMs), msgID)
    return err
}

func (s *MySQLMessageStore) ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error) {
    if limit <= 0 {
        limit = 50
    }
    queryLimit := limit + 1
    var rows *sql.Rows
    var err error
    if cursor == "" {
        rows, err = s.db.QueryContext(ctx, `
SELECT id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata, created_at
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC, id ASC
LIMIT ?
`, sessionID, queryLimit)
    } else {
        ts, id, decodeErr := DecodeCursor(cursor)
        if decodeErr != nil {
            return nil, "", decodeErr
        }
        rows, err = s.db.QueryContext(ctx, `
SELECT id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata, created_at
FROM messages
WHERE session_id = ? AND (created_at > ? OR (created_at = ? AND id > ?))
ORDER BY created_at ASC, id ASC
LIMIT ?
`, sessionID, ts, ts, id, queryLimit)
    }
    if err != nil {
        return nil, "", err
    }
    defer rows.Close()

    messages, err := scanMessages(rows)
    if err != nil {
        return nil, "", err
    }
    nextCursor := ""
    if len(messages) > limit {
        last := messages[limit-1]
        nextCursor, err = EncodeCursor(last.CreatedAt, last.ID)
        if err != nil {
            return nil, "", err
        }
        messages = messages[:limit]
    }
    return messages, nextCursor, nil
}

func (s *MySQLMessageStore) GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error) {
    rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.session_id, m.request_uuid, m.role, m.content, m.status, m.error_code, m.model, m.input_tokens, m.output_tokens, m.ttft_ms, m.latency_ms, m.metadata, m.created_at
FROM messages m
JOIN sessions sess ON sess.id = m.session_id
WHERE m.id = ? AND sess.top_organization_id = ? AND sess.organization_id = ? AND sess.deleted_at IS NULL
`, msgID, owner.TopOrganizationID, owner.OrganizationID)
    if err != nil {
        return Message{}, err
    }
    defer rows.Close()
    messages, err := scanMessages(rows)
    if err != nil {
        return Message{}, err
    }
    if len(messages) == 0 {
        return Message{}, sql.ErrNoRows
    }
    return messages[0], nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
    var messages []Message
    for rows.Next() {
        var m Message
        var errorCode, model, metadata sql.NullString
        var inputTokens, outputTokens, ttftMs, latencyMs sql.NullInt64
        if err := rows.Scan(&m.ID, &m.SessionID, &m.RequestUUID, &m.Role, &m.Content, &m.Status, &errorCode, &model, &inputTokens, &outputTokens, &ttftMs, &latencyMs, &metadata, &m.CreatedAt); err != nil {
            return nil, err
        }
        if errorCode.Valid { m.ErrorCode = &errorCode.String }
        if model.Valid { m.Model = &model.String }
        if inputTokens.Valid { v := int(inputTokens.Int64); m.InputTokens = &v }
        if outputTokens.Valid { v := int(outputTokens.Int64); m.OutputTokens = &v }
        if ttftMs.Valid { v := int(ttftMs.Int64); m.TTFTMs = &v }
        if latencyMs.Valid { v := int(latencyMs.Int64); m.LatencyMs = &v }
        if metadata.Valid { m.Metadata = json.RawMessage(metadata.String) }
        messages = append(messages, m)
    }
    return messages, rows.Err()
}

func nullableStringPtr(v *string) any {
    if v == nil { return nil }
    return *v
}

func nullableIntPtr(v *int) any {
    if v == nil { return nil }
    return *v
}
```

Run `gofmt` later to expand compact one-line `if` statements if desired.

- [ ] **Step 9: Implement feedback store**

Create `internal/store/feedback.go`:

```go
package store

import (
    "context"
    "database/sql"

    "github.com/google/uuid"
)

type MySQLFeedbackStore struct {
    db *sql.DB
}

func NewFeedbackStore(db *sql.DB) *MySQLFeedbackStore {
    return &MySQLFeedbackStore{db: db}
}

func (s *MySQLFeedbackStore) Insert(ctx context.Context, msgID, rating, comment string) (string, error) {
    id := uuid.NewString()
    var commentArg any
    if comment != "" {
        commentArg = comment
    }
    _, err := s.db.ExecContext(ctx, `
INSERT INTO message_feedback (id, message_id, rating, comment)
VALUES (?, ?, ?, ?)
`, id, msgID, rating, commentArg)
    if err != nil {
        return "", err
    }
    return id, nil
}
```

- [ ] **Step 10: Run store tests**

Run:

```bash
go test ./internal/store -count=1
```

Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add deploy/migrations/0001_init.sql internal/store
git commit -m "feat(store): add MySQL session persistence"
```

---

## Task 3: Add LLM delta callback and engine chat options

**Files:**
- Modify: `internal/llm/client.go`
- Modify: `internal/llm/client_test.go`
- Modify: `internal/engine/engine.go`
- Create or modify: `internal/engine/chat_options_test.go`

- [ ] **Step 1: Write engine tests first**

Create `internal/engine/chat_options_test.go`:

```go
package engine

import (
    "context"
    "testing"

    "github.com/compshare-agent/internal/llm"
    "github.com/compshare-agent/internal/tools"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    openai "github.com/sashabaranov/go-openai"
)

type deltaMockLLM struct {
    reqs []llm.ChatRequest
}

func (m *deltaMockLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
    m.reqs = append(m.reqs, req)
    if req.OnTextDelta != nil {
        req.OnTextDelta("你")
        req.OnTextDelta("好")
    }
    return &llm.ChatResponse{Content: "你好", Usage: llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}, nil
}

func TestChatWithOptionsStreamsTextDeltas(t *testing.T) {
    client := &deltaMockLLM{}
    eng := NewWithDeps(client, tools.NewMockExecutor(nil), func(string, map[string]any) bool { return false })
    eng.InitWithContext("用户当前没有实例。")

    var deltas []string
    var usage llm.TokenUsage
    reply, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
        OnTextDelta: func(s string) { deltas = append(deltas, s) },
        OnUsage:     func(u llm.TokenUsage) { usage = u },
    })

    require.NoError(t, err)
    assert.Equal(t, "你好", reply)
    assert.Equal(t, []string{"你", "好"}, deltas)
    assert.Equal(t, 3, usage.TotalTokens)
    require.Len(t, client.reqs, 1)
    assert.NotNil(t, client.reqs[0].OnTextDelta)
}

func TestRehydrateHistoryBuildsSystemUserAssistantMessages(t *testing.T) {
    eng := NewWithDeps(&deltaMockLLM{}, tools.NewMockExecutor(nil), func(string, map[string]any) bool { return false })

    eng.RehydrateHistory([]HistoryMessage{
        {Role: openai.ChatMessageRoleUser, Content: "第一问"},
        {Role: openai.ChatMessageRoleAssistant, Content: "第一答"},
    })

    require.Len(t, eng.messages, 3)
    assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
    assert.Equal(t, openai.ChatMessageRoleUser, eng.messages[1].Role)
    assert.Equal(t, "第一问", eng.messages[1].Content)
    assert.Equal(t, openai.ChatMessageRoleAssistant, eng.messages[2].Role)
    assert.Equal(t, "第一答", eng.messages[2].Content)
}
```

If `tools.NewMockExecutor` does not exist, use the existing test executor helper already present in `internal/engine/*_test.go`; do not create a second mock type if one exists.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
go test ./internal/engine -run 'TestChatWithOptionsStreamsTextDeltas|TestRehydrateHistoryBuildsSystemUserAssistantMessages' -count=1
```

Expected: FAIL because `ChatWithOptions`, `ChatOptions`, and `RehydrateHistory` do not exist.

- [ ] **Step 3: Add llm callback field**

In `internal/llm/client.go`, extend `ChatRequest`:

```go
type ChatRequest struct {
    Messages []openai.ChatCompletionMessage
    Tools    []openai.Tool
    ToolChoice any
    OnTextDelta func(string)
}
```

In `chatOnce`, change the text delta block:

```go
if delta.Content != "" {
    contentBuf.WriteString(delta.Content)
    if req.OnTextDelta != nil {
        req.OnTextDelta(delta.Content)
    }
}
```

- [ ] **Step 4: Add engine types and rehydration**

In `internal/engine/engine.go`, near `KnowledgeRetriever` add:

```go
type HistoryMessage struct {
    Role    string
    Content string
}

type ChatOptions struct {
    OnTextDelta func(string)
    OnUsage     func(llm.TokenUsage)
}
```

Add method near `InitWithContext`:

```go
func (e *Engine) RehydrateHistory(msgs []HistoryMessage) {
    systemPrompt := prompt.BuildSystemWithOptions("", prompt.BuildOptions{MutatingToolsEnabled: e.mutatingToolsEnabled})
    e.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: systemPrompt}}
    for _, msg := range msgs {
        if msg.Content == "" {
            continue
        }
        switch msg.Role {
        case openai.ChatMessageRoleUser, openai.ChatMessageRoleAssistant:
            e.messages = append(e.messages, openai.ChatCompletionMessage{Role: msg.Role, Content: msg.Content})
        }
    }
}
```

- [ ] **Step 5: Make old Chat delegate to ChatWithOptions**

Replace the `Chat` function signature block with:

```go
func (e *Engine) Chat(ctx context.Context, userMsg string, onStep func(StepEvent)) (string, error) {
    return e.ChatWithOptions(ctx, userMsg, onStep, ChatOptions{})
}

func (e *Engine) ChatWithOptions(ctx context.Context, userMsg string, onStep func(StepEvent), opts ChatOptions) (string, error) {
```

Keep the existing body inside `ChatWithOptions`.

- [ ] **Step 6: Wire callbacks only for final text LLM calls**

In the ReAct loop, before `e.llmClient.Chat(ctx, req)`, do not set the callback yet. Instead, after `resp` returns the engine cannot know whether the request had tool calls until it is too late, so the implementation must buffer deltas until it knows the response is final.

Add local buffer before calling LLM:

```go
var streamedDeltas []string
if opts.OnTextDelta != nil {
    req.OnTextDelta = func(s string) {
        streamedDeltas = append(streamedDeltas, s)
    }
}
resp, err := e.llmClient.Chat(ctx, req)
```

Then in the `len(resp.ToolCalls) == 0` final-text branch, before appending assistant message, emit buffered deltas:

```go
if opts.OnTextDelta != nil {
    for _, delta := range streamedDeltas {
        opts.OnTextDelta(delta)
    }
}
```

Call usage observer option after `resp`:

```go
e.emitTokenUsage(resp.Usage)
if opts.OnUsage != nil {
    opts.OnUsage(resp.Usage)
}
```

Keep `e.emitTokenUsage` for existing tracing behavior.

- [ ] **Step 7: Run engine tests**

Run:

```bash
go test ./internal/engine -run 'TestChatWithOptionsStreamsTextDeltas|TestRehydrateHistoryBuildsSystemUserAssistantMessages' -count=1
```

Expected: PASS.

- [ ] **Step 8: Run broader regression**

Run:

```bash
go test ./internal/llm ./internal/engine ./cmd -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/llm/client.go internal/llm/client_test.go internal/engine/engine.go internal/engine/chat_options_test.go
git commit -m "feat(engine): expose streaming chat options"
```

---

## Task 4: Add per-session agent pool

**Files:**
- Create: `internal/agentpool/pool.go`
- Create: `internal/agentpool/rehydrate.go`
- Create: `internal/agentpool/pool_test.go`

- [ ] **Step 1: Write pool tests first**

Create `internal/agentpool/pool_test.go`:

```go
package agentpool

import (
    "context"
    "encoding/json"
    "testing"
    "time"

    "github.com/compshare-agent/internal/config"
    "github.com/compshare-agent/internal/engine"
    "github.com/compshare-agent/internal/store"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

type mockMessageStore struct {
    messages map[string][]store.Message
    calls    int
}

func (m *mockMessageStore) Append(context.Context, store.Message) error { return nil }
func (m *mockMessageStore) UpdateAssistant(context.Context, string, store.AssistantPatch) error { return nil }
func (m *mockMessageStore) GetWithOwnerCheck(context.Context, store.Owner, string) (store.Message, error) { return store.Message{}, nil }
func (m *mockMessageStore) ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]store.Message, string, error) {
    m.calls++
    return m.messages[sessionID], "", nil
}

func TestPoolRehydratesOnMissAndCachesOnHit(t *testing.T) {
    cfg := testConfig()
    ms := &mockMessageStore{messages: map[string][]store.Message{
        "sess-1": {
            {Role: "user", Content: "第一问", Status: "ok"},
            {Role: "assistant", Content: "第一答", Status: "ok"},
        },
    }}
    p := New(cfg, ms, Options{Capacity: 2, IdleTTL: time.Minute})
    defer p.Close()

    first, err := p.Get(context.Background(), "sess-1")
    require.NoError(t, err)
    second, err := p.Get(context.Background(), "sess-1")
    require.NoError(t, err)

    assert.Same(t, first, second)
    assert.Equal(t, 1, ms.calls)
}

func TestPoolEvictsLeastRecentlyUsed(t *testing.T) {
    cfg := testConfig()
    ms := &mockMessageStore{messages: map[string][]store.Message{}}
    p := New(cfg, ms, Options{Capacity: 1, IdleTTL: time.Minute})
    defer p.Close()

    first, err := p.Get(context.Background(), "sess-1")
    require.NoError(t, err)
    _, err = p.Get(context.Background(), "sess-2")
    require.NoError(t, err)
    again, err := p.Get(context.Background(), "sess-1")
    require.NoError(t, err)

    assert.NotSame(t, first, again)
}

func testConfig() *config.Config {
    return &config.Config{Agent: config.AgentConfig{
        PublicKey:  "pub",
        PrivateKey: "priv",
        Region:     "cn-wlcb",
        LLM: config.LLMConfig{BaseURL: "http://127.0.0.1", APIKey: "key", Model: "test-model"},
    }}
}

var _ = json.RawMessage{}
var _ = engine.HistoryMessage{}
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
go test ./internal/agentpool -count=1
```

Expected: FAIL because `agentpool` does not exist.

- [ ] **Step 3: Implement pool**

Create `internal/agentpool/pool.go`:

```go
package agentpool

import (
    "container/list"
    "context"
    "sync"
    "time"

    "github.com/compshare-agent/internal/config"
    "github.com/compshare-agent/internal/engine"
    "github.com/compshare-agent/internal/store"
)

type Options struct {
    Capacity int
    IdleTTL  time.Duration
}

type Pool struct {
    cfg          *config.Config
    messageStore store.MessageStore
    capacity     int
    idleTTL      time.Duration

    mu      sync.Mutex
    entries map[string]*list.Element
    lru     *list.List
    stop    chan struct{}
}

type entry struct {
    sessionID   string
    engine      *engine.Engine
    lastTouched time.Time
}

func New(cfg *config.Config, messageStore store.MessageStore, opts Options) *Pool {
    if opts.Capacity <= 0 {
        opts.Capacity = 200
    }
    if opts.IdleTTL <= 0 {
        opts.IdleTTL = 30 * time.Minute
    }
    p := &Pool{
        cfg:          cfg,
        messageStore: messageStore,
        capacity:     opts.Capacity,
        idleTTL:      opts.IdleTTL,
        entries:      map[string]*list.Element{},
        lru:          list.New(),
        stop:         make(chan struct{}),
    }
    go p.gcLoop()
    return p
}

func (p *Pool) Get(ctx context.Context, sessionID string) (*engine.Engine, error) {
    now := time.Now()
    p.mu.Lock()
    if elem, ok := p.entries[sessionID]; ok {
        ent := elem.Value.(*entry)
        ent.lastTouched = now
        p.lru.MoveToFront(elem)
        eng := ent.engine
        p.mu.Unlock()
        return eng, nil
    }
    p.mu.Unlock()

    eng, err := p.buildEngine(ctx, sessionID)
    if err != nil {
        return nil, err
    }

    p.mu.Lock()
    defer p.mu.Unlock()
    if elem, ok := p.entries[sessionID]; ok {
        ent := elem.Value.(*entry)
        ent.lastTouched = now
        p.lru.MoveToFront(elem)
        return ent.engine, nil
    }
    elem := p.lru.PushFront(&entry{sessionID: sessionID, engine: eng, lastTouched: now})
    p.entries[sessionID] = elem
    p.evictOverflowLocked()
    return eng, nil
}

func (p *Pool) Close() {
    close(p.stop)
}

func (p *Pool) evictOverflowLocked() {
    for len(p.entries) > p.capacity {
        elem := p.lru.Back()
        if elem == nil {
            return
        }
        ent := elem.Value.(*entry)
        delete(p.entries, ent.sessionID)
        p.lru.Remove(elem)
    }
}

func (p *Pool) gcLoop() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            p.evictIdle(time.Now())
        case <-p.stop:
            return
        }
    }
}

func (p *Pool) evictIdle(now time.Time) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for elem := p.lru.Back(); elem != nil; {
        prev := elem.Prev()
        ent := elem.Value.(*entry)
        if now.Sub(ent.lastTouched) > p.idleTTL {
            delete(p.entries, ent.sessionID)
            p.lru.Remove(elem)
        }
        elem = prev
    }
}
```

- [ ] **Step 4: Implement rehydrate**

Create `internal/agentpool/rehydrate.go`:

```go
package agentpool

import (
    "context"

    "github.com/compshare-agent/internal/engine"
    "github.com/compshare-agent/internal/store"
)

func (p *Pool) buildEngine(ctx context.Context, sessionID string) (*engine.Engine, error) {
    eng := engine.New(p.cfg, func(string, map[string]any) bool { return false })
    messages, _, err := p.messageStore.ListBySession(ctx, sessionID, 100, "")
    if err != nil {
        return nil, err
    }
    history := make([]engine.HistoryMessage, 0, len(messages))
    for _, msg := range messages {
        if msg.Status != "ok" {
            continue
        }
        if msg.Role != "user" && msg.Role != "assistant" {
            continue
        }
        history = append(history, engine.HistoryMessage{Role: msg.Role, Content: msg.Content})
    }
    eng.RehydrateHistory(history)
    return eng, nil
}
```

- [ ] **Step 5: Run pool tests**

Run:

```bash
go test ./internal/agentpool -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agentpool
git commit -m "feat(agentpool): cache per-session engines"
```

---

## Task 5: Add HTTP API core, base request parsing, errors, and SSE writer

**Files:**
- Create: `internal/httpapi/errors.go`
- Create: `internal/httpapi/baserequest.go`
- Create: `internal/httpapi/baserequest_test.go`
- Create: `internal/httpapi/sse/writer.go`
- Create: `internal/httpapi/sse/writer_test.go`
- Create: `internal/httpapi/healthz.go`

- [ ] **Step 1: Write base request tests first**

Create `internal/httpapi/baserequest_test.go`:

```go
package httpapi

import (
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestParseBaseRequestJSONGeneratesRequestUUID(t *testing.T) {
    c := testContext("application/json", `{"Action":"GetMeta","top_organization_id":123,"organization_id":456}`)

    raw, base, err := ParseBaseRequest(c)

    require.NoError(t, err)
    assert.Equal(t, "GetMeta", base.Action)
    assert.Equal(t, uint32(123), base.Owner.TopOrganizationID)
    assert.Equal(t, uint32(456), base.Owner.OrganizationID)
    assert.NotEmpty(t, base.RequestUUID)
    got, _ := raw.Get("request_uuid").String()
    assert.Equal(t, base.RequestUUID, got)
}

func TestParseBaseRequestForm(t *testing.T) {
    c := testContext("application/x-www-form-urlencoded", "Action=Chat&SessionId=sess-1&request_uuid=req-1&top_organization_id=123&organization_id=456")

    raw, base, err := ParseBaseRequest(c)

    require.NoError(t, err)
    assert.Equal(t, "Chat", base.Action)
    assert.Equal(t, "req-1", base.RequestUUID)
    assert.Equal(t, "sess-1", raw.Get("SessionId").MustString())
}

func TestParseBaseRequestRejectsMissingOrganization(t *testing.T) {
    c := testContext("application/json", `{"Action":"GetMeta","top_organization_id":123}`)

    _, _, err := ParseBaseRequest(c)

    require.Error(t, err)
    apiErr := AsAPIError(err)
    assert.Equal(t, "InvalidParam", apiErr.Code)
}

func testContext(contentType, body string) *gin.Context {
    gin.SetMode(gin.TestMode)
    req := httptest.NewRequest(http.MethodPost, "/api/gateway", strings.NewReader(body))
    req.Header.Set("Content-Type", contentType)
    w := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(w)
    c.Request = req
    return c
}
```

- [ ] **Step 2: Write SSE writer tests first**

Create `internal/httpapi/sse/writer_test.go`:

```go
package sse

import (
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestWriteEvent(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

    writer := New(c.Writer)
    err := writer.WriteEvent("token", map[string]string{"Text": "你好"})

    require.NoError(t, err)
    assert.Equal(t, "event: token\ndata: {\"Text\":\"你好\"}\n\n", rec.Body.String())
}

func TestWriteKeepalive(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)

    writer := New(c.Writer)
    err := writer.WriteKeepalive()

    require.NoError(t, err)
    assert.Equal(t, ":keepalive\n\n", rec.Body.String())
}
```

- [ ] **Step 3: Run tests and verify failure**

Run:

```bash
go test ./internal/httpapi/... -run 'TestParseBaseRequest|TestWriteEvent|TestWriteKeepalive' -count=1
```

Expected: FAIL because package files do not exist.

- [ ] **Step 4: Implement API errors**

Create `internal/httpapi/errors.go`:

```go
package httpapi

import (
    "errors"
    "fmt"
    "net/http"
)

type APIError struct {
    Code    string
    Status  int
    Message string
}

func (e *APIError) Error() string {
    if e.Message != "" {
        return e.Message
    }
    return e.Code
}

func (e *APIError) WithMessage(format string, args ...any) *APIError {
    cp := *e
    cp.Message = fmt.Sprintf(format, args...)
    return &cp
}

var (
    ErrInvalidParam = &APIError{Code: "InvalidParam", Status: http.StatusBadRequest, Message: "参数缺失或非法"}
    ErrUnauthorized = &APIError{Code: "Unauthorized", Status: http.StatusUnauthorized, Message: "未登录或 token 失效"}
    ErrForbidden    = &APIError{Code: "Forbidden", Status: http.StatusForbidden, Message: "无权访问"}
    ErrNotFound     = &APIError{Code: "NotFound", Status: http.StatusNotFound, Message: "资源不存在"}
    ErrRateLimited  = &APIError{Code: "RateLimited", Status: http.StatusTooManyRequests, Message: "超出速率限制"}
    ErrInternal     = &APIError{Code: "InternalError", Status: http.StatusInternalServerError, Message: "后端未预期错误"}
    ErrModelTimeout = &APIError{Code: "ModelTimeout", Status: http.StatusGatewayTimeout, Message: "LLM 调用超时"}
    ErrModelError   = &APIError{Code: "ModelError", Status: http.StatusBadGateway, Message: "LLM 上游错误"}
    ErrAborted      = &APIError{Code: "Aborted", Status: 499, Message: "用户中断"}
)

func AsAPIError(err error) *APIError {
    if err == nil {
        return nil
    }
    var apiErr *APIError
    if errors.As(err, &apiErr) {
        return apiErr
    }
    return ErrInternal.WithMessage(err.Error())
}
```

- [ ] **Step 5: Implement base request parsing**

Create `internal/httpapi/baserequest.go`:

```go
package httpapi

import (
    "encoding/json"
    "io"
    "net/http"
    "strconv"

    "github.com/bitly/go-simplejson"
    "github.com/compshare-agent/internal/store"
    "github.com/gin-gonic/gin"
    "github.com/google/uuid"
)

type BaseRequest struct {
    Action      string
    RequestUUID string
    Owner       store.Owner
}

func ParseBaseRequest(c *gin.Context) (*simplejson.Json, BaseRequest, error) {
    raw, err := parseBody(c.Request)
    if err != nil {
        return nil, BaseRequest{}, err
    }
    action := raw.Get("Action").MustString()
    if action == "" {
        return nil, BaseRequest{}, ErrInvalidParam.WithMessage("missing Action")
    }
    requestUUID := raw.Get("request_uuid").MustString()
    if requestUUID == "" {
        requestUUID = raw.Get("RequestId").MustString()
    }
    if requestUUID == "" {
        requestUUID = uuid.NewString()
        raw.Set("request_uuid", requestUUID)
    }
    topOrg, err := readUint32(raw, "top_organization_id")
    if err != nil || topOrg == 0 {
        return nil, BaseRequest{}, ErrInvalidParam.WithMessage("missing top_organization_id")
    }
    org, err := readUint32(raw, "organization_id")
    if err != nil || org == 0 {
        return nil, BaseRequest{}, ErrInvalidParam.WithMessage("missing organization_id")
    }
    return raw, BaseRequest{
        Action:      action,
        RequestUUID: requestUUID,
        Owner: store.Owner{TopOrganizationID: topOrg, OrganizationID: org},
    }, nil
}

func parseBody(r *http.Request) (*simplejson.Json, error) {
    if r.Method != http.MethodPost {
        return nil, ErrInvalidParam.WithMessage("only support post request")
    }
    contentType := r.Header.Get("Content-Type")
    if contentType == "application/x-www-form-urlencoded" {
        if err := r.ParseForm(); err != nil {
            return nil, ErrInvalidParam.WithMessage("invalid form body")
        }
        m := map[string]any{}
        for k, values := range r.PostForm {
            if len(values) > 0 {
                m[k] = values[0]
            }
        }
        return simplejson.NewJson(mustJSON(m))
    }
    body, err := io.ReadAll(r.Body)
    if err != nil {
        return nil, ErrInvalidParam.WithMessage("read body: %v", err)
    }
    if len(body) == 0 {
        return nil, ErrInvalidParam.WithMessage("empty body")
    }
    data, err := simplejson.NewJson(body)
    if err != nil {
        return nil, ErrInvalidParam.WithMessage("invalid json body")
    }
    return data, nil
}

func mustJSON(v any) []byte {
    raw, err := json.Marshal(v)
    if err != nil {
        panic(err)
    }
    return raw
}

func readUint32(raw *simplejson.Json, key string) (uint32, error) {
    if v, err := raw.Get(key).Uint64(); err == nil {
        return uint32(v), nil
    }
    s := raw.Get(key).MustString()
    if s == "" {
        return 0, nil
    }
    v, err := strconv.ParseUint(s, 10, 32)
    return uint32(v), err
}
```

- [ ] **Step 6: Implement SSE writer**

Create `internal/httpapi/sse/writer.go`:

```go
package sse

import (
    "encoding/json"
    "fmt"
    "net/http"
    "sync"
)

type Writer struct {
    w  http.ResponseWriter
    fl http.Flusher
    mu sync.Mutex
}

func New(w http.ResponseWriter) *Writer {
    fl, _ := w.(http.Flusher)
    return &Writer{w: w, fl: fl}
}

func (s *Writer) WriteEvent(event string, data any) error {
    raw, err := json.Marshal(data)
    if err != nil {
        return err
    }
    s.mu.Lock()
    defer s.mu.Unlock()
    if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
        return err
    }
    if s.fl != nil {
        s.fl.Flush()
    }
    return nil
}

func (s *Writer) WriteKeepalive() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if _, err := fmt.Fprint(s.w, ":keepalive\n\n"); err != nil {
        return err
    }
    if s.fl != nil {
        s.fl.Flush()
    }
    return nil
}
```

- [ ] **Step 7: Implement healthz**

Create `internal/httpapi/healthz.go`:

```go
package httpapi

import (
    "net/http"

    "github.com/gin-gonic/gin"
)

func Healthz(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
```

- [ ] **Step 8: Run HTTP core tests**

Run:

```bash
go test ./internal/httpapi/... -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/httpapi
git commit -m "feat(httpapi): add gateway request primitives"
```

---

## Task 6: Add non-streaming handlers and dispatcher

**Files:**
- Create: `internal/httpapi/types.go`
- Create: `internal/httpapi/handlers.go`
- Create: `internal/httpapi/dispatch.go`
- Create: `internal/httpapi/handlers_meta.go`
- Create: `internal/httpapi/handlers_session.go`
- Create: `internal/httpapi/handlers_feedback.go`
- Create: `internal/httpapi/handlers_test.go`

- [ ] **Step 1: Write handler tests first**

Create `internal/httpapi/handlers_test.go` with focused mock stores:

```go
package httpapi

import (
    "context"
    "database/sql"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/compshare-agent/internal/config"
    "github.com/compshare-agent/internal/store"
    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

type mockSessions struct{ byID map[string]store.Session }
func (m *mockSessions) Create(ctx context.Context, owner store.Owner, title *string, ctxJSON json.RawMessage) (store.Session, error) {
    s := store.Session{ID: "sess-new", TopOrganizationID: owner.TopOrganizationID, OrganizationID: owner.OrganizationID, Title: title, Context: ctxJSON, CreatedAt: time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)}
    if m.byID == nil { m.byID = map[string]store.Session{} }
    m.byID[s.ID] = s
    return s, nil
}
func (m *mockSessions) GetByID(ctx context.Context, owner store.Owner, sessionID string) (store.Session, error) {
    s, ok := m.byID[sessionID]
    if !ok || s.TopOrganizationID != owner.TopOrganizationID || s.OrganizationID != owner.OrganizationID { return store.Session{}, sql.ErrNoRows }
    return s, nil
}
func (m *mockSessions) BumpUpdatedAtAndIncCount(context.Context, string, int) error { return nil }

type mockMessages struct{ list []store.Message; checked map[string]store.Message }
func (m *mockMessages) Append(context.Context, store.Message) error { return nil }
func (m *mockMessages) UpdateAssistant(context.Context, string, store.AssistantPatch) error { return nil }
func (m *mockMessages) ListBySession(context.Context, string, int, string) ([]store.Message, string, error) { return m.list, "", nil }
func (m *mockMessages) GetWithOwnerCheck(ctx context.Context, owner store.Owner, msgID string) (store.Message, error) {
    msg, ok := m.checked[msgID]
    if !ok { return store.Message{}, sql.ErrNoRows }
    return msg, nil
}

type mockFeedback struct{}
func (m mockFeedback) Insert(context.Context, string, string, string) (string, error) { return "fb-1", nil }

func TestDispatchGetMeta(t *testing.T) {
    h := newTestHandlers()
    rec := performGateway(h, `{"Action":"GetMeta","top_organization_id":1,"organization_id":2}`)

    require.Equal(t, http.StatusOK, rec.Code)
    assert.Contains(t, rec.Body.String(), `"Code":"Success"`)
    assert.Contains(t, rec.Body.String(), `"Welcome":"welcome"`)
}

func TestDispatchCreateSession(t *testing.T) {
    h := newTestHandlers()
    rec := performGateway(h, `{"Action":"CreateSession","Title":"hello","top_organization_id":1,"organization_id":2}`)

    require.Equal(t, http.StatusOK, rec.Code)
    assert.Contains(t, rec.Body.String(), `"SessionId":"sess-new"`)
}

func TestDispatchGetSessionRequiresSessionID(t *testing.T) {
    h := newTestHandlers()
    rec := performGateway(h, `{"Action":"GetSession","top_organization_id":1,"organization_id":2}`)

    require.Equal(t, http.StatusBadRequest, rec.Code)
    assert.Contains(t, rec.Body.String(), `"Code":"InvalidParam"`)
}

func TestDispatchFeedback(t *testing.T) {
    h := newTestHandlers()
    rec := performGateway(h, `{"Action":"Feedback","MessageId":"msg-1","Rating":"Up","top_organization_id":1,"organization_id":2}`)

    require.Equal(t, http.StatusOK, rec.Code)
    assert.Contains(t, rec.Body.String(), `"FeedbackId":"fb-1"`)
}

func newTestHandlers() *Handlers {
    title := "session title"
    return NewHandlers(&config.Config{Agent: config.AgentConfig{
        LLM: config.LLMConfig{Model: "model-x"},
        Meta: config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p1"}, MaxInputLength: 4000},
        HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: 15 * time.Second},
    }}, &mockSessions{byID: map[string]store.Session{"sess-1": {ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2, Title: &title, CreatedAt: time.Now(), UpdatedAt: time.Now()}}}, &mockMessages{checked: map[string]store.Message{"msg-1": {ID: "msg-1", SessionID: "sess-1", Role: "assistant", Status: "ok"}}}, mockFeedback{}, nil)
}

func performGateway(h *Handlers, body string) *httptest.ResponseRecorder {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    c.Request = httptest.NewRequest(http.MethodPost, "/api/gateway", strings.NewReader(body))
    c.Request.Header.Set("Content-Type", "application/json")
    h.Dispatch(c)
    return rec
}
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
go test ./internal/httpapi -run 'TestDispatchGetMeta|TestDispatchCreateSession|TestDispatchGetSessionRequiresSessionID|TestDispatchFeedback' -count=1
```

Expected: FAIL because handlers do not exist.

- [ ] **Step 3: Implement response/request types**

Create `internal/httpapi/types.go`:

```go
package httpapi

import "time"

type Response struct {
    RequestID string `json:"RequestId"`
    Code      string `json:"Code"`
    Message   string `json:"Message"`
    Data      any    `json:"Data"`
}

type MessageDTO struct {
    MessageID string    `json:"MessageId"`
    Role      string    `json:"Role"`
    Content   string    `json:"Content"`
    Status    string    `json:"Status"`
    CreatedAt time.Time `json:"CreatedAt"`
}
```

- [ ] **Step 4: Implement handler container**

Create `internal/httpapi/handlers.go`:

```go
package httpapi

import (
    "github.com/compshare-agent/internal/agentpool"
    "github.com/compshare-agent/internal/config"
    "github.com/compshare-agent/internal/store"
)

type Handlers struct {
    cfg           *config.Config
    sessions      store.SessionStore
    messages      store.MessageStore
    feedback      store.FeedbackStore
    pool          *agentpool.Pool
}

func NewHandlers(cfg *config.Config, sessions store.SessionStore, messages store.MessageStore, feedback store.FeedbackStore, pool *agentpool.Pool) *Handlers {
    return &Handlers{cfg: cfg, sessions: sessions, messages: messages, feedback: feedback, pool: pool}
}
```

- [ ] **Step 5: Implement dispatch**

Create `internal/httpapi/dispatch.go`:

```go
package httpapi

import (
    "database/sql"
    "errors"
    "net/http"

    "github.com/gin-gonic/gin"
)

func (h *Handlers) Dispatch(c *gin.Context) {
    raw, base, err := ParseBaseRequest(c)
    if err != nil {
        h.writeError(c, "", err)
        return
    }

    switch base.Action {
    case "GetMeta":
        h.writeResult(c, base, h.handleGetMeta(c, base, raw))
    case "CreateSession":
        h.writeResult(c, base, h.handleCreateSession(c, base, raw))
    case "GetSession":
        h.writeResult(c, base, h.handleGetSession(c, base, raw))
    case "Feedback":
        h.writeResult(c, base, h.handleFeedback(c, base, raw))
    case "Chat":
        h.handleChat(c, base, raw)
    default:
        h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("unsupported Action %s", base.Action))
    }
}

func (h *Handlers) writeResult(c *gin.Context, base BaseRequest, data any, err error) {
    if err != nil {
        h.writeError(c, base.RequestUUID, err)
        return
    }
    c.JSON(http.StatusOK, Response{RequestID: base.RequestUUID, Code: "Success", Message: "", Data: data})
}

func (h *Handlers) writeError(c *gin.Context, requestID string, err error) {
    if errors.Is(err, sql.ErrNoRows) {
        err = ErrNotFound
    }
    apiErr := AsAPIError(err)
    c.JSON(apiErr.Status, Response{RequestID: requestID, Code: apiErr.Code, Message: apiErr.Message, Data: nil})
}
```

- [ ] **Step 6: Implement meta handler**

Create `internal/httpapi/handlers_meta.go`:

```go
package httpapi

import "github.com/bitly/go-simplejson"

type metaData struct {
    Model            string   `json:"Model"`
    Version          string   `json:"Version"`
    Welcome          string   `json:"Welcome"`
    SuggestedPrompts []string `json:"SuggestedPrompts"`
    MaxInputLength   int      `json:"MaxInputLength"`
}

func (h *Handlers) handleGetMeta(_ any, _ BaseRequest, _ *simplejson.Json) (any, error) {
    return metaData{
        Model:            h.cfg.Agent.LLM.Model,
        Version:          "0.1.0",
        Welcome:          h.cfg.Agent.Meta.Welcome,
        SuggestedPrompts: h.cfg.Agent.Meta.SuggestedPrompts,
        MaxInputLength:   h.cfg.Agent.Meta.MaxInputLength,
    }, nil
}
```

If Go rejects `_ any` because dispatch passes `*gin.Context`, change the signature to `func (h *Handlers) handleGetMeta(_ *gin.Context, _ BaseRequest, _ *simplejson.Json) (any, error)` and import gin. Use that same signature pattern for the other handlers.

- [ ] **Step 7: Implement session handlers**

Create `internal/httpapi/handlers_session.go`:

```go
package httpapi

import (
    "encoding/json"

    "github.com/bitly/go-simplejson"
    "github.com/gin-gonic/gin"
)

type createSessionData struct {
    SessionID string  `json:"SessionId"`
    Title     *string `json:"Title"`
    CreatedAt any     `json:"CreatedAt"`
}

type getSessionData struct {
    SessionID    string       `json:"SessionId"`
    Title        *string      `json:"Title"`
    MessageCount int          `json:"MessageCount"`
    CreatedAt    any          `json:"CreatedAt"`
    UpdatedAt    any          `json:"UpdatedAt"`
    Messages     []MessageDTO `json:"Messages"`
    NextCursor   string       `json:"NextCursor,omitempty"`
}

func (h *Handlers) handleCreateSession(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
    title := optionalString(raw, "Title")
    ctxJSON, err := optionalJSON(raw, "Context")
    if err != nil {
        return nil, ErrInvalidParam.WithMessage("invalid Context")
    }
    sess, err := h.sessions.Create(c.Request.Context(), base.Owner, title, ctxJSON)
    if err != nil {
        return nil, err
    }
    return createSessionData{SessionID: sess.ID, Title: sess.Title, CreatedAt: sess.CreatedAt}, nil
}

func (h *Handlers) handleGetSession(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
    sessionID := raw.Get("SessionId").MustString()
    if sessionID == "" {
        return nil, ErrInvalidParam.WithMessage("missing SessionId")
    }
    limit := raw.Get("Limit").MustInt(50)
    if limit < 1 || limit > 100 {
        return nil, ErrInvalidParam.WithMessage("Limit must be between 1 and 100")
    }
    cursor := raw.Get("Cursor").MustString()
    sess, err := h.sessions.GetByID(c.Request.Context(), base.Owner, sessionID)
    if err != nil {
        return nil, err
    }
    messages, nextCursor, err := h.messages.ListBySession(c.Request.Context(), sessionID, limit, cursor)
    if err != nil {
        return nil, ErrInvalidParam.WithMessage("invalid Cursor")
    }
    dtos := make([]MessageDTO, 0, len(messages))
    for _, msg := range messages {
        dtos = append(dtos, MessageDTO{MessageID: msg.ID, Role: msg.Role, Content: msg.Content, Status: msg.Status, CreatedAt: msg.CreatedAt})
    }
    return getSessionData{SessionID: sess.ID, Title: sess.Title, MessageCount: sess.MessageCount, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt, Messages: dtos, NextCursor: nextCursor}, nil
}

func optionalString(raw *simplejson.Json, key string) *string {
    s := raw.Get(key).MustString()
    if s == "" {
        return nil
    }
    return &s
}

func optionalJSON(raw *simplejson.Json, key string) (json.RawMessage, error) {
    s := raw.Get(key).MustString()
    if s == "" {
        return nil, nil
    }
    var v any
    if err := json.Unmarshal([]byte(s), &v); err != nil {
        return nil, err
    }
    return json.RawMessage(s), nil
}
```

- [ ] **Step 8: Implement feedback handler**

Create `internal/httpapi/handlers_feedback.go`:

```go
package httpapi

import (
    "github.com/bitly/go-simplejson"
    "github.com/gin-gonic/gin"
)

type feedbackData struct {
    FeedbackID string `json:"FeedbackId"`
}

func (h *Handlers) handleFeedback(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
    messageID := raw.Get("MessageId").MustString()
    if messageID == "" {
        return nil, ErrInvalidParam.WithMessage("missing MessageId")
    }
    rating := raw.Get("Rating").MustString()
    if rating != "Up" && rating != "Down" {
        return nil, ErrInvalidParam.WithMessage("Rating must be Up or Down")
    }
    comment := raw.Get("Comment").MustString()
    if _, err := h.messages.GetWithOwnerCheck(c.Request.Context(), base.Owner, messageID); err != nil {
        return nil, err
    }
    id, err := h.feedback.Insert(c.Request.Context(), messageID, rating, comment)
    if err != nil {
        return nil, err
    }
    return feedbackData{FeedbackID: id}, nil
}
```

- [ ] **Step 9: Run handler tests**

Run:

```bash
go test ./internal/httpapi -count=1
```

Expected: PASS. Fix compile mismatches in signatures; do not change protocol field names.

- [ ] **Step 10: Commit**

```bash
git add internal/httpapi
git commit -m "feat(httpapi): add phase-one non-stream handlers"
```

---

## Task 7: Add Chat SSE handler

**Files:**
- Modify: `internal/httpapi/handlers.go`
- Create: `internal/httpapi/handlers_chat.go`
- Create: `internal/httpapi/handlers_chat_test.go`

- [ ] **Step 1: Add interface seam for pool**

Modify `internal/httpapi/handlers.go` so tests can pass a fake pool:

```go
type EnginePool interface {
    Get(ctx context.Context, sessionID string) (*engine.Engine, error)
}
```

Add imports:

```go
import (
    "context"
    "github.com/compshare-agent/internal/engine"
)
```

Change `pool *agentpool.Pool` field to `pool EnginePool`. Keep `NewHandlers` parameter type as `EnginePool`.

- [ ] **Step 2: Write Chat handler test first**

Create `internal/httpapi/handlers_chat_test.go` using a fake pool backed by an engine with mock LLM. Reuse `deltaMockLLM` pattern from engine tests or define a local one.

```go
package httpapi

import (
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/compshare-agent/internal/config"
    "github.com/compshare-agent/internal/engine"
    "github.com/compshare-agent/internal/llm"
    "github.com/compshare-agent/internal/store"
    "github.com/compshare-agent/internal/tools"
    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

type chatLLM struct{}
func (m chatLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
    if req.OnTextDelta != nil {
        req.OnTextDelta("你")
        req.OnTextDelta("好")
    }
    return &llm.ChatResponse{Content: "你好", Usage: llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}, nil
}

type fakePool struct{ eng *engine.Engine }
func (p fakePool) Get(ctx context.Context, sessionID string) (*engine.Engine, error) { return p.eng, nil }

type recordingMessages struct{ mockMessages; appended []store.Message; patch store.AssistantPatch }
func (m *recordingMessages) Append(ctx context.Context, msg store.Message) error { m.appended = append(m.appended, msg); return nil }
func (m *recordingMessages) UpdateAssistant(ctx context.Context, msgID string, patch store.AssistantPatch) error { m.patch = patch; return nil }

func TestDispatchChatStreamsMetaTokenDone(t *testing.T) {
    eng := engine.NewWithDeps(chatLLM{}, tools.NewMockExecutor(nil), func(string, map[string]any) bool { return false })
    eng.RehydrateHistory(nil)
    messages := &recordingMessages{}
    h := NewHandlers(&config.Config{Agent: config.AgentConfig{
        LLM: config.LLMConfig{Model: "model-x"},
        HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
        Meta: config.MetaConfig{MaxInputLength: 4000},
    }}, &mockSessions{byID: map[string]store.Session{"sess-1": {ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2, CreatedAt: time.Now(), UpdatedAt: time.Now()}}}, messages, mockFeedback{}, fakePool{eng: eng})

    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    c.Request = httptest.NewRequest(http.MethodPost, "/api/gateway", strings.NewReader(`{"Action":"Chat","SessionId":"sess-1","Message":"hi","request_uuid":"req-1","top_organization_id":1,"organization_id":2}`))
    c.Request.Header.Set("Content-Type", "application/json")

    h.Dispatch(c)

    require.Equal(t, http.StatusOK, rec.Code)
    body := rec.Body.String()
    assert.Contains(t, body, "event: meta")
    assert.Contains(t, body, `"RequestId":"req-1"`)
    assert.Contains(t, body, "event: token")
    assert.Contains(t, body, `"Text":"你"`)
    assert.Contains(t, body, `"Text":"好"`)
    assert.Contains(t, body, "event: done")
    require.Len(t, messages.appended, 2)
    assert.Equal(t, "user", messages.appended[0].Role)
    assert.Equal(t, "assistant", messages.appended[1].Role)
    assert.Equal(t, "你好", messages.patch.Content)
    assert.Equal(t, "ok", messages.patch.Status)
}
```

If `tools.NewMockExecutor(nil)` does not exist, use the existing engine test helper.

- [ ] **Step 3: Run test and verify failure**

Run:

```bash
go test ./internal/httpapi -run TestDispatchChatStreamsMetaTokenDone -count=1
```

Expected: FAIL because Chat handler does not exist.

- [ ] **Step 4: Implement Chat handler**

Create `internal/httpapi/handlers_chat.go`:

```go
package httpapi

import (
    "context"
    "errors"
    "net/http"
    "strings"
    "time"

    "github.com/bitly/go-simplejson"
    "github.com/compshare-agent/internal/engine"
    "github.com/compshare-agent/internal/httpapi/sse"
    "github.com/compshare-agent/internal/llm"
    "github.com/compshare-agent/internal/store"
    "github.com/gin-gonic/gin"
    "github.com/google/uuid"
)

type metaEvent struct {
    RequestID string `json:"RequestId"`
    SessionID string `json:"SessionId"`
    MessageID string `json:"MessageId"`
}

type tokenEvent struct { Text string `json:"Text"` }
type doneEvent struct {
    Usage     usageEvent `json:"Usage"`
    LatencyMs int        `json:"LatencyMs"`
    TTFTMs    int        `json:"TtftMs"`
}
type usageEvent struct { InputTokens int `json:"InputTokens"`; OutputTokens int `json:"OutputTokens"` }

type streamErrorEvent struct { Code string `json:"Code"`; Message string `json:"Message"` }

func (h *Handlers) handleChat(c *gin.Context, base BaseRequest, raw *simplejson.Json) {
    sessionID := raw.Get("SessionId").MustString()
    if sessionID == "" {
        h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("missing SessionId"))
        return
    }
    message := strings.TrimSpace(raw.Get("Message").MustString())
    if message == "" {
        h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("missing Message"))
        return
    }
    if len([]rune(message)) > h.cfg.Agent.HTTP.MaxInputLength {
        h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("Message exceeds MaxInputLength"))
        return
    }
    if _, err := h.sessions.GetByID(c.Request.Context(), base.Owner, sessionID); err != nil {
        h.writeError(c, base.RequestUUID, err)
        return
    }

    userMsgID := uuid.NewString()
    assistantMsgID := uuid.NewString()
    model := h.cfg.Agent.LLM.Model
    if err := h.messages.Append(c.Request.Context(), store.Message{ID: userMsgID, SessionID: sessionID, RequestUUID: base.RequestUUID, Role: "user", Content: message, Status: "ok"}); err != nil {
        h.writeError(c, base.RequestUUID, err)
        return
    }
    if err := h.messages.Append(c.Request.Context(), store.Message{ID: assistantMsgID, SessionID: sessionID, RequestUUID: base.RequestUUID, Role: "assistant", Content: "", Status: "pending", Model: &model}); err != nil {
        h.writeError(c, base.RequestUUID, err)
        return
    }
    if err := h.sessions.BumpUpdatedAtAndIncCount(c.Request.Context(), sessionID, 2); err != nil {
        h.writeError(c, base.RequestUUID, err)
        return
    }

    c.Header("Content-Type", "text/event-stream")
    c.Header("Cache-Control", "no-cache")
    c.Header("X-Accel-Buffering", "no")
    c.Status(http.StatusOK)
    sw := sse.New(c.Writer)
    _ = sw.WriteEvent("meta", metaEvent{RequestID: base.RequestUUID, SessionID: sessionID, MessageID: assistantMsgID})

    agent, err := h.pool.Get(c.Request.Context(), sessionID)
    if err != nil {
        h.writeStreamError(c.Request.Context(), sw, assistantMsgID, ErrInternal.WithMessage(err.Error()))
        return
    }

    start := time.Now()
    var firstToken time.Time
    var usage llm.TokenUsage
    ticker := time.NewTicker(h.cfg.Agent.HTTP.SSEKeepaliveInterval)
    defer ticker.Stop()
    done := make(chan struct{})
    go func() {
        defer close(done)
        for {
            select {
            case <-ticker.C:
                _ = sw.WriteKeepalive()
            case <-c.Request.Context().Done():
                return
            }
        }
    }()

    reply, chatErr := agent.ChatWithOptions(c.Request.Context(), message, nil, engine.ChatOptions{
        OnTextDelta: func(s string) {
            if firstToken.IsZero() { firstToken = time.Now() }
            _ = sw.WriteEvent("token", tokenEvent{Text: s})
        },
        OnUsage: func(u llm.TokenUsage) { usage = u },
    })

    latencyMs := int(time.Since(start).Milliseconds())
    ttftMs := latencyMs
    if !firstToken.IsZero() { ttftMs = int(firstToken.Sub(start).Milliseconds()) }

    if errors.Is(chatErr, context.Canceled) || errors.Is(c.Request.Context().Err(), context.Canceled) {
        _ = h.messages.UpdateAssistant(context.Background(), assistantMsgID, store.AssistantPatch{Status: "aborted"})
        return
    }
    if chatErr != nil {
        apiErr := classifyChatError(chatErr)
        code := apiErr.Code
        _ = h.messages.UpdateAssistant(context.Background(), assistantMsgID, store.AssistantPatch{Status: "error", ErrorCode: &code, LatencyMs: &latencyMs, TTFTMs: &ttftMs})
        _ = sw.WriteEvent("error", streamErrorEvent{Code: apiErr.Code, Message: apiErr.Message})
        return
    }

    inputTokens := usage.PromptTokens
    outputTokens := usage.CompletionTokens
    _ = h.messages.UpdateAssistant(context.Background(), assistantMsgID, store.AssistantPatch{Content: reply, Status: "ok", InputTokens: &inputTokens, OutputTokens: &outputTokens, TTFTMs: &ttftMs, LatencyMs: &latencyMs})
    _ = sw.WriteEvent("done", doneEvent{Usage: usageEvent{InputTokens: inputTokens, OutputTokens: outputTokens}, LatencyMs: latencyMs, TTFTMs: ttftMs})
}

func (h *Handlers) writeStreamError(ctx context.Context, sw *sse.Writer, msgID string, apiErr *APIError) {
    code := apiErr.Code
    _ = h.messages.UpdateAssistant(context.Background(), msgID, store.AssistantPatch{Status: "error", ErrorCode: &code})
    _ = sw.WriteEvent("error", streamErrorEvent{Code: apiErr.Code, Message: apiErr.Message})
}

func classifyChatError(err error) *APIError {
    if errors.Is(err, context.DeadlineExceeded) {
        return ErrModelTimeout
    }
    return ErrModelError.WithMessage(err.Error())
}
```

- [ ] **Step 5: Run Chat handler test**

Run:

```bash
go test ./internal/httpapi -run TestDispatchChatStreamsMetaTokenDone -count=1
```

Expected: PASS.

- [ ] **Step 6: Run all HTTP tests**

Run:

```bash
go test ./internal/httpapi/... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi
git commit -m "feat(httpapi): stream chat responses over SSE"
```

---

## Task 8: Add server cobra command and preserve CLI

**Files:**
- Modify: `cmd/agent.go`
- Create: `cmd/root.go`
- Create: `cmd/cli.go`
- Create: `cmd/server.go`
- Modify: `cmd/agent_test.go`
- Create: `cmd/server_test.go`

- [ ] **Step 1: Move root/cli declarations without logic change**

Refactor carefully:

- Keep `package main` everywhere under `cmd/`.
- Move `var configPath`, `rootCmd`, `cliCmd`, and `init()` command registration from `cmd/agent.go` to new `cmd/root.go` and `cmd/cli.go`.
- Keep `main()` in `cmd/agent.go`:

```go
func main() {
    if err := rootCmd.Execute(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

`cmd/cli.go` should contain the old `cliCmd`, `runCLI`, and CLI-only helpers (`cliConfirm`, `applyKnowledgeRetrieverStartup`, `applyStartupSuggestion`, `cliShadowPlannerInput`, planner wrappers).

- [ ] **Step 2: Run CLI regression tests after move**

Run:

```bash
go test ./cmd -count=1
```

Expected: PASS. If it fails, fix only refactor-induced missing imports/symbols.

- [ ] **Step 3: Write server test first**

Create `cmd/server_test.go`:

```go
package main

import (
    "testing"

    "github.com/compshare-agent/internal/config"
    "github.com/stretchr/testify/assert"
)

func TestValidateServerConfigRequiresMySQLDSN(t *testing.T) {
    cfg := &config.Config{}
    err := validateServerConfig(cfg)
    assert.Error(t, err)
}

func TestValidateServerConfigAcceptsRequiredFields(t *testing.T) {
    cfg := &config.Config{Agent: config.AgentConfig{
        MySQL: config.MySQLConfig{DSN: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"},
        Meta: config.MetaConfig{Welcome: "welcome", SuggestedPrompts: []string{"p"}, MaxInputLength: 4000},
        HTTP: config.HTTPConfig{MaxInputLength: 4000},
    }}
    err := validateServerConfig(cfg)
    assert.NoError(t, err)
}
```

- [ ] **Step 4: Implement server command**

Create `cmd/server.go`:

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/compshare-agent/internal/agentpool"
    "github.com/compshare-agent/internal/config"
    "github.com/compshare-agent/internal/httpapi"
    "github.com/compshare-agent/internal/store"
    "github.com/gin-gonic/gin"
    "github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
    Use:   "server",
    Short: "启动 HTTP 服务",
    RunE:  runServer,
}

func init() {
    serverCmd.Flags().String("addr", "", "覆盖配置的监听地址")
    rootCmd.AddCommand(serverCmd)
}

func runServer(cmd *cobra.Command, args []string) error {
    cfg, err := config.Load(configPath)
    if err != nil { return err }
    if addr, _ := cmd.Flags().GetString("addr"); addr != "" {
        cfg.Agent.HTTP.ListenAddr = addr
    }
    if err := validateServerConfig(cfg); err != nil { return err }

    db, err := store.OpenMySQL(cfg.Agent.MySQL)
    if err != nil { return err }
    defer db.Close()

    sessionStore := store.NewSessionStore(db)
    messageStore := store.NewMessageStore(db)
    feedbackStore := store.NewFeedbackStore(db)
    pool := agentpool.New(cfg, messageStore, agentpool.Options{Capacity: cfg.Agent.HTTP.PoolCapacity, IdleTTL: cfg.Agent.HTTP.PoolIdleTTL})
    defer pool.Close()

    handlers := httpapi.NewHandlers(cfg, sessionStore, messageStore, feedbackStore, pool)
    router := gin.New()
    router.Use(gin.CustomRecovery(func(c *gin.Context, recovered any) {
        c.JSON(http.StatusInternalServerError, httpapi.Response{Code: "InternalError", Message: fmt.Sprint(recovered), Data: nil})
    }))
    router.GET("/healthz", httpapi.Healthz)
    router.POST("/api/gateway", handlers.Dispatch)

    srv := &http.Server{Addr: cfg.Agent.HTTP.ListenAddr, Handler: router, ReadTimeout: cfg.Agent.HTTP.ReadTimeout, WriteTimeout: cfg.Agent.HTTP.WriteTimeout}
    return serveUntilSignal(srv)
}

func validateServerConfig(cfg *config.Config) error {
    if cfg.Agent.MySQL.DSN == "" { return fmt.Errorf("agent.mysql.dsn is required for server") }
    if cfg.Agent.Meta.Welcome == "" { return fmt.Errorf("agent.meta.welcome is required for server") }
    if len(cfg.Agent.Meta.SuggestedPrompts) == 0 { return fmt.Errorf("agent.meta.suggested_prompts is required for server") }
    if cfg.Agent.HTTP.MaxInputLength != cfg.Agent.Meta.MaxInputLength { return fmt.Errorf("agent.http.max_input_length must equal agent.meta.max_input_length") }
    return nil
}

func serveUntilSignal(srv *http.Server) error {
    errCh := make(chan error, 1)
    go func() {
        err := srv.ListenAndServe()
        if err != nil && !errors.Is(err, http.ErrServerClosed) { errCh <- err; return }
        errCh <- nil
    }()

    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    select {
    case err := <-errCh:
        return err
    case <-sigCh:
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        return srv.Shutdown(ctx)
    }
}
```

- [ ] **Step 5: Run cmd tests**

Run:

```bash
go test ./cmd -count=1
```

Expected: PASS.

- [ ] **Step 6: Build binary**

Run:

```bash
go build -o agent ./cmd
```

Expected: PASS and `agent` binary created (gitignored by `*.exe` only, so remove it after this step or add `agent` to `.gitignore` if needed).

If `agent` appears untracked, run:

```bash
rm ./agent
```

- [ ] **Step 7: Commit**

```bash
git add cmd
git commit -m "feat(cmd): add HTTP server command"
```

---

## Task 9: Update CLAUDE.md and verify full suite

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update CLAUDE.md**

Add this section to `CLAUDE.md`:

```md
## HTTP service

`compshare-agent server` runs the HTTP gateway alongside the CLI; both share the engine/knowledge/planner core.

- Entry: `cmd/server.go`. Routes: `POST /api/gateway` (Action-routed) + `GET /healthz`.
- Identity is taken from the request body (gateway-injected), not headers: `top_organization_id` / `organization_id` (uint32, snake_case) and `request_uuid` (string, snake_case, auto-generated if missing). Business fields stay PascalCase (`Action`, `SessionId`, `Message`).
- Phase-1 Actions: `GetSession` / `CreateSession` / `Chat` (SSE) / `GetMeta` / `Feedback`. `SessionId` is mandatory on every session-scoped Action; the frontend persists it in localStorage.
- Per-session `*engine.Engine` lives in `internal/agentpool` (LRU 200 / 30min idle). HTTP path skips `engine.Init()` and rehydrates history from MySQL via `engine.RehydrateHistory`.
- SSE stream is per-token end-to-end via `llm.ChatRequest.OnTextDelta` → `engine.ChatOptions.OnTextDelta` → `sse.Writer`. ReAct intermediate `StepEvent`s are not exposed in phase 1.
- Persistence: MySQL 8 via `database/sql + go-sql-driver/mysql`; schema in `deploy/migrations/0001_init.sql`. `messages` is INSERTed twice per turn (user immediately, assistant placeholder before LLM call) and UPDATEd once on SSE done — never per-token. DDL is run by ops, not the binary.
```

In the commands section, add:

```bash
go build -o agent ./cmd
./agent server --addr :8080
```

In the env/runtime table, add:

```md
| `MYSQL_DSN` | DSN string | Required by `compshare-agent server`; ignored by `compshare-agent cli`. |
```

- [ ] **Step 2: Run formatting**

Run:

```bash
gofmt -w cmd internal/config internal/store internal/agentpool internal/httpapi internal/engine internal/llm
```

Expected: no output.

- [ ] **Step 3: Run focused tests**

Run:

```bash
go test ./internal/config ./internal/store ./internal/agentpool ./internal/httpapi/... ./internal/engine ./internal/llm ./cmd -count=1
```

Expected: PASS.

- [ ] **Step 4: Run full suite**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Verify build**

Run:

```bash
go build -o agent ./cmd
```

Expected: PASS. Remove local binary if created:

```bash
rm ./agent
```

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: document HTTP service entrypoint"
```

---

## Task 10: Manual smoke test with local MySQL

**Files:**
- No code changes unless smoke reveals a bug.

- [ ] **Step 1: Prepare database**

Using a local MySQL 8 instance, run:

```bash
mysql -uroot -p -e 'CREATE DATABASE IF NOT EXISTS compshare_agent DEFAULT CHARSET utf8mb4;'
mysql -uroot -p compshare_agent < deploy/migrations/0001_init.sql
```

Expected: tables `sessions`, `messages`, and `message_feedback` exist.

- [ ] **Step 2: Start server**

Run with real env values:

```bash
export MYSQL_DSN='root:password@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Local&charset=utf8mb4'
export COMPSHARE_PUBLIC_KEY='...'
export COMPSHARE_PRIVATE_KEY='...'
export LLM_API_KEY='...'
./agent server --addr :8080
```

Expected: server listens on `:8080`.

- [ ] **Step 3: Health check**

Run:

```bash
curl -s http://127.0.0.1:8080/healthz
```

Expected:

```json
{"status":"ok"}
```

- [ ] **Step 4: GetMeta**

Run:

```bash
curl -s -X POST http://127.0.0.1:8080/api/gateway \
  -H 'Content-Type: application/json' \
  -d '{"Action":"GetMeta","top_organization_id":1,"organization_id":2}'
```

Expected: JSON with `Code=Success`, non-empty `RequestId`, and configured `Welcome`.

- [ ] **Step 5: CreateSession**

Run:

```bash
curl -s -X POST http://127.0.0.1:8080/api/gateway \
  -H 'Content-Type: application/json' \
  -d '{"Action":"CreateSession","Title":"smoke","top_organization_id":1,"organization_id":2}'
```

Expected: JSON with `Data.SessionId`. Save it as `SESSION_ID`.

- [ ] **Step 6: GetSession**

Run:

```bash
curl -s -X POST http://127.0.0.1:8080/api/gateway \
  -H 'Content-Type: application/json' \
  -d '{"Action":"GetSession","SessionId":"'"$SESSION_ID"'","top_organization_id":1,"organization_id":2}'
```

Expected: `Messages: []`.

- [ ] **Step 7: Chat SSE**

Run:

```bash
curl -N -X POST http://127.0.0.1:8080/api/gateway \
  -H 'Content-Type: application/json' \
  -d '{"Action":"Chat","SessionId":"'"$SESSION_ID"'","Message":"你好","request_uuid":"req-smoke-1","top_organization_id":1,"organization_id":2}'
```

Expected event order: `meta`, one or more `token`, then `done`. If LLM credentials are not valid, expect `meta` then `error` with `ModelError`, and DB assistant message status `error`.

- [ ] **Step 8: Verify messages persisted**

Run `GetSession` again.

Expected: two messages (`user`, `assistant`) for success or assistant `status=error` for model error.

- [ ] **Step 9: Feedback**

Use the assistant `MessageId` from `GetSession`, then run:

```bash
curl -s -X POST http://127.0.0.1:8080/api/gateway \
  -H 'Content-Type: application/json' \
  -d '{"Action":"Feedback","MessageId":"'"$MESSAGE_ID"'","Rating":"Up","top_organization_id":1,"organization_id":2}'
```

Expected: JSON with `Data.FeedbackId`.

- [ ] **Step 10: Final status**

Run:

```bash
git status --short
```

Expected: only intentional uncommitted files remain. Do not commit smoke-only local configs or binaries.

---

## Self-review checklist

- Spec coverage: Tasks cover config, MySQL schema/store, request identity, non-stream handlers, Chat SSE, per-session engine pool, cobra server, CLAUDE.md, verification, and smoke testing.
- Scope check: Second-stage session list/update/delete, STS credential fetching, OTel/Prometheus, and ReAct typed events are explicitly out of scope.
- Type consistency: Request identity uses `request_uuid`, `top_organization_id`, `organization_id`; response uses `RequestId`, `Code`, `Message`, `Data`; `SessionId` is mandatory for `GetSession` and `Chat`.
- TDD: Each implementation task starts with a focused failing test where practical, then implementation, verification, and commit.
