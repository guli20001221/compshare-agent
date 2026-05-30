# ADR-002: Model Routing per Tier

**Status**: Proposed (2026-05-29)
**Depends on**: ADR-001(三路分层)
**Supersedes**: 当前单一 `llm.Client` 全局共享、所有 tier 都跑同一个 model 的设计

## Context

当前 `internal/llm/client.go:19-22` 的 `Client` 绑定单个 model(`cfg.Agent.LLM.Model`),6 个 inject 点(`cmd/cli.go:105`、`cmd/cli.go:349`、`cmd/shared_deps.go:57`、`internal/engine/engine.go:296`、`internal/ocr/client.go:27`、eval `golden_test.go:684`+`evaluate_test.go:180` 合 1)共享同一个 model。

ADR-001 决定三 tier 走不同复杂度的模型:fast/knowledge=flash 类、agent=ds-v4-pro 类强模型。当前架构无法表达这个需求。

幸运的是 `internal/llm/capability.go:38-108` 的 `builtinCapabilities` 已经支持多 model(目前已注册 6 个 model 的能力矩阵),这部分不需要改;只缺一个 Router 来按 tier 选 model。

**Tier vs capability 矩阵区分**:`builtinCapabilities` 的 6 个 model 含对话 tier(本 ADR scope:fast / knowledge / agent 共 2 个 unique model)+ OCR(`internal/ocr/`,独立路径,不走 Router)+ 备用 / fallback model(claude / qwen 等,B7+ MCP 接入或 fail-over 时启用)。本 ADR 只决策对话 tier 路由,其余 model 用途以 `capability.go` 内 doc-comment 为准。

## Decision

新增 `internal/llm/router.go`,把"按 tier 选 model"作为 first-class 操作。每 tier 一个 Client 实例(底层共享 HTTP transport,model 字段不同)。

### Router 接口

```go
package llm

// Tier identifies the task-complexity tier from ADR-001.
type Tier string

const (
    TierFast      Tier = "fast"
    TierKnowledge Tier = "knowledge"
    TierAgent     Tier = "agent"
)

// Router holds per-tier LLM clients. Construct once at boot, share across sessions.
type Router struct {
    clients map[Tier]*Client
}

// For returns the Client for the given tier. Panics if tier is not configured —
// misrouting is a bug, not a runtime fallback condition (per ADR-001 risk
// mitigation: fallback direction is fast > knowledge > agent, decided at the
// planner level, NOT at the router).
func (r *Router) For(tier Tier) *Client { ... }

// Capability returns the capability matrix for the tier's model.
func (r *Router) Capability(tier Tier) Capability { ... }
```

### Config 形态

`deploy/conf/agent.yaml`:

```yaml
agent:
  llm:
    base_url: "https://api.modelverse.cn/v1"
    api_key: "${LLM_API_KEY}"
    # Default model used when tier_routing is empty (backward compat).
    model: "deepseek-v4-flash"
  tier_routing:
    fast:
      model: "deepseek-v4-flash"
    knowledge:
      model: "deepseek-v4-flash"
    agent:
      model: "deepseek-v4-pro"
      # Optional per-tier override (timeout, base_url, etc.)
      timeout_ms: 180000
```

`tier_routing` 为空时,全 tier 回退 `agent.llm.model`(backward compat,旧 config 无侵入)。

### Inject 改造

| 点 | 当前 | 改后 |
|---|---|---|
| `cmd/cli.go:105` main CLI client constructor | 单 `*Client` | CLI 启动构造 `*Router` 注入 SharedDeps |
| `internal/engine/engine.go:296` `SharedDeps.LLMClient` | 单 `*Client` | `*Router`,Engine 内部按 step 上下文调 `router.For(tier)` |
| `cmd/cli.go:349` planner client | 单 client | `router.For(TierFast)` — planner 一律走 fast(planner 本身是 fast 类) |
| `cmd/shared_deps.go:57` grounded renderer | 单 client | `router.For(TierKnowledge)` — grounded render 是 knowledge 路径产物 |
| `internal/ocr/client.go:27` OCR | 单 client | 不动,OCR 不是对话 LLM,独立 model 配置 |
| `eval/golden_test.go:684` / `evaluate_test.go:180` | 单 client | 接受 `tier` 参数,默认 fast |

## Consequences

**Positive**
- ADR-001 三 tier 落地的最小必要变化(只动 router + inject 点,不改 engine 业务逻辑)
- Capability 表已有的多 model 支持得到利用
- Per-tier 独立 timeout / extra_body 配置,agent path 给更长 reasoning 时间不影响 fast path 延迟

**Negative**
- 6 个 inject 点要改(可控,grep-able)
- `SharedDeps.LLMClient *Client` 改 `LLMClient *Router` 是 breaking API,所有持有 SharedDeps 的代码同步改

**Risks**
- **Tier 选错**(本应 agent 的请求被 planner 误判为 fast)→ 缓解在 ADR-001(fallback 倾向 fast,降级体验而非成本飙升);Router 不做兜底,misrouting 是 planner bug 不是 router bug
- **Per-tier model 成本差异 user 看不见** → `internal/observability` 加 `task_tier` 顶层字段 + 复用 nested `PlannerTrace.Model` / `RendererTrace.Model`,trace 落库后可 JOIN 聚合成本(具体字段位置见 Acceptance #4)
- **Planner-on-flash 雪崩前提**:本 ADR 决定 planner 走 fast tier(ds-v4-flash)。雪崩事实仍成立——memory `priortext-avalanche-invalidates-planner` 实测 PriorText 滚雪球把 planner input_tok 从 5k 推到 11k——但缓解手段是 **prompt 结构工作**(B2b progressive disclosure + 祈使句 directive 清理 + `target_ref` few-shot),而非换 model。~~在 ADR-004 (B4) 落地前 planner 必须继续跑 ds-v4-pro~~ 此 pro-interim 要求已被 **Amendment 1(2026-05-31)RETRACTED**:N=8 jitter + pro oracle 实测 ds-v4-pro 在 borderline 上更差,planner 永久守 flash。详见本 ADR 末尾 Amendment 1

## 业界对照

| 平台 | 模型分流机制 |
|---|---|
| AWS Q Developer | Nova-lite(快查)vs Claude(Pro)双 model |
| AWS Bedrock Agent | Agent 配置时指定 foundation model;不同 Agent 不同 model |
| OpenAI Assistants | Per-Assistant model + per-Run model override |
| Anthropic Claude.ai | Haiku / Sonnet / Opus 用户选择 + 服务端按场景 auto-select |
| 本项目 | Per-tier model,planner 决定 tier |

## Acceptance

- [ ] `internal/llm/router.go` 新增,~100 行,含 `Router` + `Tier` + `For/Capability` 方法 + table tests
- [ ] `deploy/conf/agent.yaml.example` 加 `tier_routing` block + 文档说明 backward compat 规则
- [ ] 6 个 inject 点全部迁移到 Router,grep `llm.NewClient(` 在产品代码只剩 Router 内部一处。**Batch 拆分**:B1 落 Router infra + 2 个 grounded renderer 点(`cmd/cli.go:105` + `cmd/shared_deps.go:57` → `Router.For(TierKnowledge)`);B2 迁 `internal/engine/engine.go:296` + `eval/golden_test.go:684` + `evaluate_test.go:180`(engine 内部 + eval tier-aware);B4 迁 `cmd/cli.go:349` planner 到 `Router.For(TierFast)` = flash(**不是** pro):此迁移仅为 tier-routing 接线,不改 planner 的 model family(planner 守 flash,见上方 Risks 第 3 项 + 末尾 Amendment 1)。B1 时 grep `llm.NewClient(` 产品代码命中 **5 处**:B2/B4 待迁 4 处(planner / engine / 2 eval)+ `internal/ocr/client.go:27` OCR **永不迁**(独立路径,ADR-002:82),不是 acceptance 失败
- [ ] `internal/observability` trace 加 `task_tier` **顶层字段**(server/cli 双路径都落)。**per-call model 复用现有 nested 字段**(`PlannerTrace.Model` / `RendererTrace.Model`)— 不加顶层 `trace.model`,跟 router.go:102-108 doc 保持一致。B1 schema 已落 `task_tier` slot,B4 接 populator 把 router.For(tier) 的 model 写进对应 nested trace
- [ ] `agent_yaml.tier_routing` 为空时 N=10 backward-compat 回归确认行为跟改造前一致(注:此 N=10 是 backward-compat smoke,跟 ADR-001 Acceptance #4 的 N=20+ tier 分类回归正交;前者测旧 config 不破,后者测新 tier 分类准确,两个 metric 不可混用)

## References

- ADR-001: Task-Complexity Tiered Architecture
- ADR-006: Agent path hard requirements(消费本 ADR 的 router 接口)
- `internal/llm/capability.go`(已有多 model 能力矩阵,本 ADR 复用)

## Amendment 1(2026-05-31):Planner 守 flash

**决定**:planner 永久走 ds-v4-flash(fast tier),不迁 ds-v4-pro,连过渡态也不行。本 Amendment RETRACT 上方 Risks 第 3 项原 "在 ADR-004 (B4) 落地前 planner 必须继续跑 ds-v4-pro" 的 pro-interim 要求,以及 "pro 是 ADR-004 落地前安全过渡态" 的前提。

**证据**:针对当前 pre-ADR-004 prompt,pro-interim 前提被实测推翻。一次 6-question jitter check(每题 N=8 跑,planner 分别跑两个 model)叠加一次 pro oracle smoke 发现 ds-v4-pro 在 borderline 上比 flash 更差:pro 漏判到 `intent=unknown` 并抬高 `schema_invalid`,而 flash 守住 `schema_invalid=0`(如 `ssh-boundary`、`vague-monitor`)。zero-target `帮我关机` 案例在两个 model 上都 `schema_invalid` 8/8 失败——这是 prompt / `target_ref` few-shot 缺口,不是 model 缺口。因此可靠性杠杆在 **prompt**(B2b progressive disclosure + 祈使句 directive 清理 + `target_ref` few-shot),不在 model。

**`≤3k` 降级**:lead 已放宽 token-cost 约束(cheap model),故原 "planner system prompt ≤3k" 目标从 hard gate **降级为 reported metric**——progressive disclosure 的价值转为 prompt clarity/maintainability + reliability 的 prompt-structure 工作,不再卡字节目标。

**Caveat**:jitter 证据是 N=8 跑在当前大 prompt 上;在 B2b 更小的 prompt 下需重测 pro 后才能推广该结论。

**来源**:`docs/plans/roadmap.md` "Decision #1"(resolved 2026-05-29);session-memory note `ds-v4-pro-not-model-bottleneck-for-planner-prompt`。
