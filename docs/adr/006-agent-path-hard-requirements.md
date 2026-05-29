# ADR-006: Agent Path 6 Hard Requirements

**Status**: Proposed (2026-05-29)
**Depends on**: ADR-001(三路分层定义 agent tier)/ ADR-002(强模型路由)/ ADR-003(skill + tool 正交)
**Scope**: agent tier 内部架构,fast/knowledge tier 不受本 ADR 影响

## Context

ADR-001 定义 agent tier 处理"多步任务/部署/排障"类请求。chatbot-grade 单轮 LLM 调用不足以处理这类请求,需要 6 项 agent-grade 能力。当前代码 5/6 缺失或不达标。

业界对照(AWS Bedrock Agent / OpenAI Assistants Run / Anthropic Computer Use / k8sgpt / HolmesGPT)在这 6 项上均有完整实现,本 ADR 把每项 gap 翻译为本项目的具体接口和文件。

## 6 项硬要求总览

| # | 要求 | 当前状态 | 本 ADR 决定 |
|---|---|---|---|
| 1 | Step trace | per-turn JSONL,无 step 粒度 | 复用 `workflow.StepEvent`,observability 加 step_id/saga_id/skill_id |
| 2 | Saga rollback | workflow 单步失败即停,无 compensation | `Step.Compensate` 字段 + 倒序触发 |
| 3 | Multi-step HITL | ConfirmFunc 单步同步 | `PauseToken` + Resume 协议(OpenAI requires_action 风格) |
| 4 | SSH sandbox | 未实现 | 新建 `internal/security/ssh_sandbox.go`,命令白名单 + STS 临时凭据 |
| 5 | 强模型 | 单 model | ADR-002 router.For(TierAgent) |
| 6 | Idempotency key | 平台 API 侧有,agent 层无去重 | `Step.IdempotencyKey = hash(skill_id, step_index, args)` |

下面逐项展开。

## 决策 1:Step Trace(粒度从 turn 升 step)

**Why**: agent path 一个 turn 可能 5-15 步,UI 要逐步 emit,出错时需要回放定位到具体 step。

**方案**: 复用 `internal/workflow/types.go:70-81` 现有 `StepEvent`(workflow 包已有),扩展 observability schema。

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
    State           StepState `json:"state"` // pending/running/awaiting_confirm/success/failed/compensated
    Result          any       `json:"result,omitempty"`
    ErrorCategory   string    `json:"error_category,omitempty"` // user_abort/api_error/timeout/...
    StartedAt       time.Time `json:"started_at"`
    EndedAt         time.Time `json:"ended_at,omitempty"`
    CompensateOf    string    `json:"compensate_of,omitempty"` // 指向被补偿的 step_id
}
```

`workflow.Engine.emit()`(`engine.go:46/53/57` 等 6 处)直接产 `StepTrace` 落库,UI 通过 SSE channel 实时收。

**Acceptance**: server SSE 每 step 一条 `event: step` payload;**`migrations/0004_alter_agent_traces.sql` 在现有 `agent_traces` 表 ALTER 加 `step_id` / `saga_id` / `skill_id` / `task_tier` 列(nullable,旧 per-turn 行兼容)**,不新建子表;5-step+ 工作流端到端可回放。`observability.Writer` 接口加 `EmitStep(StepTrace)` 方法,`EmitTurn` 不动。

## 决策 2:Saga + Compensating Action

**Why**: 部署类任务(deploy_model skill 6 步)中间失败,已创建实例 / 已配置 SG 等副作用必须能回滚,否则资源泄漏。

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
| Step timeout | 本 ADR `workflow.Step.Timeout` 字段(新增) | **240s = 4 min** | Step 主动 cancel,emit `StepTrace.ErrorCategory=timeout`,saga 进入失败分支触发 compensate |
| PauseToken ExpiresAt | Decision 3 | **30 min** | Saga 收口 abort,触发全 compensate;过期 token 走定时 GC |

序保证 `180s < 240s < 30min`:LLM call 卡时 step 仍可观测(saga 不会误以为 step 已死)/ step timeout 时 saga 主动收口 / pause 长但有上限。`Step.Timeout` 可 per-step override(例如 deploy_model 步骤 6 轮询启动可设 10 min)。Per-step override > 4 min 时 build-time 警告,避免静默放宽。

**Risks**:
- **Compensate 自身失败**:`BestEffort` flag 控制是 abort 还是 continue。**默认 BestEffort=true**,因为"部分回滚 + 提示用户去控制台" 比 "rollback 卡死" 更稳
- **不可逆操作 不能写 compensate**:例如 `DeleteInstance` 一旦执行无法回滚,skill 必须在 ConfirmFunc 阶段拦死,不能放进 saga

**Acceptance**: 至少 deploy_model skill 6 步实现完整 saga + 1 个 E2E test 模拟步骤 4 失败回滚步骤 3/2(创建实例 + 配置 SG)。

## 决策 3:Multi-Step HITL(Pause + Resume)

**Why**: 当前 `ConfirmFunc(action, args) bool`(`workflow/types.go:68`)是同步阻塞;HTTP 服务模式下不能阻塞 goroutine 等用户点击。OpenAI Assistants 用 `run.status=requires_action` + resume 解决,本项目对齐。

**方案**: `ConfirmFunc` 退化为 fast path / CLI 同步场景;agent path 引入 `PauseToken` + `Resume` 协议。

```go
// internal/orchestrator/hitl.go (新增)
type PauseToken struct {
    TokenID    string         `json:"token_id"`
    SessionID  string         `json:"session_id"`
    SagaID     string         `json:"saga_id"`
    StepID     string         `json:"step_id"`
    Reason     PauseReason    `json:"reason"` // confirm / select_option / ssh_approval
    Prompt     string         `json:"prompt"`
    Options    []PauseOption  `json:"options,omitempty"`
    ExpiresAt  time.Time      `json:"expires_at"` // default 30 min;deploy_model 等典型 10-30 min,5 min 会触发刚部署一半 rollback;per-skill 可 override 更长
}

type Decision struct {
    TokenID  string `json:"token_id"`
    Accepted bool   `json:"accepted"`
    Selected string `json:"selected,omitempty"`
    Note     string `json:"note,omitempty"`
}

type HITLStore interface {
    Park(token PauseToken, ctx SagaContext) error
    Resume(tokenID string, decision Decision) (SagaContext, error)
    Expire(now time.Time) []string // 返回过期的 tokenID
}
```

HTTP server 收到 saga 暂停时,SSE emit `event: requires_action` + token,持久化 saga 状态到 MySQL(`saga_pauses` 表);用户在前端点击后,frontend POST `Action=ResumeSaga`,server 加载 saga + 继续执行。

CLI 端简化:`cliConfirm`(`cmd/agent.go::cliConfirm`)实现 HITLStore,Park 立即同步等用户输入。

**Acceptance**: HTTP server 暂停 → 持久化 → 用户 30 秒后回来 → resume 正确续跑;过期 token GC 走定时任务。

## 决策 4:SSH Sandbox

**Why**: "SSH 进实例排障" 是 agent path 核心场景。LLM 直接发命令到用户实例风险极高(rm -rf / 权限提升 / 数据泄漏)。需要命令白名单 + STS 临时凭据。

**方案**: 新建 `internal/security/ssh_sandbox.go`,作为 `OpenSSHSession` tool 的实现层。

**分层 rationale**:tool spec(`internal/tools/open_ssh_session.json`)是声明接口对 LLM 可见,实现细节(STS / 白名单匹配 / output cap / sanitizer)放 `internal/security/` — memory `internal/security is secret boundary, not product policy` 同源原则,security 包负责 boundary enforcement(谁能跑什么 / 输出脱敏),tool 包只暴露 LLM 可见的 schema。两者解耦:换 sandbox 实现(SSM API vs console proxy)不动 tool spec,改 tool spec(加 timeout 参数等)不动 security policy。

```go
// internal/security/ssh_sandbox.go (新增,~400 行)
type SSHSandbox struct {
    allowedCmds CommandPolicy // structural-grammar allowlist,NOT regex
    stsClient   STSClient     // 复用现有 STS 实现(在 internal/config STS block + internal/httpapi handlers 的 AssumeRole 链路,非独立 auth 包),SSH key 是 STS 衍生子系统
    sessionMax  time.Duration // default 10 min
    outputCap   int           // default 8 KB per command
}

type CommandPolicy struct {
    // 结构化白名单:cmd 名 + 允许的 flag 集
    // 不用 regex(易绕过);用 AST-level allowlist
    ReadOnly  []CommandPattern // nvidia-smi / df / cat /var/log/* / journalctl 等
    Diagnose  []CommandPattern // systemctl status / dmesg / lsmod 等
    // Mutating 类 (apt-get / kill / rm) 永不允许,必须人工 SSH
}

func (s *SSHSandbox) Run(ctx context.Context, instanceID, cmd string) (SSHResult, error) {
    // 1. AssumeRole 拿临时 STS 凭据(现有链路在 internal/config STS block + internal/httpapi handlers)
    // 2. policy.Match(cmd) → reject 不在白名单
    // 3. 通过 console proxy 或 SSM session 跑 command(NOT 直接开 SSH 隧道)
    // 4. cap output 8KB,超出截断 + 提示
    // 5. emit StepTrace(state=success/failed, sensitive 字段走 sanitizer)
}
```

**关键决定**:
- **不开真 TCP SSH 隧道**,通过 CompShare 控制台代理或 SSM-style API(平台需提供) — 避免 agent 进程持有用户 SSH 私钥
- **白名单是结构化的**,不用 regex(memory `l0-stop-grow-dictionary` 教训:hand-maintained list 必须有 stop-grow ceiling + 结构化替代)
- **任何不在白名单的 command**,skill 引导用户自己 SSH 进去执行,agent 只观察 stdout(用户粘贴)

#### V1 白名单起步 10 条(2026-05-29 定)

每条 argv-level 结构化(**无 pipe / 无 redirect / 无 shell substitution**),sandbox 端做 post-filter,5 个 diagnose_* skill 都用得上:

| # | 命令(structural argv) | 用途 | skill 关联 | argv 约束 |
|---|---|---|---|---|
| 1 | `nvidia-smi` | GPU 状态 / driver / process | diagnose_gpu_not_detected | 无参 |
| 2 | `nvidia-smi -q` | GPU 详细 query | diagnose_gpu_not_detected | 唯一允许 flag = `-q` |
| 3 | `lsmod` | 内核模块列表 | diagnose_gpu_not_detected | 无参;sandbox 端 grep `nvidia` post-filter |
| 4 | `lspci` | PCI 设备 | diagnose_gpu_not_detected | 无参;sandbox 端 grep `nvidia\|VGA` post-filter |
| 5 | `dmesg --since=<duration>` | 近期 kernel msg | diagnose_init_failure / diagnose_gpu_not_detected | `--since` 必填,值 `[0-9]+ (minutes\|hours) ago` 形态;output cap 8KB |
| 6 | `systemctl status <name>` | service 状态 | diagnose_init_failure / diagnose_image_issue | **name 走 enum 白名单**(V1):`ssh` / `sshd` / `nginx` / `docker` / `containerd` / `kubelet` / `jupyter` / `filebrowser` / `cron` / `dbus` 共 10 个;不在白名单 reject。V2 视使用情况扩。**取消 regex 形态**防 LLM recon 探测内部 service 名 |
| 7 | `journalctl -u <name> --no-pager -n <N>` | service log | diagnose_init_failure / diagnose_image_issue | name 同 #6 enum 白名单;`-n` 上限 200;`--no-pager` 强制 |
| 8 | `df -h` | 磁盘占用 | diagnose_image_issue / diagnose_init_failure | 无参 |
| 9 | `free -h` | 内存占用 | diagnose_ssh / diagnose_init_failure | 无参 |
| 10 | `uptime` | load average | diagnose_ssh / diagnose_init_failure | 无参 |
| 11 | `cat /proc/cpuinfo` | CPU 型号 / 核数 | diagnose_init_failure / GPU 配置确认 | 路径硬编码,只这一个 /proc 文件 |
| 12 | `cat /proc/meminfo` | 内存详情(MemAvailable / Buffers / Cached) | diagnose_init_failure / diagnose_ssh | 路径硬编码,只这一个 /proc 文件 |

#### V1 范围外(后续按需逐条 issue 评审加)

- `ss -tlnp` / `iptables -L`(端口/防火墙,PortFirewall 用)— `ss` 部分场景需 root,`iptables` 必须 root,**v1 永不允许**
- `cat /var/log/<path>`(日志查看)— 路径白名单需要单独定义,v2
- `lsblk`(磁盘块)— 可加,v2
- `ip addr show` / `ip route`(网络配置)— 可加,v2
- 其他 `/proc/*` 路径(`/proc/loadavg` / `/proc/stat` 等)— 可加,v2(V1 只允许 cpuinfo + meminfo 两条防 grow)
- 任何 `apt-get` / `yum` / `systemctl restart|start|stop` / `kill` / `rm` 等 mutating — **永不允许**(超出 read-only 边界)

#### 不需要 SSH 命令的 skill

- `diagnose_ssh`:SSH 连不上本身就是连不进去,SSH command 没用;仅用 monitor API + 引导用户 JupyterLab 自查
- `diagnose_port_firewall`:用户自定义服务端口排查超出云侧能力,引导用户实例内只读自查
- 已迁 fast tier 的 `billing_instance`:纯 API 查询,不走 SSH

**Risks**:
- **白名单维护成本** → V1 起步 10 条,每加一条要 issue 评审,memo 在 ADR-006 后续 amendment;memory `l0-stop-grow-dictionary` 防止 list 无限增长
- **systemctl / journalctl 的 name 参数白名单** → V1 走 enum 10 个 service 名白名单(防 LLM recon 探测客户实例内部跑了什么);V2 视实际使用情况扩;memory `l0-stop-grow-dictionary` 思路 — 每加一条 service 名 require explicit review
- **平台不支持 SSM-style API** → fallback 方案是 console proxy(用户已登录控制台的 session),性能差但安全

**Acceptance**: V1 白名单 **12 条命令**上线(GPU 诊断 4 + kernel/log 3 + 资源 3 + /proc 2)→ E2E test 验证 `rm -rf /` / `nvidia-smi; echo pwned` / `systemctl restart ssh` / `systemctl status some-internal-name`(不在 enum)/ `cat /proc/self/mem` 等被 reject;sensitive(IP / hostname / 用户名 / 路径)经过 sanitizer 不入 trace;每条白名单命令至少 1 个正常 case + 1 个 argv 篡改 case 测试覆盖。

## 决策 5:Agent Path 走强模型

直接引用 ADR-002 — `router.For(TierAgent)` 返回 ds-v4-pro client。本 ADR 不重复决策。

**Acceptance**: Engine agent path 任何 LLM 调用都通过 router,grep `r.For(TierAgent)` 必须 ≥1 处。

## 决策 6:Idempotency Key

**Why**: Network partition / 用户刷新 / saga resume 都可能导致同一 step 重复触发。平台 API(CompShare OpenAPI)很多 endpoint 是 idempotent 的,但 agent 层不传 key 等于不去重。

**方案**: `workflow.Step` 加 `IdempotencyKey func(*Context) string`,Engine 在执行前 hash(skill_id + step_index + canonical_args) → 传给 executor → executor 注入到 API request header(平台侧已支持 `X-Idempotency-Key`)。

```go
type Step struct {
    // ...existing fields...
    IdempotencyKey func(*Context) string // 默认实现 = hash(skill_id, step_index, args)
}
```

Agent 内部记录(session_id, saga_id, step_id, idem_key, result) → step 重跑时直接返回 cached result(MySQL 一行 lookup);跨 session 不去重(粒度过粗 + 用户预期是独立 saga)。

**同 session 并发 saga 防护**:用户开 2 个 tab 用同 session_id 并发触发同一 skill 的 saga(deploy_model × 2),idempotency_key 同 step 级 dedupe 但 saga 状态机两 goroutine 并发写有数据竞态。**Saga-level lock 按 `(session_id, skill_id)` 取 MySQL row lock(`saga_pauses` 表加唯一约束 + INSERT IGNORE 抢锁)** — 同 session + 同 skill 只允许 1 个 active saga,后到的返回"该 saga 已在跑,等完成或人工 cancel"。**跨 skill 同 session 允许并发**(用户可能同时跑 deploy + 查计费)。

**Acceptance**: 模拟 saga 步骤 3 后断网,resume 时步骤 1/2 不重新调 API(cached);步骤 3 走 idempotency key 不重复创建实例。

## 整体 Consequences

**Positive**
- 6 项硬要求全数对齐头部 cloud agent(Bedrock / OpenAI / Anthropic / k8sgpt / HolmesGPT)
- 每项决策都有明确文件/接口/Acceptance,直接驱动 B6 实施
- Saga + idempotency 让用户敢把"创建实例 + 部署模型"这类不可逆链路交给 agent

**Negative**
- B6 工作量是 8 批里最大:`internal/orchestrator/`(~800 行)+ `internal/security/ssh_sandbox.go`(~400 行)+ workflow 包扩展(~200 行)+ observability step 化(~150 行)+ HTTP server resume route + saga_pauses 表 schema → ~2000 行新增/改造
- HITL + saga 状态机增加 cognitive load,新人上手需 1 周以上

**Risks**
- **Orchestrator 写成 framework 的诱惑** → 反对,**见 ADR-007**(memory `no-graph-framework-for-agent` 是原始数据来源,ADR-007 已结构化为 anti-pattern 决策 + 触发条件 + Q3 重审 governance),只写够当前 6 项要求的 ~800 行
- **SSH sandbox 平台 API 依赖** → 必须先跟平台沟通是否提供 SSM-style API,否则降级到 console proxy 或砍掉本项
- **HITL pause/resume 跨 server 多副本** → 必须 sticky session(memory `pr2_5_联调_2026_05_28` 已有,网关后已支持)

## Acceptance(整 ADR)

- [ ] `internal/orchestrator/` 目录建立,含 `step.go` / `saga.go` / `hitl.go` / `loop.go` 4 个核心文件
- [ ] `workflow.Step` 加 `Compensate` + `IdempotencyKey` 字段,所有现有 workflow 显式声明(read-only 设 nil)
- [ ] `internal/security/ssh_sandbox.go` 上线,白名单 12 条命令(GPU 诊断 4 + kernel/log 3 + 资源 3 + /proc 2,见 §决策 4 V1 表),policy 测试覆盖 50+ 拒绝 case
- [ ] B6 sandbox 落地后,逐条 cross-check `internal/skills/diagnose_*` SOP 引用的命令(`dmesg --since=<duration>` / `systemctl status <enum>` / `cat /proc/{cpuinfo,meminfo}` 等)跟 V1 whitelist 完全相容;任何 skill 命令越界 → 改为 user-runs(JupyterLab terminal)或走 issue 评审扩 whitelist,不得静默执行
- [ ] `internal/observability/step_trace.go` + MySQL `migrations/0004_alter_agent_traces.sql`(ALTER 加 step_id/saga_id/skill_id/task_tier 列,nullable)+ `migrations/0005_create_saga_pauses.sql`(新建 saga_pauses 表)— 跟 §决策 1 line 53 一致
- [ ] HTTP server 增 `ResumeSaga` Action + SSE `requires_action` event
- [ ] 1 个端到端 skill(推荐 `deploy_model` 或 `diagnose_gpu_not_detected`)跑通完整 6 要求,作为 B6 验收 demo

## References

- ADR-001 / ADR-002 / ADR-003: 前置依赖
- memory `no-graph-framework-for-agent`: 反对引入 Eino/LangGraph 的理由
- memory `l0-stop-grow-dictionary`: 白名单维护原则
- OpenAI Assistants Run lifecycle: requires_action / pause 模型来源
- k8sgpt Analyzer + Failure: ADR-005(诊断包重构,待写)消费本 ADR 的 step trace
