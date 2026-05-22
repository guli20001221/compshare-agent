# CompShare Agent HTTP API

面向前端接入的 HTTP 接口文档。所有业务接口走同一个网关入口，按请求体中的 `Action` 字段路由。

- Base URL（默认）：`http://<server-host>:8080`
- 业务入口：`POST /`（根路径直接打）
- 健康探测：`GET /healthz` → `{"status":"ok"}`
- Content-Type：`application/json`（也接受 `application/x-www-form-urlencoded`）

---

## 1. 公共字段（每个请求都带）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `Action` | string | **是** | 业务动作名，见下文每节标题 |
| `ProjectId` | string | **是** | 当前选中的项目 ID，形如 `org-cwy2qk`。由前端从项目下拉框透传 |
| `organization_id` | uint32 | 是* | 当前组织 ID（子组织 / 项目所属组织） |
| `top_organization_id` | uint32 | 是* | 顶级组织 ID（账户 ID） |
| `request_uuid` | string | 否 | 链路追踪 ID，缺省时服务端自动生成 UUIDv4。建议前端为每次请求生成一次并回写到日志，便于排查。也兼容 PascalCase 别名 `RequestId`（仅当 `request_uuid` 缺省时回退读取） |

> *`organization_id` 与 `top_organization_id` 正常由网关（控制台 BFF）从已登录会话中注入，前端**经网关访问时无需自己填**；如果绕过网关直连 agent（如本地联调），则前端必须显式填上。

### 通用响应包（非 SSE）

```json
{
  "RequestId": "req-uuid",
  "Code": "Success",
  "Message": "",
  "Data": { ... }
}
```

| 字段 | 说明 |
|---|---|
| `RequestId` | 等于请求里的 `request_uuid`（或服务端生成的那个） |
| `Code` | `Success` 或错误码（见下） |
| `Message` | 错误时的简短描述；成功时为 `""` |
| `Data` | 业务数据，结构因 Action 而异；失败时为 `null` |

### 错误码

HTTP 状态码与 `Code` 一一对应：

| HTTP | `Code` | 含义 |
|---|---|---|
| 400 | `InvalidParam` | 参数缺失或非法 |
| 401 | `Unauthorized` | 未登录 / token 失效 |
| 403 | `Forbidden` | 无权访问该会话 / 资源 |
| 404 | `NotFound` | 资源不存在（会话、消息、反馈） |
| 409 | `SessionTurnLimitExceeded` | 本会话轮数已达上限（默认 10 问答对），需新开 session |
| 429 | `RateLimited` | 触发限流（按 `(top_organization_id, organization_id)` 计） |
| 500 | `InternalError` | 后端未预期错误 |
| 502 | `ModelError` | LLM 上游错误 |
| 504 | `ModelTimeout` | LLM 调用超时 |
| —   | `Aborted` | 客户端主动断开（仅 SendCSAgentChat SSE 流，**没有对应的 HTTP 状态**：此时 SSE 响应已是 200，服务端只把对应 assistant 消息的 `Status` 置为 `aborted`，下次 `GetCSAgentSession` 才能看到） |

---

## 2. Action: `GetCSAgentMeta` — 获取元信息

启动会话前可调用一次，拿到欢迎语、推荐 Prompt、模型名、最大输入长度。

**请求体**：

```json
{
  "Action": "GetCSAgentMeta",
  "ProjectId": "org-cwy2qk"
}
```

**响应 Data**：

```json
{
  "Model": "deepseek-v4-flash",
  "Version": "0.1.0",
  "Welcome": "我是优云算力共享平台 AI 助手，可以问我控制台相关问题。",
  "SuggestedPrompts": ["我有哪些实例", "4090 现在有库存吗", "创建实例的操作步骤"],
  "MaxInputLength": 4000
}
```

> `MaxInputLength` 是单条 `Message` 允许的最大字符数（按 rune 计），前端应在输入框做相同校验。

---

## 3. Action: `CreateCSAgentSession` — 创建新会话

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `Title` | string | 否 | 会话标题，未传则为 `null` |
| `Context` | string | 否 | 自定义上下文，**JSON 编码后的字符串**（非对象），会原样存储以便后续追溯（当前不用于 prompt）。直接传对象会被忽略 |

```json
{
  "Action": "CreateCSAgentSession",
  "ProjectId": "org-cwy2qk",
  "Title": "查询我的实例"
}
```

**响应 Data**：

```json
{
  "SessionId": "9348b71e-7081-492d-99d9-650a34c120ef",
  "Title": "查询我的实例",
  "CreatedAt": "2026-05-21T17:01:38.572Z"
}
```

> `Title` 在创建时未传则返回 `null`（非空字符串）。
>
> 前端创建后应把 `SessionId` 持久化（localStorage / URL），后续每个 session-scoped Action 都要带它。

---

## 4. Action: `GetCSAgentSession` — 拉取会话元信息 + 历史消息

**请求体**：

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `SessionId` | string | **是** | — | 会话 ID |
| `Limit` | int | 否 | 50 | 1–100，单页消息数 |
| `Cursor` | string | 否 | — | 分页游标，由上一次响应的 `NextCursor` 提供 |

```json
{
  "Action": "GetCSAgentSession",
  "ProjectId": "org-cwy2qk",
  "SessionId": "9348b71e-7081-492d-99d9-650a34c120ef",
  "Limit": 50
}
```

**响应 Data**：

```json
{
  "SessionId": "9348b71e-7081-492d-99d9-650a34c120ef",
  "Title": "查询我的实例",
  "MessageCount": 12,
  "CreatedAt": "2026-05-21T17:01:38.572Z",
  "UpdatedAt": "2026-05-21T17:05:01.000Z",
  "Messages": [
    {
      "MessageId": "uuid",
      "Role": "user",
      "Content": "帮我看下我都开了哪些实例",
      "Status": "ok",
      "CreatedAt": "2026-05-21T17:01:39.000Z"
    },
    {
      "MessageId": "uuid",
      "Role": "assistant",
      "Content": "...",
      "Status": "ok",
      "CreatedAt": "2026-05-21T17:01:48.000Z"
    }
  ]
}
```

字段说明：

- `Title`：会话创建时未传则为 `null`。
- `Role`：`user` | `assistant`
- `Status`：`ok` | `pending`（assistant 占位，SSE 流进行中）| `error` | `aborted`
- `NextCursor`：非空字符串表示还有更早的消息，下一次请求把它原样回填到 `Cursor`；**已到顶时该字段会从 JSON 中省略**（不是返回 `""`），前端按"字段不存在或为空"判断即可。

---

## 5. Action: `SendCSAgentChat` — 流式对话（SSE）

**与其它 Action 的关键差异**：响应是 `text/event-stream`，不是 JSON 包。

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `SessionId` | string | **是** | 必须是当前 `(top_organization_id, organization_id)` 拥有的会话 |
| `Message` | string | **是** | 用户输入。长度上限 = `GetCSAgentMeta.MaxInputLength`（按 rune 计） |

> 单个 session 默认最多 10 个问答对（由 `agent.http.max_session_turns` 配置；含中断 / 报错的轮次）。超过后此接口返回 HTTP 409 `SessionTurnLimitExceeded`，前端需提示用户开新会话（调用 `CreateCSAgentSession`）。

```json
{
  "Action": "SendCSAgentChat",
  "ProjectId": "org-cwy2qk",
  "SessionId": "9348b71e-7081-492d-99d9-650a34c120ef",
  "Message": "帮我看下我都开了哪些实例"
}
```

**响应头**：

```
Content-Type: text/event-stream
Cache-Control: no-cache
X-Accel-Buffering: no
```

**事件流**：标准 SSE，每帧形如 `event: <name>\ndata: <json>\n\n`。事件先后顺序：

```
event: meta   ← 头一帧，必有
event: token  ← N 帧（每个 LLM 输出 token 一帧）
event: token
...
event: done   ← 成功收尾
```

或者：

```
event: meta
event: error  ← 任意时刻出现，表示终止
```

每种事件的 `data` 结构：

### `event: meta`

```json
{
  "RequestId": "req-uuid",
  "SessionId": "9348b71e-...",
  "MessageId": "6b4e5f12-..."   // 本轮 assistant 消息 ID，用于后续 SendCSAgentFeedback
}
```

> 前端拿到 `MessageId` 后就可以渲染一个"正在思考..."的占位气泡，token 到来时逐字拼接，结束后把这条气泡的 `Status` 标记为 `ok`。

### `event: token`

```json
{ "Text": "你好" }
```

直接追加到当前 assistant 气泡。Text 可能是单字、一个标点、一个空格、或半句 markdown（流式 token 不保证分词边界），前端不要做任何分词假设。

### `event: done`

```json
{
  "Usage": { "InputTokens": 4442, "OutputTokens": 198 },
  "LatencyMs": 8542,
  "TtftMs": 8542
}
```

| 字段 | 说明 |
|---|---|
| `Usage.InputTokens` | 本轮 prompt + 历史 + 工具 result 的总 token |
| `Usage.OutputTokens` | LLM 本轮输出 token |
| `LatencyMs` | 从开始到 `done` 的端到端耗时 |
| `TtftMs` | Time-to-first-token；若 LLM 一个 token 都没吐就出错收尾，等于 `LatencyMs` |

### `event: error`

```json
{ "Code": "ModelTimeout", "Message": "LLM 调用超时" }
```

`Code` 取值同表 §1。出现 `error` 后流即结束，前端把当前气泡 `Status` 设为 `error` 并展示 `Message`。

### keep-alive

服务端每隔 `sse_keepalive_interval`（默认 15s）发一个 SSE 注释帧（以 `:` 开头），用于保活 nginx / 浏览器。前端 EventSource 会自动忽略，不需要处理。

### 客户端中断

前端关闭连接（EventSource.close / 切路由 / 关 tab）后服务端会把当前 assistant 消息标记为 `aborted`，下次 `GetCSAgentSession` 会看到这条消息的 `Status = "aborted"`。

> 中断**不会**额外发 `event: error` 或改 HTTP 状态码（SSE 已经回 200），前端只能依赖自己的连接关闭事件和后续 `GetCSAgentSession` 校对。

---

## 6. Action: `SendCSAgentFeedback` — 对单条 assistant 消息点赞 / 点踩

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `MessageId` | string | **是** | 来自 `SendCSAgentChat` 的 `event: meta` 或 `GetCSAgentSession.Messages[].MessageId`，且必须是 `Role=assistant` 的消息 |
| `Rating` | string | **是** | `"Up"` 或 `"Down"`，其它值返回 `InvalidParam` |
| `Comment` | string | 否 | 用户补充评论，自由文本 |

```json
{
  "Action": "SendCSAgentFeedback",
  "ProjectId": "org-cwy2qk",
  "MessageId": "6b4e5f12-f994-429c-be9b-1d63948f986e",
  "Rating": "Down",
  "Comment": "工具调用返回了 IAM 错误"
}
```

**响应 Data**：

```json
{ "FeedbackId": "fb-uuid" }
```

> 同一 `MessageId` 可重复提交；后端会写入多条 feedback 记录而非覆盖。

---

## 7. 前端最小流程示例（伪代码）

```ts
// 1. 启动时拉元信息
const meta = await post('/', {
  Action: 'GetCSAgentMeta',
  ProjectId,
});

// 2. 首次进入会话页：localStorage 没有 sessionId 就创建
let sessionId = localStorage.getItem('agent.sessionId');
if (!sessionId) {
  const r = await post('/', {
    Action: 'CreateCSAgentSession',
    ProjectId,
  });
  sessionId = r.Data.SessionId;
  localStorage.setItem('agent.sessionId', sessionId);
}

// 3. 进入会话页：加载历史
const history = await post('/', {
  Action: 'GetCSAgentSession',
  ProjectId,
  SessionId: sessionId,
  Limit: 50,
});

// 4. 用户发问：用 fetch + ReadableStream 读 SSE
//    （EventSource 不支持 POST，必须用 fetch）
const resp = await fetch('/', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', 'Accept': 'text/event-stream' },
  body: JSON.stringify({
    Action: 'SendCSAgentChat',
    ProjectId,
    SessionId: sessionId,
    Message: userInput,
  }),
});

let assistantMessageId = '';
const reader = resp.body!.getReader();
const decoder = new TextDecoder();
let buf = '';
while (true) {
  const { value, done } = await reader.read();
  if (done) break;
  buf += decoder.decode(value, { stream: true });

  // 按 \n\n 切帧
  let idx;
  while ((idx = buf.indexOf('\n\n')) !== -1) {
    const frame = buf.slice(0, idx);
    buf = buf.slice(idx + 2);
    const ev = parseSSEFrame(frame); // { event, data }
    if (!ev) continue;
    switch (ev.event) {
      case 'meta':  assistantMessageId = ev.data.MessageId; break;
      case 'token': appendToBubble(assistantMessageId, ev.data.Text); break;
      case 'done':  finalizeBubble(assistantMessageId, ev.data); break;
      case 'error': errorBubble(assistantMessageId, ev.data); break;
    }
  }
}

// 5. 用户点踩
await post('/', {
  Action: 'SendCSAgentFeedback',
  ProjectId,
  MessageId: assistantMessageId,
  Rating: 'Down',
  Comment: '回答没用',
});
```

> 注意 SSE 帧解析必须按 `\n\n` 切帧，不能按行处理（一帧里有两行：`event:` 和 `data:`）。

---

## 8. 联调常见错误

| 现象 | 可能原因 |
|---|---|
| `InvalidParam: missing top_organization_id` | 没经网关直连时前端没填这俩字段（参考 §1 表脚注） |
| `InvalidParam: missing SessionId` | SendCSAgentChat / GetCSAgentSession / SendCSAgentFeedback 漏带 `SessionId` |
| `InvalidParam: Message exceeds MaxInputLength` | 输入超长，提示用户裁剪。上限值通过 `GetCSAgentMeta` 拿 |
| `NotFound`（GetCSAgentSession） | `SessionId` 不存在 / 已删除 / 属于另一个组织 |
| SendCSAgentChat 流里 LLM 回答 `IAM 权限错误` 之类业务错 | 这是工具调用失败的**业务文案**，不是 HTTP 错误，对应的 SSE 流仍会正常 `done`。需要排查角色策略而不是网关接入 |
| 长时间没收到 token，但连接没断 | 后端正在做工具调用 + 二次 LLM 调用；只要 keep-alive 帧还在，就不要主动 close |
