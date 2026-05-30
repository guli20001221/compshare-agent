# ADR-005: Diagnosis Sub-Tier Boundary

**Status**: Proposed (2026-05-29,Revised 2026-05-29 撤回 wrap-as-ChainSubAgent 方案)
**Depends on**: ADR-001(agent tier 定义)/ ADR-004(skill bundle 框架)/ ADR-006(agent path 硬要求,diagnosis 复用其 SSH sandbox + step trace)
**Scope**: 诊断 sub-tier 的接口契约 + 现有 Chain Go 代码处置 + BillingAnomaly 重新分类;具体实现(A 路线 vs B 路线)**本 ADR 不决断**,留 Q2 真机故障 case 后定

## Context

诊断(SSH debug / GPU 排障 / 日志查 / 配置问题)是 agent tier 内部一类特化任务,但跟"部署 / 扩容 / 重建"这类 mutating 任务有本质不同:

| 特征 | Mutating(部署类) | Diagnosis(排障类) |
|---|---|---|
| 操作类型 | 关键 mutating + 副作用 | 主要 read-only 探索 |
| 计划形态 | 闭合 step 序列(skill 写死) | 开放 reasoning(无法预定步) |
| Rollback | 必需(saga) | 不需要 |
| HITL | 步级 confirm + state pause | 命令级 confirm |
| 成功标准 | 实例 Running + 配置就绪 | "找到了原因"(用户主观) |
| 失败模式 | 资源泄漏 / 部分回滚 | LLM loop / 找不到结论 |

### 现状 audit(2026-05-29)

`internal/diagnosis/` 当前 6 个 chain:

| Chain | 验证状态 | 实际类型 |
|---|---|---|
| `BillingAnomalyChain` | **production_validated**(同事生产已部署) | **不是诊断,是 API 查询型**(2 步:列实例 + 查价格,然后渲染) |
| `SSHFailureChain` | unverified | 真诊断,state branching + monitor cross-check |
| `InitFailureChain` | unverified | 真诊断,multi-instance scan |
| `GPUNotDetectedChain` | unverified | 真诊断,state + GPU monitor |
| `PortFirewallChain` | unverified | 真诊断,instance + platform port catalog |
| `ImageIssueChain` | unverified | 真诊断,state + image type |

5 个真诊断 chain 全部 **从未在 production 跑过**(只有设计意图 + Go 代码骨架)。BillingAnomaly **不是诊断**,是查询型业务逻辑,被错放在 diagnosis 包。

## Decision

### 1. 诊断作为 agent tier 子路径

诊断不是第四个 tier,是 **agent tier 的子分类**。Planner emit `task_tier=agent` + `agent_subtype=diagnosis`,Engine 按 subtype 走分流:

```
agent tier
├── mutating sub-path  → orchestrator + saga + 步级 HITL(ADR-006)
└── diagnosis sub-path → SubAgentRegistry.Pick(ctx).Run()(本 ADR)
```

复用 ADR-006 的 step trace / observability / SSH sandbox / 凭证生命周期,**不进 saga**(诊断无 compensate 概念)。

**Multi-SubAgent routing**:`Match(ctx) bool` 方法存在的理由 — 允许多个 SubAgent impl 并存(A 路线 + B 路线同时 ship,或同路线下不同 skill 走不同 SubAgent)。Registry 遍历所有注册的 SubAgent,按 `Match()` 返回 true 的第一个胜出(注册顺序 = priority)。如果未来只决定走单一 impl,Match() 退化为永远 true,但接口保留以保 backward compat。Q2 决断后可视情况删 Match()。

### 2. BillingAnomaly 迁出 diagnosis 包,归 fast tier capability

| 字段 | 当前 | 迁移后 |
|---|---|---|
| 位置 | `internal/diagnosis/billing_anomaly.go` | `internal/intent/capability_billing.go` |
| Intent | `IntentBillingInstance`(已存在 `types.go:11`) | 同左,新增 capability_registry 项 |
| Tier | agent.diagnosis(错位) | fast |
| 形态 | Chain 框架 + 两步 API + 模板渲染 | 标准 fast tier handler(对照 `capability_pricing.go` 形态) |
| `BillingFactsSummary` struct | `diagnosis` package | 迁 `intent` package 或 `internal/entity/billing.go` |
| 验证状态 | production_validated 保留 | 同左,迁移后回归测试覆盖 |

迁移实施 spec 见 `docs/plans/2026-05-29-billing-anomaly-to-fast-tier.md`(B5 范围)。

### 3. 5 个 unverified Chain Go 代码 — 退役

**直接删除**(撤回 2026-05-28 初版 ADR-005 的 wrap-as-ChainSubAgent 方案)。

撤回理由:
- 5 chain 全 unverified = 没在 production 用 = 删除不 break user
- A 路线(LLM 自写 ReAct)+ B 路线(Claude Code 子进程)**都是 LLM 自己决定下一步**,deterministic step 序列没有用武之地
- wrap-as-ChainSubAgent 是给死代码找借口("reference impl + fallback library"),实际不会被任何路径调用
- 维护 5 个未验证 Go 文件 + 适配层(~150 行 ChainSubAgent)= 纯增加 codebase 噪音

### 4. 5 个 Chain 的 SOP 内容 — 提炼为 skill markdown

5 chain 的内容价值(state branching 决策树 + verdict 文案 + 业务知识)真实存在,以 ADR-004 skill 框架的 markdown body 形式保留:

```
internal/skills/
├── diagnose_ssh/skill.md                 # ← 从 SSHFailureChain 提炼
├── diagnose_init_failure/skill.md        # ← 从 InitFailureChain 提炼
├── diagnose_gpu_not_detected/skill.md    # ← 从 GPUNotDetectedChain 提炼
├── diagnose_port_firewall/skill.md       # ← 从 PortFirewallChain 提炼
└── diagnose_image_issue/skill.md         # ← 从 ImageIssueChain 提炼
```

**全部 `verification_status: unverified`**(ADR-004 amendment 引入的 frontmatter 字段),Loader fetch body 时前置 `[CAUTION: this methodology is unverified, treat steps as suggestions not facts]` 行,降低 LLM 把方法论假设当事实输出的 fab 风险(memory `corpus-input-source-tiering` 相邻应用)。

升级路径:`unverified → spike_validated`(≥1 真实故障 case 跑通 + reviewer sign-off)`→ production_validated`(≥10 真实流量场景 + 误判率 < 10%)。

### 5. DiagnosisSubAgent Interface

```go
// internal/diagnosis/subagent.go (新增,impl-agnostic interface)
package diagnosis

type SubAgent interface {
    Name() string
    Match(ctx DiagCtx) bool
    Run(ctx DiagCtx, emit func(StepEvent)) (Result, error)
}

type DiagCtx struct {
    SessionID    string
    InstanceID   string
    UserSymptom  string             // 用户原话,e.g. "GPU 不识别"
    SSHCreds     SSHCredentialRef   // 凭证 reference,subagent 不直接持 key
    AllowedCmds  CommandPolicy      // 复用 ADR-006 SSH sandbox 白名单
    Budget       Budget             // turn cap / wall time / output bytes
    SkillBodies  []string           // 主 agent fetch 后的 skill body 字符串列表(见 §6 B 路线澄清)
}

type Result struct {
    Conclusion       string             // 给用户的最终结论
    Confidence       float64            // 0-1,subagent 自评
    EvidenceCmds     []ExecutedCmd      // audit trail,所有跑过的命令
    SuggestedActions []SuggestedAction  // 可选 fix 建议(用户决策是否执行)
    ConvergedAt      time.Time
}

type ExecutedCmd struct {
    Cmd       string                    // 已 sanitize(IP/host/banner 已脱敏)
    Output    string                    // 已 cap + sanitize
    StartedAt time.Time
    EndedAt   time.Time
    Exit      int
}

type SuggestedAction struct {
    Kind        string                  // "ssh_command" / "console_link" / "skill_invoke"
    Description string                  // 中文给用户看
    Detail      map[string]any
    NeedConfirm bool                    // 任何 mutating action 必 true
}
```

主 Engine 端不知道 subagent 内部是 LLM ReAct loop 还是子进程委派 — 只看 `emit StepEvent` 流 + 最终 `Result`。

### 6. B 路线 skill consumption 责任归属 — 主 agent,不是 Claude Code

**Claude Code 不知道什么是 skill**,只接受 `--append-system-prompt` 单字符串。所以 B 路线的实际数据流:

```
主 agent planner 选中 diagnose_gpu_not_detected skill (+ 可能 safety_warning 等 cross-tier skill)
  → ADR-004 loader 按 verification_status 处理(unverified 前置 CAUTION 行)
  → 主 agent fetch + 拼接 skill bodies (受 ADR-004 N=2 默认 + 2K token budget cap 约束)
  → 拼成完整字符串放进 DiagCtx.SkillBodies
  → 主 agent 通过 exec.Command("claude", "--append-system-prompt", <拼好的 prompt>, ...) 起子进程
  → Claude Code 完全黑盒,只看到一个 system prompt 增量
```

**关键澄清**: skill consumption 是 ADR-004 loader + 主 agent prompt 拼接的责任,Claude Code 不感知 skill 概念。B 路线实施时不要尝试让 Claude Code "理解 SKILL.md format",那是 wrong abstraction level。

A 路线同理:主 agent 拼好的 skill bodies 作为 system prompt 的一部分注入自己的 ReAct loop。

### 7. A vs B 实现轮廓(本 ADR 不决断)

**A 路线 — 自写 ReAct loop with skill body methodology injection**

- 主 agent 自己跑 ReAct loop,system prompt 含 5 个 diagnose_* skill body(按 planner 触发的子集)
- 模型:ADR-002 `router.For(TierAgent)` 即 ds-v4-pro
- 工作量:**~800-1000 行**(production-quality ReAct loop 不是 200 行;HolmesGPT `tool_calling_llm.py` ~800 行 + k8sgpt `analysis.go` ~500 行作为业界对照点)。拆解:ReAct loop 300-400 + tool dispatch 150-200 + result aggregation 100-150 + skill body injection 50-80 + adapter 100 + 失败恢复 100-150
- 优点:模型统一、无 vendor lock、跟 mutating sub-path 共享基础设施
- 缺点:LLM 自写 ReAct loop 容易踩坑(planner-style 漂移)+ 工作量是 B 的 ~1.6-1.8x

**B 路线 — Claude Code subprocess 委派**

- 主 agent `exec.Command("claude", ...)` 起子进程,通过 ModelVerse Anthropic-compat endpoint + ds-v4-pro 后端
- 走 reference memory `claude-code-provider-routing` 路径 A(env vars 直连,**绕过 CC Switch**;ModelVerse 已确认有 Anthropic-compat,2026-05-29 user confirm)
- skill bodies 通过 `--append-system-prompt` 注入(见 §6)
- 工作量:~550 行胶水(stream-json 翻译 200 + sanitizer 150 + 凭证生命周期 200)
- 优点:Claude Code 本身的 ReAct + tool loop + budget control 是产品级的,质量起点高
- 缺点:vendor 黑盒 + stream-json schema 跨版本风险 + `--allowed-tools` 实际 SSH 匹配粒度待 spike

**两路 trade-off 表**

| 维度 | A | B |
|---|---|---|
| 工作量 | ~800-1000 行 | ~550 行(B 显著省 30-45%) |
| ReAct 质量起点 | 自写不稳 | 产品级稳 |
| 模型 | ds-v4-pro 统一 | ds-v4-pro via ModelVerse Anthropic endpoint |
| 跨版本风险 | 低 | 中(Claude Code release + stream-json schema) |
| Vendor 依赖 | 无 | Claude Code CLI + ModelVerse Anthropic endpoint |
| Open exploration 适配度 | 中 | 高 |
| Closed plan 适配度 | 高 | 中(可用,但杀鸡用牛刀) |

### 8. 决断时机与触发条件

**本 ADR 不决断,Q2 真机故障 case 出现后定**。spike 需要:
- 真机故障 case ≥ 5 个(目前无,这是 user 2026-05-29 显式提到的不决断原因)
- 5 个 diagnose_* skill 至少 1 个升级到 `spike_validated` 状态
- A/B 双跑 N≥10 困难诊断回归(memory `jitter-check-for-classification`)
- 对比 metric:结论命中率 / EvidenceCmds 数量(over-explore 信号)/ 用户主观满意度

**决断阈值不预设**。当真实数据出来后,视实际差距和分布形态决定阈值线;必要时升 Claude Opus(成本翻倍)或 runbook 强约束(skill body 写死步骤,LLM 只填参数)作为 escape hatch;阈值定后回 amend 本 ADR(或新起后续 ADR)。

## Consequences

**Positive**
- 诊断跟 mutating 分流,各自走最合适的执行模型
- 5 个未验证 Chain Go 代码退役,减少维护噪音
- BillingAnomaly 归 fast tier 后 ADR-001 三 tier 边界恢复纯净
- 5 个诊断 skill 给 ADR-004 skill 框架带来真实 5+1 use case(非"为 deploy_model 一个 case 写的")
- `SubAgent` interface 让 A/B 并存,降低决策成本
- 复用 ADR-006 SSH sandbox / step trace / observability,不重复造
- B 路线 skill consumption 责任清晰(主 agent 拼,Claude Code 黑盒)

**Negative**
- `internal/diagnosis/` 包剩余 `subagent.go`(interface)+ `registry.go`(SubAgent registry,非 chain registry);其他 6 个 chain 文件删
- BillingAnomaly 迁移需保 backward compat(handler 接同样 input + 同 SSE 输出形态),memory `colleague-deploy-status` 显示生产已部署
- 5 个 unverified skill 上线前需 spike,否则 fab 风险高

**Risks**
- **A/B 都不达标**: 升 Opus 是 escape hatch,但成本是 deal breaker;runbook 强约束让诊断变 closed plan(失去 open exploration 价值);任一方案需要 ADR amendment
- **B 路线 `--allowed-tools` SSH 匹配粒度宽松**: `Bash(ssh user@target:*)` 里 `*` 实际能否阻断 shell injection 待 spike;不达标需主 agent 侧加 pre-validator(~100 行)
- **B 路线 stream-json schema 跨版本破坏**: 翻译层内嵌 schema version check + 失败 fallback;锁定 Claude Code 版本
- **5 unverified skill 永远卡在 unverified**: 升级到 spike_validated 需要真机故障 case + reviewer 投入;如果产品长期没真实故障流量,skill 一直带 CAUTION 行,LLM 行为偏保守 — 可接受,memory `corpus-input-source-tiering` 思路:与其升级假数据,不如保持 disclosure

## 业界对照

| 项目 | 诊断 sub-path | 实现 |
|---|---|---|
| k8sgpt | Analyzer interface + Failure | 自写 ReAct + 多 backend LLM(reference for A) |
| HolmesGPT | toolset.yaml + runbook + transformer | 自写,声明化 toolset + runbook(reference for A) |
| AWS Bedrock | Agent ReAct on Knowledge Base | foundation model driven(类 B) |
| Anthropic Computer Use | Subprocess + screenshot tool | model autonomous loop(类 B,但 desktop) |
| 本项目 | SubAgent interface + skill body methodology + A/B impl | 接口先行,impl 延后,内容(skill)先行 |

## Acceptance

- [ ] `internal/diagnosis/subagent.go` 新增,定义 `SubAgent` / `DiagCtx` / `Result` 等接口
- [ ] 5 个 unverified diagnose_* skill markdown 落地 `internal/skills/`(本 ADR 配套产出)
- [ ] BillingAnomaly 迁 fast tier:新 `capability_billing.go` + capability_registry 项 + 回归测试 + 旧文件删除(B5 范围,spec 见 `docs/plans/2026-05-29-billing-anomaly-to-fast-tier.md`)
- [ ] 5 个 unverified Chain Go 代码删除(`ssh_failure.go` / `init_failure.go` / `gpu_not_detected.go` / `port_firewall.go` / `image_issue.go` + 对应 `*_test.go`)
- [ ] `internal/diagnosis/registry.go` 改造:从 chainRegistry 改为 SubAgent registry,只剩 BillingAnomaly 临时项删除后整个文件可能也删
- [ ] Engine agent path dispatch 加 `agent_subtype=diagnosis` 分支,走 `SubAgent.Run()`
- [ ] SubAgent contract test 框架,跑 fixture 化的"症状 → 期望 conclusion / 必跑命令集合"
- [ ] 至少 1 个 SubAgent impl 投产(A 路线先行,因为不引入外部 vendor 依赖)
- [ ] 至少 1 个 diagnose_* skill 升级 `spike_validated`
- [ ] Q2 真机故障 case + spike + 决断 ADR-005 amendment(或新起后续 ADR)

## References

- ADR-001 / ADR-002 / ADR-004 / ADR-006: 前置依赖
- memory `claude-code-provider-routing`: B 路线 backend 路由路径
- memory `industry-agent-patterns-2026-05-28`: k8sgpt + HolmesGPT 借鉴点
- memory `jitter-check-for-classification`: N=10 决断方法论
- memory `corpus-input-source-tiering`: unverified skill body 加 CAUTION 行的相邻 fab 风险防护
- memory `routing-verification-via-trace-not-latency`: 诊断 sub-tier 命中率验证靠 trace
- `docs/plans/2026-05-29-billing-anomaly-to-fast-tier.md`: BillingAnomaly 迁移 spec
