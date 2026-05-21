# Console AI Assistant — Go 后端第一阶段设计

> 创建日期: 2026-05-21
> 状态: Draft
> 配套上游 spec: `~/design/console-ai/docs/superpowers/specs/2026-05-21-console-ai-assistant-api.md`
> 配套上游设计: `~/design/console-ai/docs/superpowers/specs/2026-05-21-console-ai-assistant-design.md`

## 1. 范围

把现有 CLI 形式的 `compshare-agent` 改造为 HTTP 服务，第一阶段实现 spec §1 的 5 个 Action + `/healthz`：

- `GetMeta`
- `CreateSession`
- `GetSession`
- `Chat`(SSE)
- `Feedback`

CLI 入口 `compshare-agent cli` 保留不动，HTTP 入口新增 `compshare-agent server` 子命令；engine/knowledge/planner/prompt/tools 等核心包**复用**，仅在历史来源（CLI 进程内单例 vs HTTP 每会话池）上分叉。

### 与上游 spec 的偏离点（一处）

spec §1.2 写"第一阶段 GetSession 不传 SessionId 返回当前用户的当前会话 + 懒创建"。本设计**改为**：

> 前端必须自己持有 SessionId（localStorage 缓存），每次 `GetSession` / `Chat` / `Feedback` 都带上。

理由：去掉后端的"当前会话"指针后 schema 简化、查询更便宜、第二阶段无破坏性扩展。`CreateSession` 仍是首次会话/"新建对话"的入口。前端 localStorage 失效时（无痕 / 换浏览器 / 清缓存）自行先调 `CreateSession`。

## 2. 总体结构

```
cmd/
  root.go             rootCmd + 全局 --config（沿用现有）
  cli.go              cliCmd（搬自现有 cmd/agent.go 的 runCLI）
  server.go           serverCmd（新增）
  trace.go            既有；CLI 路径用
  agent.go            仅保留 main() + rootCmd

internal/
  httpapi/
    dispatch.go       POST /api/gateway 分发 + 响应包装 {RequestId, Code, Message, Data}
    baserequest.go    request_uuid / top_organization_id / organization_id 解析与校验
    middleware/
      identity.go     gin 中间件：补 request_uuid，校验 owner 二元组
      access_log.go   每条访问日志带 request_uuid
      recovery.go     panic → InternalError + stack 进日志
    sse/
      writer.go       SSE 帧格式 / keepalive / mutex
    handlers/
      meta.go
      session.go      Create / Get
      chat.go         SSE
      feedback.go
    healthz.go
    errors.go         APIError + 归一化映射

  store/
    mysql.go          OpenMySQL + ping + schema 自检
    sessions.go       SessionStore 接口 + mysql 实现
    messages.go       MessageStore 接口 + mysql 实现
    feedback.go       FeedbackStore 接口 + mysql 实现
    cursor.go         (created_at, id) 二元组 base64 编解码

  agentpool/
    pool.go           sessionId → *engine.Engine 的并发安全 LRU
    rehydrate.go      miss 时从 MessageStore 拉历史灌进 engine

  engine/             既有；新增 RehydrateHistory + ChatWithOptions
  llm/                既有；ChatRequest 新增 OnTextDelta 回调

deploy/
  conf/agent.yaml.example   新增 http / mysql / meta 三节
  migrations/0001_init.sql  三表 + 索引

docs/superpowers/specs/2026-05-21-console-ai-assistant-go-phase1-design.md  本文件
```

## 3. 请求 / 响应协议

### 3.1 字段命名

业务字段（来自上游 spec）保留 PascalCase；身份/trace 字段（网关注入，参考 `uhost-compshare-api/pkg/api/base.go`）保留 snake_case：

```json
{
  "Action": "Chat",
  "SessionId": "sess-xxx",
  "Message": "...",
  "request_uuid": "req-xxx",
  "top_organization_id": 12345,
  "organization_id": 67890
}
```

`top_organization_id` 和 `organization_id` 类型为 `uint32`（与参考项目一致）。

### 3.2 响应（非 SSE）

按上游 spec §0.3，PascalCase：

```json
{
  "RequestId": "req-xxx",
  "Code": "Success",
  "Message": "",
  "Data": { ... }
}
```

请求体里的 `request_uuid` 在响应里转写为 `RequestId`。HTTP status 严格按 spec §0.5 表（不全用 200）。

### 3.3 SSE 响应（仅 Chat）

```
event: meta
data: {"RequestId":"...","SessionId":"...","MessageId":"..."}

event: token
data: {"Text":"..."}

event: done
data: {"Usage":{"InputTokens":12,"OutputTokens":34},"LatencyMs":1820,"TtftMs":340}

event: error
data: {"Code":"...","Message":"..."}
```

服务端每 15s 写一行 `:keepalive\n\n` 维持链路。

### 3.4 错误码

`internal/httpapi/errors.go`：

```go
type APIError struct {
    Code    string
    Status  int
    Message string
}

var (
    ErrInvalidParam   = &APIError{Code: "InvalidParam",   Status: 400}
    ErrUnauthorized   = &APIError{Code: "Unauthorized",   Status: 401}
    ErrForbidden      = &APIError{Code: "Forbidden",      Status: 403}
    ErrNotFound       = &APIError{Code: "NotFound",       Status: 404}
    ErrRateLimited    = &APIError{Code: "RateLimited",    Status: 429}
    ErrInternal       = &APIError{Code: "InternalError",  Status: 500}
    ErrModelTimeout   = &APIError{Code: "ModelTimeout",   Status: 504}
    ErrModelError     = &APIError{Code: "ModelError",     Status: 502}
    ErrAborted        = &APIError{Code: "Aborted",        Status: 499}
)
```

归一化映射：

| 来源 | 映射 |
|---|---|
| middleware 校验：缺 `top_organization_id` / `organization_id` / Action 不在白名单 | `InvalidParam` |
| `SessionStore.GetByID` 返回 `sql.ErrNoRows` | `NotFound` |
| `messages.session_id` 不属于 owner | `NotFound`（不返回 Forbidden，避免泄露 sessionId 存在性）|
| `governance/ratelimit` 拒绝 | `RateLimited` |
| `ctx.DeadlineExceeded` 在 LLM 路径 | `ModelTimeout` |
| `c.Request.Context().Done()` 由客户端关连接触发 | `Aborted`（仅记录，不发响应）|
| LLM client 4xx/5xx | `ModelError` |
| 启动期 yaml/db 失败 | `log.Fatal`，进程不起 |
| panic / 其它未预期 | `InternalError` + recover 中间件记 stack |

业务 handler 不允许直接 `c.JSON(...)`，统一抛 error，由 dispatch 包装。SSE 路径已开始流之后的错误改写 `event:error` 帧；`Aborted` 例外，仅落库不发响应。

## 4. 持久化设计

### 4.1 选型

- MySQL 8.0+
- driver: `database/sql` + `github.com/go-sql-driver/mysql`
- 不引入 sqlc / sqlx
- DDL 由运维跑 `deploy/migrations/0001_init.sql`，程序只做存在性自检（`SELECT 1 ... LIMIT 1`），缺表立即 `log.Fatal`

### 4.2 表结构

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

`pinned` / `deleted_at` 为第二阶段保留，第一阶段不暴露对应 handler。

### 4.3 Store 接口

```go
type Owner struct {
    TopOrganizationID uint32
    OrganizationID    uint32
}

type SessionStore interface {
    Create(ctx context.Context, owner Owner, title *string, ctxJSON json.RawMessage) (Session, error)
    GetByID(ctx context.Context, owner Owner, sessionID string) (Session, error)  // 越权返回 NotFound
    BumpUpdatedAtAndIncCount(ctx context.Context, sessionID string, delta int) error
}

type MessageStore interface {
    Append(ctx context.Context, m Message) error
    UpdateAssistant(ctx context.Context, msgID string, patch AssistantPatch) error
    ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error)
    GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error)  // Feedback 用
}

type FeedbackStore interface {
    Insert(ctx context.Context, msgID, rating, comment string) (string, error)
}
```

### 4.4 Cursor

`(created_at, id)` 二元组 base64：

```go
cursor = base64({"created_at": "2026-05-21T10:23:48.123Z", "id": "msg-aaa"})
```

SQL 翻页：`WHERE session_id = ? AND (created_at, id) > (?, ?) ORDER BY created_at ASC, id ASC LIMIT ?`。同毫秒撞键时按 id 决断，不丢消息。

### 4.5 写入时机

```
用户发送 Chat
  ↓
tx 事务:
  INSERT messages (id=userMsgID,      role=user,      content=Message, status=ok,      ...)
  INSERT messages (id=assistantMsgID, role=assistant, content="",      status=pending, model=cfg.LLM.Model, ...)
  UPDATE sessions SET message_count = message_count + 2, updated_at = NOW(3)
  ↓
[SSE 开流，逐 token 推送]
  ↓
SSE 流结束 / 出错 / 用户中断
  ↓
UPDATE messages
  SET content=?, status=?, error_code=?, input_tokens=?, output_tokens=?, ttft_ms=?, latency_ms=?
  WHERE id = assistantMsgID
```

**禁止**每个 token UPDATE。

容错：
- INSERT 失败：直接返回 500，**不开 SSE**。
- UPDATE 失败：已经把 reply 流完了，仅记 ERROR 日志，SSE 仍然发 `done`（前端体验优先）。

## 5. Identity / RequestUUID 中间件

### 5.1 行为

`internal/httpapi/middleware/identity.go`：

1. `parseRequest` 读 raw bytes（form 或 JSON 二选一），统一解到 `simplejson.Json`。
2. `BaseRequest{ Action, RequestUUID, TopOrganizationID(uint32), OrganizationID(uint32) }` 解析，参考 `uhost-compshare-api/pkg/api/base.go::NewBaseRequest`。
3. `request_uuid` 缺失时 `uuid.NewString()` 生成并写回 simplejson，让下游和响应共用同一个值。
4. `top_organization_id == 0` 或 `organization_id == 0` → `InvalidParam`。
5. `BaseRequest` 和 simplejson raw 都塞进 `gin.Context`，handler 通过 helper 取，避免散在各处用字符串 key。
6. `/healthz` 不走该中间件。

### 5.2 入口路由

```
POST /api/gateway   →  middleware: parseRequest + identity → dispatch
GET  /healthz       →  返回 200 {"status":"ok"}
```

## 6. Engine 复用 / agentpool

### 6.1 改造点

```go
// internal/llm/client.go
type ChatRequest struct {
    Messages    []openai.ChatCompletionMessage
    Tools       []openai.Tool
    ToolChoice  any
    OnTextDelta func(string)   // 新增：每次 delta.Content != "" 时回调；nil 时行为不变
}

// internal/engine/engine.go
type ChatOptions struct {
    OnTextDelta func(string)        // 仅最后一轮 LLM 文本输出的 chunk 透传
    OnUsage     func(llm.TokenUsage)
}

func (e *Engine) ChatWithOptions(ctx context.Context, userMsg string, onStep func(StepEvent), opts ChatOptions) (string, error)
func (e *Engine) RehydrateHistory(msgs []HistoryMessage)

type HistoryMessage struct {
    Role    string  // "user" / "assistant"
    Content string
}

// 旧的 Chat(ctx, userMsg, onStep) 保留作 CLI 入口，内部委托 ChatWithOptions
```

### 6.2 agentpool 结构

```
internal/agentpool/
  pool.go        sessionId → *engine.Engine 的并发安全 LRU(cap=200, idle=30min)
  rehydrate.go   miss 时调 MessageStore.ListBySession 拉历史，调 RehydrateHistory
```

LRU 用 `container/list` + `sync.Mutex` 手写（约 80 行），不引第三方。每个 entry 自带 `lastTouchedAt`，gc goroutine 30 秒扫一次按 idle 阈值清。被 evict 的 engine 没有未落库状态（messages 一回合结束就同步写 DB），直接丢弃；下一次 miss 时 rehydrate 重建。

### 6.3 HTTP 路径与 CLI 路径的差异

| 行为 | CLI | HTTP |
|---|---|---|
| `engine.Init()` | 调（拉欢迎语 / suggestions / 实例预热）| **不调** |
| 历史来源 | 进程内单例累积 | 每次 miss 从 MessageStore rehydrate |
| confirm 函数 | stdin 交互 | `func(...) bool { return false }` 永远拒绝 L1 |
| 启动 suggestions | `prompt.Suggestion[]` 展示 | 由 `GetMeta.SuggestedPrompts` 配置返回 |
| trace JSONL | 现有逻辑保留 | 第一阶段不接（仅日志 + DB 字段）|

### 6.4 ReAct 中间事件

第一阶段**不暴露**给前端。`StepEvent` 仅写后端日志。SSE 只发 `meta` / `token` / `done` / `error`。后续要加 typed event（`tool_call` / `thinking` / `citation`）时按 spec §0.4 设计 ——前端按 type 路由，不破坏现有协议。

## 7. Action handler 行为

### 7.1 GetMeta

无业务参数。返回 `agent.meta` 配置快照（启动时加载，不每请求读 yaml）。

```json
Data: {
  "Model":            "deepseek-v4-flash",
  "Version":          "0.1.0",
  "Welcome":          "...",
  "SuggestedPrompts": ["...", "..."],
  "MaxInputLength":   4000
}
```

### 7.2 CreateSession

```
req: { Title?: string, Context?: string(JSON) }
```

- `Context` 非空时校验是合法 JSON，否则 `InvalidParam`。
- `INSERT sessions (id=uuid, top_org, org, title, context, message_count=0, ...)`。
- 不预热 agentpool。

```json
Data: {"SessionId": "...", "Title": null, "CreatedAt": "..."}
```

### 7.3 GetSession

```
req: { SessionId: string, Limit?: int=50 (1..100), Cursor?: string }
```

- `SessionStore.GetByID(owner, sessionID)`：用 `(top_org, org, id)` 三元组过滤。
- `MessageStore.ListBySession`：升序按 `created_at` + `id` 翻页。
- `Limit` 越界 → `InvalidParam`。
- 越权 / 不存在 → `NotFound`。

```json
Data: {
  "SessionId": "...", "Title": "...", "MessageCount": 6,
  "CreatedAt": "...", "UpdatedAt": "...",
  "Messages": [{"MessageId": "...", "Role": "user|assistant", "Content": "...", "Status": "ok|error|aborted|pending", "CreatedAt": "..."}, ...],
  "NextCursor": "..."
}
```

到底（无更早历史）时省略 `NextCursor`。

### 7.4 Chat（SSE）

```
req: { SessionId: string, Message: string, Context?: string(JSON), RequestId: string }
```

`RequestId` 在中间件层从 `request_uuid` / `RequestId` 任一取，缺失时自动生成，所以到 handler 时一定非空。

**校验**：
- `SessionId` 属于 owner（否则 `NotFound`）。
- `Message` 长度 ≤ `cfg.HTTP.MaxInputLength`（=`cfg.Meta.MaxInputLength`），否则 `InvalidParam`。

**串行流程**：

```
1. session := SessionStore.GetByID(owner, sessionID)
2. tx 事务:
     INSERT messages(userMsgID, role=user, content=Message, status=ok, request_uuid=RequestId)
     INSERT messages(assistantMsgID, role=assistant, content="", status=pending, model=cfg.LLM.Model, request_uuid=RequestId)
     UPDATE sessions SET message_count = message_count + 2, updated_at = NOW(3)
3. 写 SSE header:
     Content-Type: text/event-stream
     Cache-Control: no-cache
     X-Accel-Buffering: no
4. flush event:meta {RequestId, SessionId, MessageId=assistantMsgID}
5. agent := agentpool.Get(sessionID)   // miss 触发 rehydrate
6. 启动 keepalive ticker（15s）
   ttftStart := now()
   var firstTokenAt time.Time
   onTextDelta := func(s) {
     if firstTokenAt.IsZero() { firstTokenAt = now() }
     flush event:token {Text: s}
   }
7. reply, err := agent.ChatWithOptions(ctx, Message, nil, ChatOptions{
        OnTextDelta: onTextDelta,
        OnUsage: func(u) { usage = u },
   })
8. 流结束分支:
     成功:
       UPDATE messages SET content=reply, status=ok, input_tokens=..., output_tokens=...,
                           ttft_ms=firstTokenAt-ttftStart,
                           latency_ms=now()-ttftStart
       flush event:done {Usage, LatencyMs, TtftMs}
     LLM 错误 / 超时 / RateLimit:
       UPDATE messages SET status=error, error_code=Code
       flush event:error {Code, Message}
     ctx 被客户端取消 (Aborted):
       UPDATE messages SET status=aborted
       关闭连接，**不发** event:error
9. 关闭连接
```

**`reply` vs token stream**：`agent.ChatWithOptions` 返回的 `reply` 是完整文本，已经通过 `OnTextDelta` 全部流出去过；最终落库用 `reply`（权威值）。

**SSE writer**：抽出 `internal/httpapi/sse/writer.go`：

```go
type Writer struct {
    w   gin.ResponseWriter
    f   http.Flusher
    mu  sync.Mutex
}
func (s *Writer) WriteEvent(event string, data any) error
func (s *Writer) WriteKeepalive() error
```

避免 handler 直接拼字符串。

### 7.5 Feedback

```
req: { MessageId: string, Rating: "Up"|"Down", Comment?: string }
```

- `Rating` 只接受 `Up` / `Down`，否则 `InvalidParam`。
- `MessageStore.GetWithOwnerCheck(owner, msgID)`：JOIN `sessions` 校验属于当前 owner。

```sql
SELECT m.id FROM messages m JOIN sessions s ON s.id = m.session_id
WHERE m.id = ? AND s.top_organization_id = ? AND s.organization_id = ?
```

- 不存在 / 越权 → `NotFound`。
- `INSERT message_feedback`，返回 `feedbackId`。同一 message 允许多条 feedback（不去重）。

```json
Data: {"FeedbackId": "..."}
```

## 8. 配置 / 启动 / 部署

### 8.1 agent.yaml 新增节

```yaml
agent:
  # 既有节保留 ...

  http:
    listen_addr: "0.0.0.0:8080"
    read_timeout: "30s"
    write_timeout: "0s"           # SSE 必须 0
    sse_keepalive_interval: "15s"
    max_input_length: 4000        # 与 meta.max_input_length 一致
    pool_capacity: 200
    pool_idle_ttl: "30m"

  mysql:
    dsn: "${MYSQL_DSN}"           # user:pass@tcp(host:3306)/db?parseTime=true&loc=Local&charset=utf8mb4
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

`config.Load()` 校验：
- `http.max_input_length == meta.max_input_length`，否则 fatal。
- `server` 子命令启动时 `mysql.dsn` 必填；`cli` 子命令不校验 mysql 节。
- `meta.welcome` / `meta.suggested_prompts` 缺失 fatal（不给静默默认值）。

### 8.2 cobra 子命令拆分

```
cmd/
  root.go     rootCmd + 全局 --config
  cli.go      cliCmd（搬自 cmd/agent.go::runCLI）
  server.go   serverCmd（新增）
  agent.go    main() + rootCmd 定义；runCLI 已搬走
  trace.go    既有，保留给 cli 用
```

`server` 子命令：

```go
var serverCmd = &cobra.Command{
    Use:   "server",
    Short: "启动 HTTP 服务",
    RunE:  runServer,
}

func runServer(cmd *cobra.Command, args []string) error {
    cfg, err := config.Load(configPath)
    // --addr 覆盖
    db, err := store.OpenMySQL(cfg.Agent.MySQL)
    sessionStore := store.NewSessionStore(db)
    messageStore := store.NewMessageStore(db)
    feedbackStore := store.NewFeedbackStore(db)
    pool := agentpool.New(cfg, sessionStore, messageStore, agentpool.Options{...})
    handlers := httpapi.NewHandlers(cfg, sessionStore, messageStore, feedbackStore, pool)

    e := gin.New()
    e.Use(gin.CustomRecovery(httpapi.RecoveryHandler), httpapi.AccessLogMiddleware())
    e.GET("/healthz", httpapi.Healthz)
    e.POST("/api/gateway", handlers.Dispatch)

    srv := &http.Server{
        Addr:         cfg.Agent.HTTP.ListenAddr,
        Handler:      e,
        ReadTimeout:  cfg.Agent.HTTP.ReadTimeout,
        WriteTimeout: cfg.Agent.HTTP.WriteTimeout,    // 0 = 不超时
    }
    // graceful shutdown：srv.Shutdown(30s) → pool.Close() → db.Close()
}
```

### 8.3 启动期自检

1. `config.Load(path)` —— yaml + ENV 替换
2. `store.OpenMySQL(cfg.MySQL)` —— 连一次 + ping，失败 fatal
3. `verifySchema(db)` —— `SELECT 1 FROM sessions/messages/message_feedback LIMIT 1`，缺表 fatal
4. `engine.New` 一份做配置自检（LLM 客户端能否构造），失败 fatal
5. 起 HTTP server

不做 auto-migrate；DDL 由运维跑 `deploy/migrations/0001_init.sql`。

### 8.4 优雅关闭

`SIGINT` / `SIGTERM`：
1. `srv.Shutdown(ctx, 30s)` —— gin 等所有 in-flight 请求结束（含 SSE 流）
2. `pool.Close()` —— 等待所有 *Engine 释放
3. `db.Close()`

SSE 流在 shutdown 期间因 `ctx.Done()` 走 abort 分支，落 `status=aborted`。

### 8.5 .env.example

新增 git tracked `.env.example`：

```
COMPSHARE_PUBLIC_KEY=
COMPSHARE_PRIVATE_KEY=
LLM_API_KEY=
MYSQL_DSN=user:pass@tcp(127.0.0.1:3306)/compshare_agent?parseTime=true&loc=Local&charset=utf8mb4
```

## 9. 依赖变更

### 9.1 go.mod 新增

```
github.com/gin-gonic/gin
github.com/go-sql-driver/mysql
github.com/google/uuid
github.com/bitly/go-simplejson
```

集成测试（独立 build tag）依赖：

```
github.com/ory/dockertest/v3
```

### 9.2 现有 CLI 路径影响

零破坏：
- `engine.Chat(ctx, msg, onStep)` 旧签名保留。
- `llm.ChatRequest.OnTextDelta` 是新增可选字段，nil 时行为不变。
- `cmd/agent_test.go` 既有 4 个 test 不动。
- `eval/golden_test.go::TestGoldenScripts` 必须仍 PASS。
- `deploy/conf/agent.yaml` 既有节不变；CLI 不读新增的 http/mysql/meta 节。

## 10. 测试策略

### 10.1 单元测试

| 包 | 关注点 |
|---|---|
| `internal/httpapi/baserequest_test.go` | form/JSON 入参解析；缺字段错误码；request_uuid 自动补 UUID |
| `internal/httpapi/handlers/*_test.go` | 用 `httptest.NewRecorder` + mock store 覆盖每个 Action 成功/4xx 路径；`GetSession` cursor 翻页；`Feedback` 越权 NotFound |
| `internal/httpapi/sse/writer_test.go` | event/data 帧格式；keepalive 帧；并发写 mutex |
| `internal/agentpool/pool_test.go` | LRU eviction、idle TTL、并发 Get；rehydrate 用 mock store |
| `internal/engine/engine_test.go` 增补 | RehydrateHistory + ChatWithOptions：rehydrate 后历史对得上、OnTextDelta 在最后一轮收到 chunk |
| `internal/llm/client_test.go` 增补 | OnTextDelta 调用次数和顺序 |

### 10.2 集成测试

`internal/httpapi/integration_test.go`，build tag `integration`：

- `dockertest` 起 MySQL 8 容器
- 起完整 gin server（`engine.New` 用 mock LLM client）
- 端到端：CreateSession → Chat (SSE) → GetSession 看到两条消息 → Feedback 成功 → 越权 Feedback → NotFound
- 用 `bufio.Scanner` 解 SSE 帧，断言 `meta` → `token`* → `done` 顺序
- 默认 `go test ./...` 不跑（不依赖 docker），CI 单独 job 跑 `go test -tags=integration ./internal/httpapi/...`

### 10.3 回归

- CLI 路径：`eval/golden_test.go::TestGoldenScripts` 不变。
- `cmd/agent_test.go` 4 个 test 不变。
- 新增：`go test ./internal/httpapi/... ./internal/store/... ./internal/agentpool/...` 必须 PASS。

## 11. 第一阶段不做（明确范围之外）

| 项目 | 何时做 |
|---|---|
| 多会话 spec §2：`ListSessions` / `UpdateSession` / `DeleteSession` | 第二阶段；schema 字段 `pinned` / `deleted_at` 已留 |
| CompShare 凭证 STS 真实拉取（按 organization_id 拿用户临时凭证）| 第二阶段；当前所有 engine 用 `agent.yaml` 服务账号凭证（占位） |
| ReAct 中间事件透传（typed `tool_call` / `thinking` / `citation`）| 后续；spec §0.4 已为此设计 typed event 协议 |
| Prometheus / OpenTelemetry exporter | 第二阶段；第一阶段日志带 `request_uuid` 前缀即可 |
| OLAP / 数据导出 | 当 spec design §4.6 触发条件满足时 |
| L1 变更确认（HTTP 路径上的 confirm）| 不做；HTTP path 永远拒绝；CLI path 仍走 stdin |

## 12. CLAUDE.md 改动

加 "HTTP service" 节，简述：
- `compshare-agent server` 入口；`POST /api/gateway` (Action-routed) + `GET /healthz`
- 身份字段命名约定（`top_organization_id` / `organization_id` snake_case，`Action` / `SessionId` PascalCase）
- 第一阶段 5 个 Action；SessionId 强制前端持有
- agentpool LRU(200/30min)，HTTP 路径不调 `engine.Init()`
- SSE 全链路 per-token；ReAct 中间事件不暴露
- MySQL via `database/sql + go-sql-driver/mysql`，DDL 运维跑
- 集成测试 `-tags=integration` 默认不跑

env vars 表新增 `MYSQL_DSN`：仅 `server` 子命令需要。
