---
status: Approved, ready for implementation
date: 2026-05-13
scope: PR-ORCH-1 — Evidence type + visibility contract + no-evidence behavior
out_of_scope: engine.go changes, renderer integration, real RAG retrieval (those are PR-RAG-6)
blocks: PR-RAG-6 (runtime hookup)
related:
  - docs/plans/2026-05-13-stage2b-rag-w0-source-pipeline.md
---

# PR-ORCH-1 设计文档：Evidence 类型 + 可见性契约

## 1. 背景

PR-ORCH-1 是 PR-RAG-6（runtime hookup）的硬阻塞前置。本 PR 范围是**类型 + 契约 + 一个行为决策**，不动 `internal/engine/engine.go`、`internal/prompt/builder.go`、`internal/knowledge/*`。

设计动机来自三件事：

1. 工业界共识（GCP Gemini Cloud Assist 的 Observation、Snowflake 的 OTel spans、AWS Q 的 citations）一致认为 reasoning 输出应该是结构化 evidence 单元而不是自由文本。
2. 当前 `internal/security/secret_boundary.go` 的 `RedactForLLM` / `RedactForTrace` 双出口模式给我们提供了同构延伸点。
3. 不在 PR-RAG-6 临场决定"哪些字段能给用户看、哪些只进 trace、哪些不出 boundary"——这种临场决策是 RAG 落地反模式的源头。

## 2. 设计目标

- (a) 让 RAG chunk 命中、API 工具结果、诊断结果有同构的 evidence 表达
- (b) 用类型契约阻止下游临场拼字段、误把内部来源带进答案
- (c) 为 PR-RAG-6 提供"调投影方法就行"的开箱即用 API
- (d) 对接 `internal/security/secret_boundary.go` 的 ForUser/ForTrace/ForLLM 模式
- (e) 同时定义 `knowledge_qa` 检索 0 命中时的行为，避免临场决定

## 3. 非目标

- 不改 engine.go、不动 ReAct 主循环
- 不实现真正的 RAG 检索（PR-RAG-6 的事）
- 不替换现有 `envelope` / `renderer` 抽象（只是新增 Evidence）
- 不引入 streaming / SSE / structured-output schema
- 不实现 retrieval 多 chunk ranking 策略（PR-RAG-6 或更晚）

## 4. Evidence 字段表

字段按三层显式分类。命名用 Go 风格 `PascalCase`，JSON tag 用 `snake_case`，分类用 struct tag `visibility:"user|trace|internal"` 标注。

### 4.1 user 层（最终渲染给用户）

| 字段 | 类型 | 必填 | 含义 | 限制 |
|---|---|---|---|---|
| `SourceTitle` | `string` | ✓ | 来源标题，UI 卡片标题 | ≤ 80 char |
| `Snippet` | `string` | ✓ | 摘要文字，呈现给用户 | ≤ 280 char |
| `SurfaceURL` | `*string` | 否 | 用户可访问的 source URL | nil 或 host 在白名单（见 §7） |
| `EvidenceKind` | enum | ✓ | `knowledge` \| `api_fact` \| `diagnosis` \| `workflow_result` | 见 §5 |

### 4.2 trace 层（只进 `internal/observability/trace.go`，不进 LLM 上下文也不进回复）

| 字段 | 类型 | 必填 | 含义 |
|---|---|---|---|
| `ChunkID` | `string` | knowledge 必填 | 命中的 chunk_id |
| `KBVersion` | `string` | knowledge 必填 | kb_version |
| `RetrievalScore` | `float64` | knowledge 必填 | 检索 score |
| `QueryNormalized` | `string` | knowledge 必填 | 归一化后的检索 query（审一致性用） |
| `APIRefs` | `[]string` | api_fact 必填 | 对应的 API 调用名列表 |
| `ToolCallID` | `string` | api_fact / workflow_result / diagnosis 必填 | 用于和 trace 中 tool call 互相索引 |
| `ProducedAt` | `time.Time` | ✓ | 生成时间 |

### 4.3 internal 层（永不出 boundary）

| 字段 | 类型 | 必填 | 含义 |
|---|---|---|---|
| `InternalSourceID` | `*string` | 否 | 内部 source ID（GitLab path、Feishu doc id 等） |
| `ApprovalRecordHash` | `*string` | case 改写来源必填 | 对应 plan §2.4 的审批记录哈希 |
| `OriginalCaseID` | `*string` | 否 | 内部 case 来源的 case_id |
| `DebugReason` | `*string` | 否 | 调试用上下文，仅本地日志可见 |

**重要：上面三层是 struct 内部字段，对外只能通过 §6 的三个投影方法访问。**

## 5. EvidenceKind enum

| 值 | 含义 | W0 是否使用 |
|---|---|---|
| `knowledge` | 来自 RAG 检索的 chunk 命中 | ✓ |
| `api_fact` | 来自 CompShare API 工具调用的结构化事实 | 后续 |
| `diagnosis` | 来自 `internal/diagnosis` 诊断链的结论 | 后续 |
| `workflow_result` | 来自 `internal/workflow` 工作流的执行结果 | 后续 |

W0 阶段只允许 `knowledge`（与 plan §7.3 的 `evidence_kind` 字段一致）。其他三个值的承接通过 PR-RAG-6 之后的 PR 分批接入。

新增枚举值必须走 schema review PR + 同时改 `internal/knowledge/loader.go` 的允许列表。

## 6. 投影方法（API 强制路径）

只通过三个投影方法访问 Evidence。**直接读 struct 字段是违反契约的**，要靠 code review + 后续可选 linter 拦。

### 6.1 ForUser()

```go
type UserView struct {
    SourceTitle  string  `json:"source_title"`
    Snippet      string  `json:"snippet"`
    SurfaceURL   *string `json:"surface_url"`
    EvidenceKind string  `json:"evidence_kind"`
}

func (e Evidence) ForUser() UserView
```

约束：

- 即使 `SurfaceURL` 字段在 Evidence 内部不为 nil，如果 host 不在运行时白名单内，`ForUser()` 必须返回 `SurfaceURL=nil`
- 字段映射完全平铺，不嵌套
- 不暴露 `ChunkID` / `KBVersion` / `InternalSourceID` 或任何 trace / internal 层字段
- 调用方拿到的是值类型 `UserView`，不暴露内部 Evidence 引用

### 6.2 ForTrace()

```go
type TraceView struct {
    SourceTitle               string    `json:"source_title"`
    EvidenceKind              string    `json:"evidence_kind"`
    ChunkID                   string    `json:"chunk_id,omitempty"`
    KBVersion                 string    `json:"kb_version,omitempty"`
    RetrievalScore            float64   `json:"retrieval_score,omitempty"`
    QueryNormalized           string    `json:"query_normalized,omitempty"`
    APIRefs                   []string  `json:"api_refs,omitempty"`
    ToolCallID                string    `json:"tool_call_id,omitempty"`
    ProducedAt                time.Time `json:"produced_at"`
    SurfaceURL                *string   `json:"surface_url,omitempty"`
    SurfaceURLRejectionReason *string   `json:"surface_url_rejection_reason,omitempty"`
}

func (e Evidence) ForTrace() TraceView
```

约束：

- 不暴露 `Snippet`（避免 trace 体积爆炸，且 trace 不是给人读的）
- 不暴露 `InternalSourceID` / `ApprovalRecordHash` / `OriginalCaseID` / `DebugReason`（按 `secret_boundary` 同等约束，trace 不写内部 ID）
- `SurfaceURL` 字段的定义是"可给用户看的来源链接"，trace 和 user 视角共用同一份白名单：只有通过 `IsAllowedSurfaceURL` 的 URL 才写入 `TraceView.SurfaceURL`；未通过时 `TraceView.SurfaceURL = nil`，`TraceView.SurfaceURLRejectionReason` 记录 policy 给出的简短理由（如 `host_not_in_allowlist` / `scheme_not_https`）
- 原始内部 URL 永不进 trace。内部来源用 `InternalSourceID` 表达；本地调试若需原始 URL，从 `DebugReason` 字段读，且 `DebugReason` 仅本地日志可见，不进任何远端 trace

### 6.3 ForLLM()

```go
type LLMView struct {
    SourceTitle  string `json:"source_title"`
    Snippet      string `json:"snippet"`
    EvidenceKind string `json:"evidence_kind"`
    // 不暴露任何 ID、URL、score
}

func (e Evidence) ForLLM() LLMView
```

约束：

- 只供 LLM 二次阅读用（比如 ReAct 下一轮想引用这条 evidence）
- 不暴露 URL（避免 LLM 编造 URL 或泄露 trace ID 风格 token）
- 不暴露 ChunkID（避免 LLM 学会编造 chunk_id 后报错）
- 不暴露 RetrievalScore（避免 LLM 把 score 当事实陈述）

## 7. SurfaceURL 白名单

实现：纯函数，建议位置 `internal/envelope/evidence_url_policy.go`。

```go
type URLDecision struct {
    Allowed bool
    Reason  string  // 用于 trace 调试
}

func IsAllowedSurfaceURL(rawURL string) URLDecision
```

### 7.1 W0 初版允许列表

只列当前语料里能找到来源的 host：

```text
console.compshare.cn          # 控制台导航 hint
www.compshare.cn              # 仅允许 path 前缀 /docs/ 下
```

### 7.2 待确认（不进 W0 白名单，需要团队确认是否存在再加）

- 任何其他 compshare 自有的对用户开放的 host（目前没有确认存在的 `docs.*` 或 `help.*`）
- 第三方 host（社区论坛、供应商文档）需要逐个评估

### 7.3 显式拒绝（不管 host 是什么，命中即 fail）

- 任何 `*.gitlab.*` host（内部代码托管）
- 任何 `*.feishu.cn` / `*.lark.com` / `*.feishu.*` host（内部协作）
- 任何 URL path 包含 `/admin` / `/workorder` / `/internal`
- 任何 URL query 包含 `token=` / `signature=` 或其他 signed-URL 参数
- 任何 URL scheme 不是 `https`
- 任何临时下载链接（host 或 query 表明过期）

### 7.4 两层校验

- **离线**：`scripts/rag_w0/validate_chunks.py` 在 chunk 入库前调用同等规则，确保 corpus 干净（plan §7.6 失败规则已写入）
- **运行时**：`Evidence.ForUser()` 内部再调一次 `IsAllowedSurfaceURL`，作为防御层——如果离线漏过、或者未来扩展时引入新 chunk 来源，运行时仍能拦截

## 8. no-evidence 行为（决策点）

**决策**：采用方案 B（显式回避），**且严格限定在 `EvidenceKind=knowledge` 路径**。

### 8.1 触发条件

- 路由层判定本轮意图为 `knowledge_qa`
- 检索器返回 0 个通过阈值的 chunk

### 8.2 行为

返回固定文案：

```text
我没有在知识库里找到可靠资料来回答这个问题。建议你在控制台对应模块查看，或联系平台客服确认。
```

trace 同时写入：

```text
evidence_kind=knowledge
hits=[]
no_evidence=true
fallback_reason="retrieval_zero_hit"
```

### 8.3 严格不适用的范围

下列路径**不走 no-evidence refusal**——它们是 API-first 路径，API 返回空 ≠ 拒答：

- 实时实例查询：`"我有几台实例？"` 返回 0 台 → 答 `"你当前没有实例"`，正常应答
- 监控查询：`GetCompShareInstanceMonitor` 返回空数据 → 走原有 fallback 文案
- 价格查询：未知规格 → 走原有 fallback 文案
- 工作流执行：执行失败 → 走原有错误回包
- 诊断链：诊断不出结论 → 走原有诊断输出格式

### 8.4 为什么 B 而不是 A

A（让 LLM 用 model prior 答）会把不准确的回答风险放进来，而本项目目标是给 SPT 减负——一个不准的回答比一个"建议看控制台"更糟。

C（重路由到 FAQ / 工单）在 W0 阶段没有 fallback skill 可路由，等 H1 落地后可以演进到 C。

## 9. `hit_items[].kept` 语义（给 PR-ORCH-2 参考）

本字段属于 `RetrievalTrace v1`，严格属于 PR-ORCH-2，但定义在这里固化，避免 PR-ORCH-2 临场决定。

**定义**：`kept` 表示**已通过离线安全过滤的候选里，是否最终用于本轮回答**。

### 9.1 明确不属于 `kept=false` 的范围（不进运行时 trace）

下列内容**必须在离线阶段就被拦掉**，永远不出现在 RetrievalTrace 里：

- 内部资料 / 未审批 case rewrite
- ACL ≠ `customer_safe` 的 chunk
- `internal_reference_only` source 派生 chunk
- approval_record 缺失的 case 改写

如果 chunk 离线检查通不过，它根本不会进 `deploy/kb/stage2b_w0.jsonl`——所以运行时检索器永远召不到它，也就永远不会出现在 trace `hit_items[]` 里。

### 9.2 `kept=false` 的合法情形

只有以下情形 chunk 可以以 `kept=false` 写入 trace：

- 安全过滤通过、被检索器召回，但 ranking / diversification 阶段被丢弃
- 通过阈值但被 top-K 截断
- 通过 top-K 但 LLM 在 ReAct 中选择不引用

`kept=false` 用于审计 ranking 决策，不用于审计安全过滤。安全过滤是离线问题。

## 10. 单测边界清单

PR-ORCH-1 必须包含以下单测（建议位置 `internal/envelope/evidence_test.go`）：

| # | 测试 | 验证什么 |
|---|---|---|
| T1 | `ForUser` 不暴露 ChunkID | UserView 反射没有 ChunkID 字段 |
| T2 | `ForUser` 不暴露 InternalSourceID | 同上 |
| T3 | `ForUser` 在 SurfaceURL host 不在白名单时返回 nil | 输入 gitlab URL → SurfaceURL=nil |
| T4 | `ForUser` 接受 console.compshare.cn / www.compshare.cn/docs/ | 白名单允许路径 |
| T5 | `ForTrace` 不暴露 Snippet | TraceView 反射没有 Snippet 字段 |
| T6 | `ForTrace` 不暴露 InternalSourceID / ApprovalRecordHash | 同上 |
| T7 | `ForTrace` 不暴露内部 URL，记录拒绝原因 | 输入 gitlab URL → `TraceView.SurfaceURL=nil` 且 `TraceView.SurfaceURLRejectionReason` 非 nil |
| T8 | `ForLLM` 不暴露任何 ID / URL / score | LLMView 反射只有 SourceTitle / Snippet / EvidenceKind |
| T9 | `EvidenceKind=knowledge` 时 ChunkID / KBVersion / RetrievalScore 必填 | 缺一个 → 构造函数 error |
| T10 | `EvidenceKind=api_fact` 时 APIRefs / ToolCallID 必填 | 同上 |
| T11 | `IsAllowedSurfaceURL` 拒绝 gitlab / feishu / admin / token / 非 https | 显式 denied 路径 |
| T12 | `IsAllowedSurfaceURL` 接受 console.compshare.cn 顶层路径 + www.compshare.cn/docs/ 路径 | 白名单允许 |
| T13 | `IsAllowedSurfaceURL` 拒绝 www.compshare.cn 非 /docs/ 路径 | 白名单 path 约束 |
| T14 | `OriginalCaseID` 非 nil 但 `ApprovalRecordHash` nil → error | 审批一致性 |
| T15 | no-evidence helper 返回固定文案 + 标 no_evidence=true | §8.2 决策 |

反射式黑盒测试（T1/T2/T5/T6/T8）用 `reflect.TypeOf(view).NumField()` + 字段名遍历，确保编译期增字段必然让某条测试 fail。

## 11. 验收标准

- [ ] §10 全部 15 个单测过
- [ ] §6 三个投影方法签名 + 字段映射代码可读
- [ ] 本文档的字段表与代码完全一致（review 时对照）
- [ ] 不动 `internal/engine/engine.go`
- [ ] 不动 `internal/prompt/builder.go`
- [ ] 不动 `internal/knowledge/*`
- [ ] `go vet ./...` 干净
- [ ] PR 描述说明：PR-RAG-6 是消费者，PR-ORCH-1 是供应者
- [ ] PR 描述说明：W0 阶段 `EvidenceKind` 实际只使用 `knowledge`，其他枚举值是预留

## 12. PR-ORCH-1 不解决但 PR-RAG-6 必须解决的事

记下来，提醒 PR-RAG-6 开 PR 时核对：

- Renderer 从返回 `string` 切换到接受 `[]Evidence` + 调 `ForUser()` 拼最终文本
- `engine.go` ReAct 主循环消费 LLM 输出时，从工具调用结果或 knowledge 检索结果构造 Evidence
- 没有 Evidence 命中（no-evidence case）时走 §8 的 B 文案，**严格仅限 `knowledge_qa` 路径**
- trace 写入时调 `ForTrace()`
- 不允许任何代码路径直接读 Evidence struct 字段
- `inferKnowledgeProductArea` 顺手从 `engine.go` 搬到 `internal/knowledge/router.go`（< 30 分钟，趁这次 PR 一起做）

## 13. 工时估算

| 任务 | 估算 |
|---|---|
| §4 字段定义 + struct tags | 0.25 d |
| §6 三个投影方法 + 校验逻辑 | 0.5 d |
| §7 URL 白名单纯函数 + 单测 | 0.25 d |
| §8 no-evidence helper + 单测 | 0.25 d |
| §10 15 个单测全部 | 0.75 d |
| 文档润色 + PR description | 0.25 d |
| code review buffer | 0.25 d |
| **合计** | **~2.5 d** |

## 14. 不解决的已知问题（留给后续 PR）

- **Evidence 列表的 ranking / diversification**：本 PR 只定义单个 Evidence，多个 Evidence 之间的优先级 / 去重 / cross-check 是 PR-RAG-6 或更晚的事
- **Evidence 持久化**：会不会把 Evidence 列表 dump 到 trace 之外的某个 evidence store？等需要时再加
- **跨 turn evidence carry-over**：用户上一轮拿到的 evidence 下一轮还能不能用？目前默认每 turn 重算，未来如果做 session 持久化（M2）一起考虑
- **streaming Evidence emit**：要不要让每条 Evidence 边生成边推送给前端？等接入 console 时讨论

## 15. 相关参考

- 现有 secret 边界范本：`internal/security/secret_boundary.go`
- 现有 envelope 类型范本：`internal/envelope/`
- W0 plan 中的 chunk schema：`docs/plans/2026-05-13-stage2b-rag-w0-source-pipeline.md` §7.3
