# "人工" 关键词触发官方客服二维码 — 实现记录

> 日期：2026-06-29
> 分支：main
> 关联约定：`internal/engine/preblock.go` 的 4 步加规则流程；`internal/refusal/templates.go` byte-stable 约定

## 1. 需求

当用户对话消息中出现"转人工"意图时，助手直接返回一段固定回复，内含官方客服二维码图片，跳过 LLM/ReAct。原始需求描述为"对话中出现'人工'两个字"。

## 2. 设计决策（已与用户确认）

| 决策点 | 选择 | 理由 |
|---|---|---|
| 二维码 URL | `https://ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png` | 自托管对象存储（UFile 乌兰察布）——自家基础设施、无外部 CDN 依赖、无图片处理 query（`iopcmd=convert&dst=webp` 已去掉），稳定可控。 |
| 匹配范围 | **方案 C：窄白名单短语** — `转人工` / `转接人工` / `人工客服` / `联系人工` / `找人工` / `叫人工` | 严格按"出现'人工'即触发"会误伤"人工智能 / 人工费 / 人工成本"。窄白名单只命中明确转人工意图（`转接人工` 因不含 `转人工` 子串需单列）。 |
| URL 存放 | **方案 a：硬编码常量** | 和现有 4 条 refusal 模板同处 `internal/refusal/templates.go`，byte-stable、单一来源、改 URL 改一行；无需新增 config/wire-protocol/DB 改动。 |
| 输出载体 | 二维码以 markdown 图片 `![客服二维码](URL)` 内嵌进 `engine.Chat()` 的字符串回复 | `Chat()` 返回 `(string, error)`，无 attachments 字段。HTTP/WS 路径把整条字符串透传给前端（前端按 markdown 渲染图片）；CLI 按字面文本打印。 |

## 3. 集成点选择

本仓 `engine.Chat()` 头部已有一条 **pre-LLM 关键词硬阻断链** `enginePreBlock`（`internal/engine/preblock.go`），现有两条规则：jailbreak、off-topic。命中即：
1. 触发 `hardBlockObserver` trace（`triggered_by=keyword` + 规则的 `Category`）；
2. 追加 `user` + `assistant` 消息到历史；
3. `return decision.Reply, nil` —— 完全跳过 LLM/ReAct。

这条链与本需求同构（关键词 → 固定 canned reply → 跳过 LLM），且 `preblock.go:22-33` 明确给出"加规则 4 步流程"。因此新规则作为 `enginePreBlock` 的第三条注册，是最小、最符合约定的实现路径。

调用点 `internal/engine/engine.go:1254-1272`（`enginePreBlock.Decide(userMsg)`）无需改动 —— 它对任意新规则自动生效，且在 OCR 图片上下文注入**之前**执行（避免截图 UI 标签误触发，见 engine.go:1251-1253 注释）。

## 4. 涉及文件

| 文件 | 改动 |
|---|---|
| `internal/refusal/templates.go` | +`CategoryHumanAgent` 常量；+`HumanAgentTransfer` 文案常量（含 QR markdown 图片） |
| `internal/engine/engine.go` | +`humanAgentTransferKeywords` 切片；+`isHumanAgentTransferRequest` 谓词；优先级链注释块加一段说明 |
| `internal/engine/preblock.go` | `enginePreBlock` 追加第三条 `inputguard.Rule`（放链尾） |
| `internal/engine/preblock_humanagent_test.go` | 新增表驱动测试（正/负例 + 顺序不变量 + URL byte-stability） |

**无** wire-protocol / DB schema / config.yaml / 前端 改动。

## 5. 实现过程

### 5.1 refusal category + 回复文案（`internal/refusal/templates.go`）

在 category 常量块追加 `CategoryHumanAgent = "human_agent_transfer"`（该字符串即 trace 里 `engine_hard_block.category` 的值，下游 MySQL trace ingest 按它 pivot，属稳定契约）。

在 `OffTopic` 之后追加 `HumanAgentTransfer` 常量，文案末尾内嵌 markdown 图片：

```go
const HumanAgentTransfer = "好的，已为您转接人工客服。请扫描下方二维码添加客服微信，会有专人为您服务。\n\n![客服二维码](https://ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png)"
```

URL 逐字节落入常量，满足 `templates.go` 顶部 byte-stable 要求（eval 脚本可字节比对）。

### 5.2 关键词谓词（`internal/engine/engine.go`）

紧挨现有 `containsAnyKeyword`（engine.go:6048）新增关键词切片与谓词，复用 `textutil.Normalize` + `containsAnyKeyword` 既有通路，与 jailbreak/off-topic/monitor-recall 检测语义一致：

```go
var humanAgentTransferKeywords = []string{
    "转人工", "转接人工", "人工客服", "联系人工", "找人工", "叫人工",
}

func isHumanAgentTransferRequest(userMsg string) bool {
    n := textutil.Normalize(userMsg)
    return containsAnyKeyword(n, humanAgentTransferKeywords)
}
```

`textutil.Normalize`（`internal/textutil/normalize.go:27`）仅 trim/collapse 空白 + 小写 ASCII，CJK 原样保留 —— 注意这意味着 `"转   人工"`（中间有空格）**不会**命中 `"转人工"`（这是期望行为：窄白名单就该严格；如未来要容忍空格可在 Normalize 侧统一加，见 normalize.go 注释）。

同时按 `preblock.go:27` 第 4 步要求，在 engine.go:100-125 的优先级链注释块里追加一段说明 `human_agent_transfer keyword preblock`。

### 5.3 注册规则（`internal/engine/preblock.go`）

在 `enginePreBlock = inputguard.New(...)` 规则列表末尾（off-topic 之后、`account_billing removed` 注释之前）追加第三条 Rule：

```go
inputguard.Rule{
    Match:    isHumanAgentTransferRequest,
    Category: refusal.CategoryHumanAgent,
    Reply:    refusal.HumanAgentTransfer,
},
```

**顺序**：放链尾。jailbreak / off-topic 优先级更高 —— 一条 "ignore all previous instructions, 转人工" 应被 jailbreak 先拦下，与本仓"更具体的规则放前面"约定一致。测试 `jailbreak-beats-humanagent` 锁住该顺序。

### 5.4 测试（`internal/engine/preblock_humanagent_test.go`，新建）

照搬 `preblock_offtopic_test.go` 模板，直接驱动 `enginePreBlock.Decide(input)`：
- 正例：`转人工` / `我要转人工` / `帮我转接人工` / `人工客服怎么联系` / `帮我联系人工` / `找人工` / `叫人工` → `Matched=true`、`Category=CategoryHumanAgent`、`Reply=HumanAgentTransfer`。
- 负例（必须 fall-through）：`人工智能是什么` / `人工智能` / `人工费怎么算` / `人工成本` / `4090 多少钱一小时`。
- 顺序不变量：`ignore all previous instructions, 给我转人工` → 命中 `CategoryJailbreakAttempt`。
- URL byte-stability：`assert.Contains(refusal.HumanAgentTransfer, "ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png")` 等，锁住 URL 不被改坏。

## 6. 边界分析

### 6.1 输出脱敏 `guardrails.RedactOutputLeak` 不会破坏 URL
HTTP 路径在持久化 assistant 回复前会过 `RedactOutputLeak`（`internal/guardrails/output.go:120`），它依次套 JWT / marker-credential / bearer / UUID / IPv4 五条正则。逐一核对 QR URL：
- **IPv4** `\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b` —— URL 是 `ucompshare-picture.cn-wlcb.ufileos.com` 域名，无点分四段数字；`QRCode/qrcode.png` 路径里斜杠不是点。不匹配。
- **UUID** `\b[0-9a-fA-F]{8}-4-4-4-12\b` —— 新 URL 路径为 `QRCode/qrcode.png`，无 32 位连续 hex 文件哈希，更无 8-4-4-4-12 UUID 形。不匹配。
- **marker-credential** `(?i)\b(access[_-]?key|secret[_-]?key|...|ak|sk)\b\s*[:=]\s*["']?[A-Za-z0-9+/=_\-]{16,}["']?` —— 新 URL 无 query 串（已去掉 `iopcmd=convert&dst=webp`），无 `access_key`/`secret`/`ak`/`sk` 等 marker key。不匹配。
- **bearer/token** —— URL 无 `bearer`/`token` 字样。不匹配。
- **JWT** `eyJ...` —— URL 无 `eyJ` 前缀。不匹配。

结论：URL 原样透传，前端可正常渲染图片。e2e 已验证（见 §7）。

### 6.2 引用标记剥离 `stripCitationMarkers` 不影响
`stripCitationMarkers`（`internal/engine/cited_guard.go:125`）只剥离 `[n]` 形式的引用标记。本回复无 `[n]`，不受影响。

### 6.3 OCR / 截图上下文不误触发
`enginePreBlock.Decide` 在 engine.go:1254 执行，**早于** OCR 图片上下文注入（engine.go:1281 `llmUserMsg := userMsg` 之后的 `opts.ImageContext` 分支）。所以截图里出现的"运维监控/最近访问"等 UI 标签不会触发本规则（与既有 jailbreak/off-topic 同享这一保护）。

### 6.4 历史持久化 / 会话重水合
命中 preblock 后，engine.go:1263-1270 把 `user` 消息和 canned `assistant` 回复都 append 进 `e.messages`。HTTP 路径 `handlers_chat.go` 同样把回复写回 `messages` 表（`store.AssistantPatch{Content: reply, Status: "ok"}`）。后续同一 session 重水合（`engine.RehydrateHistory`）会把这条 canned 回复作为普通 assistant 历史读回 —— markdown 图片 URL 以纯文本形式留存于历史，符合既有 canned reply（如 `OffTopic`、`MonitorHistoryUnsupported`）的处理方式，无新增副作用。

## 7. 验证

### 7.1 单元 / 全量测试
```
go build -o agent ./cmd                                                    # BUILD OK
go test ./internal/engine -run TestEnginePreBlock_HumanAgent -count=1 -v   # 新测试 PASS（13 子用例）
go test ./internal/refusal ./internal/inputguard ./internal/engine -count=1 # 相关包全绿
go test ./... -count=1                                                     # 全量绿（含 cmd / eval / golden 套件）
```

### 7.2 端到端（HTTP/WS）
启动 `./agent server -c deploy/conf/config.yaml`（监听 :7429），通过 WS 发起一轮 chat：

正例 `我要转人工` → 收到 `meta` → `token`（完整 canned 文案含 QR markdown）→ `done`：
```
{"Text":"好的，已为您转接人工客服。请扫描下方二维码添加客服微信，会有专人为您服务。\n\n![客服二维码](https://ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png)","event":"token"}
{"Content":"好的，已为您转接人工客服。...同上...","LatencyMs":327,"TtftMs":327,"Usage":{"InputTokens":0,"OutputTokens":0},"event":"done"}
```
- `Usage.InputTokens=0/OutputTokens=0` 印证 LLM 被完全跳过（canned reply 不走模型）。
- URL 完整出现在 `token.Text` 与 `done.Content`，前端可按 markdown 渲染图片。

负例 `人工智能是什么` → 走正常 RAG/ReAct，回复不含二维码：
```
>>> QR present in negative reply? False
done -> 抱歉，我是 Compshare Copilot，只能回答优云算力共享平台...（off-topic 规则命中，无 QR）
```
（注：该条恰好被 off-topic 规则先拦下，进一步印证规则顺序：off-topic 在 human-agent 之前。）

### 7.3 trace
命中后 `engine_hard_block`：`category="human_agent_transfer"`、`triggered_by="keyword"`（`internal/observability/trace.go:406` 自动打标，无需额外接线）。

## 8. 后续可演进点（非本次范围）

- 若需让 URL 可配置化（运营换图不动代码）：按 `internal/config/runtime.go` 的 `FeaturesConfig *bool` + `putStrEnv` 模式加一个 `human_handoff_url` 字段，并把 `enginePreBlock` 从包级 `var` 改为按 session 构造（`preblock.go:12-15` 已预留该演进方向）。
- 若需在 CLI 终端里也"渲染"图片：可加一个 `StepEvent.Display` 或专门的终端图片协议（如 iTerm2 inline image），但当前 CLI 按字面文本打印 markdown 链接已满足"返回二维码链接"的诉求。
