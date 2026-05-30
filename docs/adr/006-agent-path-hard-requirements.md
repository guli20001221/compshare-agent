# ADR-006: Agent Path 6 Hard Requirements

**Status**: Provisionally Accepted (2026-05-31,revert-if-regression;lead 拍板 provisional-accept 瘦身版以解锁 B6 实施,full ratification deferred;原 Proposed 2026-05-29)
**Depends on**: ADR-001(三路分层定义 agent tier)/ ADR-002(强模型路由)/ ADR-003(skill + tool 正交)
**Scope**: agent tier 内部架构,fast/knowledge tier 不受本 ADR 影响

## Context

ADR-001 定义 agent tier 处理"多步任务/部署/排障"类请求。chatbot-grade 单轮 LLM 调用不足以处理这类请求,需要 6 项 agent-grade 能力。当前代码 5/6 缺失或不达标。

业界对照(AWS Bedrock Agent / OpenAI Assistants Run / Anthropic Computer Use / k8sgpt / HolmesGPT)在这 6 项上均有完整实现,本 ADR 把每项 gap 翻译为本项目的具体接口和文件。

## 6 项硬要求总览

| # | 要求 | 当前状态 | 本 ADR 决定 |
|---|---|---|---|
| 1 | Step trace | per-turn JSONL,无 step 粒度 | **定义新 `StepTrace`**(非复用 `workflow.StepEvent`),序列化进 `trace_json.steps[]`,零 DDL |
| 2 | Saga rollback | workflow 单步失败即停,无 compensation | **B6 deferred / amended(lead 2026-05-31)**:实例创建**不回滚**(create-fail 不计费→无副作用;create-success=用户资源,经 context/trace 知晓由用户决定保留或控制台删;auto-Terminate 是永不自动执行的 L2 边界)。`Step.Compensate` 字段保留(B6.1 declared-not-consumed),倒序补偿执行**推迟**到将来出现"可逆非破坏副作用"流程再做;saga 硬拒 forward 步与(将来)compensate 中的 L2 动作 |
| 3 | Multi-step HITL | ConfirmFunc 单步同步 | 复用已 ship 的内存 `ConfirmBroker`,saga 在单个活请求内同步跑完(无跨进程暂停-恢复) |
| 4 | SSH sandbox | 未实现 | **B6 范围外,decision deferred**(两路候选:自建 sandbox / Claude Code sub-agent,路线不锁) |
| 5 | 强模型 | 单 model | ADR-002 router.For(TierAgent) |
| 6 | Idempotency key | 平台 API 侧有,agent 层无去重 | **create/mutating 不自动重试(`maxRetries=0`),不引入 idempotency key / dedup 表** |

下面逐项展开。

## 决策 1:Step Trace(粒度从 turn 升 step)

**Why**: agent path 一个 turn 可能 5-15 步,UI 要逐步 emit,出错时需要回放定位到具体 step。

**方案**: **定义新 `StepTrace`**(observability 包),序列化进现有 `trace_json`,不新建表、不加列。

> **为何不复用 `workflow.StepEvent`**:`workflow.StepEvent`(`internal/workflow/types.go:71-80`,8 字段 `StepName/StepIndex/Total/Type/Status/Tool/Args/Message`,**无 json tag,CLI-display-only**)与下面的 `StepTrace`(13 个 json-tagged 字段,持久化用)字段集不同,是 **define-fresh 不是 reuse**。

```go
// internal/observability/step_trace.go (新增)
type StepTrace struct {
    SessionID       string    `json:"session_id"`
    TurnID          string    `json:"turn_id"`
    StepID          string    `json:"step_id"`           // uuid per step
    SagaID          string    `json:"saga_id,omitempty"` // multi-step task 组 id
    SkillID         string    `json:"skill_id,omitempty"` // ADR-003 skill 名
    Tool            string    `json:"tool,omitempty"`
    Args            map[string]any `json:"args,omitempty"`
    State           StepState `json:"state"` // pending/running/awaiting_confirm/success/failed/compensated/timeout
    Result          any       `json:"result,omitempty"`
    ErrorCategory   string    `json:"error_category,omitempty"` // 自由 string:user_abort/api_error/timeout/...
    StartedAt       time.Time `json:"started_at"`
    EndedAt         time.Time `json:"ended_at,omitempty"`
    CompensateOf    string    `json:"compensate_of,omitempty"` // 指向被补偿的 step_id
}
```

saga runner 在每个状态转换产 `StepTrace`,通过 read-modify-write 追加进**该 turn** 行的 `trace_json.steps[]`;HTTP 路径同时 fan 到 SSE channel 实时收。

**Acceptance**: `StepTrace` 序列化进 `trace_json.steps[]`;**不新建表、不加列、不改 `agent_traces` 结构**(零 DDL);`EmitStep` = read-modify-write 该 turn 行的 `steps[]`,**绝不 per-step INSERT**(否则撞 `uk_request_uuid`);server SSE 每 step 一条 `event: step` payload;5-step+ 工作流端到端可回放。`observability.Writer` 接口加 `EmitStep(StepTrace)` 方法,`EmitTurn` 不动。

## 决策 2:Saga + Compensating Action

> **Amendment(lead 2026-05-31,B6 实施期)**:**实例创建不进入自动回滚**。原 Why 假设"已创建实例必须能回滚否则资源泄漏",lead 复核后修正:① create 失败本就不创建资源、**不计费**,无可回滚副作用;② create 成功后实例是**用户拥有的合法资源**,后续步骤失败不应自动销毁它(销毁 = `TerminateCompShareInstance` = L2,正是我们**永不自动执行**的不可逆操作);③ agent 做过什么**经 context + `trace_json.steps[]` 完全可知**,由用户决定保留还是去控制台删。**后果**:倒序补偿**执行**推迟到将来真出现"可逆、非破坏、非计费"副作用的流程(当前 create/deploy 流程都没有);`Step.Compensate` 维持 B6.1 的 declared-not-consumed;saga **硬拒** forward 步与(将来)compensate 中任何 L2/destructive 动作(`security.Check`),从结构上杜绝"自动删实例"。B6 因此交付 6 项中的 **4 项**(#1 step-trace / #3 HITL / #5 强模型 / #6 create 不重试),#2 compensation deferred、#4 SSH deferred。下文保留原 #2 设计作为将来补偿落地时的蓝图。

**Why**(原始,部分被上方 Amendment 修正): 部署类任务(deploy_model skill 6 步)中间失败,已创建实例 / 已配置 SG 等副作用必须能回滚,否则资源泄漏。

**方案**: `workflow.Step` 加 `Compensate` 字段,Engine 维护 success-stack;失败时倒序触发 compensate。

```go
// internal/workflow/types.go 扩展
type Step struct {
    Name        string
    Type        StepType
    Tool        string
    BuildArgs   func(*Context) (map[string]any, error)
    CheckResult func(*Context, map[string]any) (bool, string)
    // NEW: compensating action triggered on later-step failure.
    // nil 表示该 step 无副作用、无需补偿(read-only 或 idempotent setter)
    Compensate  *CompensateStep
}

type CompensateStep struct {
    Tool      string
    BuildArgs func(*Context, stepResult map[string]any) (map[string]any, error)
    // BestEffort: true → compensate 失败只 log 不阻塞后续 compensate
    BestEffort bool
}
```

Engine 改造:`Run()` 内维护 `executedSteps []executedStep`,失败分支调用 `runCompensation(ctx, executedSteps)`,倒序遍历跑 compensate。compensate 也产 `StepTrace`(state=`compensated`、`compensate_of=<原 step_id>`)。

**Timeline 序约束**(本 ADR + ADR-002 联合):

| 层 | 来源 | Default | 触发后行为 |
|---|---|---|---|
| LLM call timeout | ADR-002 `tier_routing.agent.timeout_ms` | 180s | LLM call abort,step 仍可走 ReAct fallback / retry |
| Step timeout | 本 ADR `workflow.Step.Timeout` 字段(新增) | **240s = 4 min** | Step 主动 cancel,emit `StepTrace{State:timeout, ErrorCategory:timeout}`,saga 进入失败分支触发 compensate |

两层序保证 `180s < 240s`:LLM call 卡时 step 仍可观测(saga 不会误以为 step 已死)/ step timeout 时 saga 主动收口。`Step.Timeout` 可 per-step override(例如 deploy_model 步骤 6 轮询启动可设 10 min)。Per-step override > 4 min 时 build-time 警告,避免静默放宽。

**Risks**:
- **Compensate 自身失败**:`BestEffort` flag 控制是 abort 还是 continue。**默认 BestEffort=true**,因为"部分回滚 + 提示用户去控制台" 比 "rollback 卡死" 更稳
- **不可逆操作 不能写 compensate**:例如 `DeleteInstance` 一旦执行无法回滚,skill 必须在 ConfirmFunc 阶段拦死,不能放进 saga

**Acceptance**(原始,被上方 Amendment 取代): ~~至少 deploy_model skill 6 步实现完整 saga + 1 个 E2E test 模拟步骤 4 失败回滚步骤 3/2(创建实例 + 配置 SG)。~~ → **改为(lead 2026-05-31)**:multi-step flow 失败 = stop + report(已创建实例经 trace/context 暴露,**不自动删**);E2E test 断言 ①create 失败不重试、返回 `ConfirmFunc`;②saga forward 步与 compensate 字段中**无 L2 动作**(`security.Check` 守);③create 步**不带 Compensate**。倒序补偿执行 deferred。

## 决策 3:Multi-Step HITL(复用内存 ConfirmBroker,无跨进程暂停-恢复)

**Why**: 当前 `ConfirmFunc(action, args) bool`(`workflow/types.go:68`)是同步阻塞。agent path 多步任务需要逐步确认,但**不需要跨进程持久暂停-恢复**(lead 2026-05-30 拍板):saga 在一个活请求内同步跑完,中途失败内存倒序补偿即可。

**方案**: agent-tier 确认**复用已 ship 的内存 `ConfirmBroker`**——`internal/httpapi/confirm_broker.go` + `ConfirmCSAgentAction` Action(`internal/httpapi/dispatch.go:36`)+ SSE `event: confirmation`(`internal/httpapi/handlers_chat.go:353`,`confirmationEvent` 结构 `:75`)。reviewer 核实该链路已实现**单步异步确认**:server SSE emit `confirmation` 帧 → frontend POST `ConfirmCSAgentAction` → broker 在同一活请求内放行被阻塞的 goroutine。

- saga 在**单个活请求内同步跑完**;中途失败 → **内存倒序补偿**(决策 2),**不持久化、不跨进程恢复**。
- CLI 端:`cliConfirm`(`cmd/agent.go::cliConfirm`)同步等用户输入,跟现有路径一致。

**不引入**:`PauseToken` / `Resume` 协议 / `saga_pauses` 表 / `requires_action` SSE / `ResumeSaga` Action —— 全部砍掉(无跨进程持久暂停-恢复需求)。

**Acceptance**: agent-tier saga 通过内存 `ConfirmBroker` 在单请求内逐步确认;无任何持久化或跨进程 resume 路径;不新建 `saga_pauses` 表(零 DDL)。

## 决策 4:SSH 诊断 —— B6 范围外,decision deferred

**状态(lead 2026-05-30)**:SSH-into-instance 诊断**整体暂缓,B6 范围外,路线不锁**。设计未定,且 B6 spine(saga/HITL/idempotency)不依赖它。平台无 SSM-style command-exec API(只有 direct SSH),原 ADR 设想的"sandbox + 结构化白名单 + STS 临时凭据"实现细节随之搁置。

**两路候选(将来单独决策,现在不选)**:
- **(a) 自建 sandbox** —— 自有 Go-side SSH 诊断 skill,凭据 + 结构化白名单 + output cap 走 `internal/security/`(原 §决策4 设想,minus 平台缺的 SSM API)。
- **(b) Claude Code sub-agent** —— consent-gated 受限 sub-agent SSH 进实例,复用 Claude Code agent loop + `PreToolUse` hook 白名单,跑在 ds-v4(ModelVerse ds-v4 原生 Anthropic-format-兼容,路由干净)。

**带进将来设计的固定约束**(起步即 grounded):平台无 SSM-style API —— direct SSH only;须显式用户 **consent**;只允许 **whitelisted** 只读命令(deny-by-default);凭据须 **scoped/ephemeral**,绝不存持久 key;落地时触发 **ADR-006 §决策4 amendment** + `internal/prompt/segment_readonly.go:11`("助手不能 SSH 登录实例")边界文案修订(若走 Claude-Code/ds-v4 路另加 ADR-002 note)。

**对 B6 的后果**:B6 交付 ADR-006 **6 项中的 5 项**(#1 step-trace / #2 saga / #3 HITL / #5 强模型 / #6 由"create 不重试"替代);**#4 SSH deferred** 到将来批次 + 决策,不当待实现子相位。

## 决策 5:Agent Path 走强模型

直接引用 ADR-002 — `router.For(TierAgent)` 返回 ds-v4-pro client。本 ADR 不重复决策。

**Acceptance**: Engine agent path 任何 LLM 调用都通过 router,grep `r.For(TierAgent)` 必须 ≥1 处。

## 决策 6:create/mutating 不自动重试(替代 idempotency key)

**Why**: 双重创建只来自重试。**不重试即无需幂等基础设施**(lead 2026-05-30 拍板)。

**方案**: create/mutating action **不自动重试(`maxRetries=0`)**,失败直接返回走 `ConfirmFunc` 让用户重新确认;**不引入 idempotency key / dedup 表**。

- 删 `Step.IdempotencyKey` 机制 + 幂等去重表(原设想的 `hash(skill_id, step_index, args)` + MySQL 一行 lookup 全砍)。
- **并发锁内存级**:同 session 并发请求由 `internal/agentpool/pool.go:55` 的 `entry.mu`(per-session `sync.Mutex`,"serializes per-session engine access")已串行化,**无需** saga-level MySQL `UNIQUE(session_id, skill_id)` 锁。

> **待核实(实现时)**:`SafeToolExecutor` 重试是否按 action 类别可配。若一刀切,需给 mutating/create 类加 `maxRetries=0` 分类(Go 改动)。reviewer 未核此点。

**Acceptance**: create/mutating action 失败不自动重试,直接走 `ConfirmFunc` 重新确认;无 idempotency key、无 dedup 表、无 saga-level MySQL 锁(零 DDL)。

## 整体 Consequences

**Positive**
- B6 交付 6 项中的 **4 项**(#1 step-trace / #3 HITL / #5 强模型 / #6 create 不重试;**#2 compensation + #4 SSH deferred**,见 §决策2 Amendment),对齐头部 cloud agent(Bedrock / OpenAI / Anthropic / k8sgpt / HolmesGPT)的 step-trace/HITL/强模型 能力
- 每项决策都有明确文件/接口/Acceptance,直接驱动 B6 实施
- **DDL 归零**:不碰数据库表结构,step 全进 `trace_json`;saga 单请求内同步跑完,中途失败 **stop + report**(已创建实例经 `trace_json.steps[]` + context 暴露,由用户决定保留或控制台删,**不自动删**),让用户敢把"创建实例 + 部署模型"这类链路交给 agent

**Negative**
- B6 工作量:`internal/orchestrator/`(~424 行,agent 层 step runner,**无倒序补偿执行**)+ observability recorder 接线(EmitStep 累积)→ 净新增/改造比原估(~1150 行含补偿)小(补偿执行 + SSH 均 deferred 出 B6)
- agent 层 step 状态机增加 cognitive load,新人上手需数天(补偿 deferred,**无倒序补偿 cognitive load**)

**Risks**
- **Orchestrator 写成 framework 的诱惑** → 反对,**见 ADR-007**(memory `no-graph-framework-for-agent` 是原始数据来源,ADR-007 已结构化为 anti-pattern 决策 + 触发条件 + Q3 重审 governance),只写够当前要求的 ~800 行
- **SSH 诊断 deferred 出 B6** → 设计未定,两路候选(自建 sandbox / Claude Code sub-agent)单独决策,不在 B6 关键路径(§决策4)
- **StepTrace PII** → `EmitStep` 经 `prepareForPersist` choke-point 脱敏(Args/Result);read-modify-write 同 turn 行,绝不 per-step INSERT(撞 `uk_request_uuid`)

## Acceptance(整 ADR)

> **DDL 归零**:没有 0004、没有 0005、没有任何 migration;B6 不碰数据库表结构。

- [ ] `internal/orchestrator/` 目录建立,含 `step.go` / `saga.go` / `hitl.go` / `loop.go` 4 个核心文件(纯内存 saga runner)
- [ ] `workflow.Step` 加 `Compensate` 字段(**不加 `IdempotencyKey`**),所有现有 workflow 显式声明(read-only / 不可逆 设 nil)
- [ ] `internal/observability/step_trace.go` 定义新 `StepTrace`(非复用 `workflow.StepEvent`);`StepTrace` 序列化进 `trace_json.steps[]`;`EmitStep` = read-modify-write 该 turn 行,**绝不 per-step INSERT**(撞 `uk_request_uuid`);不新建表、不加列、不改 `agent_traces` 结构
- [ ] agent-tier 确认复用内存 `ConfirmBroker`(`internal/httpapi/confirm_broker.go` + `ConfirmCSAgentAction` + SSE `event: confirmation`);saga 单请求内同步跑完,中途失败内存倒序补偿;无 `PauseToken` / `ResumeSaga` / `saga_pauses`
- [ ] create/mutating action `maxRetries=0` 不自动重试,失败走 `ConfirmFunc` 重新确认;无 idempotency key、无 dedup 表
- [ ] 并发由 `agentpool` per-session `entry.mu`(`pool.go:55`)串行化;无 saga-level MySQL 锁
- [ ] SSH 诊断(#4)deferred 出 B6(§决策4),不在本 ADR 验收范围
- [ ] **倒序补偿执行(#2)deferred(lead 2026-05-31,见 §决策2 Amendment):实例创建不自动回滚;`Step.Compensate` 维持 declared-not-consumed;saga 硬拒 forward/compensate 中的 L2 动作**
- [ ] 1 个端到端 flow(`CreateInstanceWorkflow`-as-saga,B8 接 `deploy_model`)跑通 **4 项**要求(#1 step-trace / #3 HITL / #5 强模型 / #6 create 不重试;#2 compensation + #4 SSH deferred),作为 B6 验收 demo

## References

- ADR-001 / ADR-002 / ADR-003: 前置依赖
- memory `no-graph-framework-for-agent`: 反对引入 Eino/LangGraph 的理由
- memory `ssh-diagnostics-deferred-two-routes`: SSH 暂缓 + 两路候选(自建 sandbox / Claude Code sub-agent)
- MUSE 2605.27366 §4.5: 自进化 skill 风险(hardcoded 标识符 sanitize),写进将来 B9 约束(见下)
- k8sgpt Analyzer + Failure: ADR-005(诊断包重构,待写)消费本 ADR 的 step trace

## 将来约束(B9 自进化,非 B6 范围)

- **自进化走文件不走 SQL**(参考 MUSE 2605.27366):skill-level memory = per-skill `.memory.md` sibling 文件(append-only markdown),不进数据库;连"为将来自进化预留 `skill_id` 列"也不需要。
- **MUSE 自进化风险**(hvac-control 回归 80%→20%,MUSE §4.5 case iv):自进化 skill 会把单次运行的校准常数/路径/数值范围烤进 body。**接受 refined skill 前必须 sanitize 硬编码标识符**(project_id / instance_id / region / RetCode / 配额数字)—— compshare V100S RetCode=230 同源。**这是 B9 约束,不是 B6 scope。**
