# ADR-002: Model Routing per Tier

**Status**: Proposed (2026-05-29)
**Depends on**: ADR-001(三路分层)
**Supersedes**: 当前单一 `llm.Client` 全局共享、所有 tier 都跑同一个 model 的设计

## Context

当前 `internal/llm/client.go:19-22` 的 `Client` 绑定单个 model(`cfg.Agent.LLM.Model`),6 个 inject 点(`cmd/cli.go:105`、`cmd/cli.go:344`、`cmd/shared_deps.go:57`、`internal/engine/engine.go:296`、`internal/ocr/client.go:27`、eval `golden_test.go:684`+`evaluate_test.go:180` 合 1)共享同一个 model。

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
| `cmd/cli.go:344` planner client | 单 client | `router.For(TierFast)` — planner 一律走 fast(planner 本身是 fast 类) |
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
- **Per-tier model 成本差异 user 看不见** → `internal/observability` 加 `task_tier` + `model` 双字段,trace 落库后可用 SQL 按 tier 聚合成本
- **Planner-on-flash 雪崩前提**:本 ADR 决定 planner 走 fast tier(ds-v4-flash),但**安全前提是 ADR-004 progressive disclosure 已落地**将 planner system prompt 从 ~5.9k 降到 ≤3k(memory `priortext-avalanche-invalidates-planner` 实测 input_tok 5k→11k 雪崩)。**在 ADR-004 (B4) 落地前 planner 必须继续跑 ds-v4-pro**(临时 `tier_routing.fast.planner_override: "deepseek-v4-pro"` 配置);B2(本 ADR 落地)与 B4 切换 planner 走 flash 必须显式同步,不可单飞

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
- [ ] 6 个 inject 点全部迁移到 Router,grep `llm.NewClient(` 在产品代码只剩 Router 内部一处
- [ ] `internal/observability` trace 加 `task_tier` + `model` 字段,server/cli 双路径都落
- [ ] `agent_yaml.tier_routing` 为空时 N=10 backward-compat 回归确认行为跟改造前一致(注:此 N=10 是 backward-compat smoke,跟 ADR-001 Acceptance #4 的 N=20+ tier 分类回归正交;前者测旧 config 不破,后者测新 tier 分类准确,两个 metric 不可混用)

## References

- ADR-001: Task-Complexity Tiered Architecture
- ADR-006: Agent path hard requirements(消费本 ADR 的 router 接口)
- `internal/llm/capability.go`(已有多 model 能力矩阵,本 ADR 复用)
