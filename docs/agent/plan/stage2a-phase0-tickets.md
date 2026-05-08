---
status: ready for execution
parent: stage2-intent-planner.md
phase: Phase 0 (foundation infrastructure only, no business intent migration)
created: 2026-05-01
estimated_duration: 2 weeks
---

# Stage 2A Phase 0 工单清单

## 目标

Phase 0 **只搭基础设施 + IntentPlan 影子模式**，**绝不**触碰业务意图切流。
- 切流是 Phase 1+ 的工作（按意图逐个切，从 monitor 开始）
- Phase 0 完成时主流程仍走当前 ReAct + 老 guard，但 SafeToolExecutor / SecretBoundary / RateLimiter 已就位、planner 已并行运行并出 dashboard

## 退出标准（Phase 0 done = 全部满足）

- 全部测试 PASS（既有 + 新增）
- IntentPlan 在影子模式下连续运行 5 个工作日，无 panic / 无内存泄漏
- planner-vs-old-guard 一致率 dashboard 可看，覆盖现有所有 force-tool guard 场景
- SafeToolExecutor 接管所有工具调用，老路径（engine 直接调 inner executor）grep 全无
- 任何一个 secret（PublicKey / PrivateKey / API Key）在 LLM prompt / trace / error message 里 grep 全无
- RateLimiter 触发能正确返回友好提示并写 audit
- 验证集：附录 A `phase0_smoke.json`（10 条手动样例）全 PASS

## 工单总览

9 个工单（T-004 / T-007 各拆两段，让 planner 风险尽早上线），按依赖关系分 3 批：

```text
Batch A（Week 1 上半）— 互不依赖，可并行
  T-001   SafeToolExecutor + ToolExecutionPolicy 抽离
  T-002   SecretBoundary + YAML 占位符化
  T-003   CapabilityRegistry
  T-004a  EntityRegistry parser/resolver（无外部依赖）

Batch B（Week 1 下半 ~ Week 2 上半）— 依赖 Batch A
  T-004b  EntityRegistry 接入 SafeToolExecutor       依赖 T-001 + T-004a
  T-005   RateLimiter / QuotaManager                 依赖 T-002
  T-006   Trace / Audit skeleton                     依赖 T-001 + T-002
  T-007a  IntentPlan types/Validator/Offline planner 依赖 T-003 + T-004a   ← 关键：planner 风险尽早暴露

Batch C（Week 2 下半）— 依赖 Batch A+B
  T-007b  Shadow planner 接入线上 trace + dashboard  依赖 T-001 + T-004b + T-005 + T-006 + T-007a
```

## 工单详情

---

### T-001 SafeToolExecutor + ToolExecutionPolicy 抽离

**依赖**：无
**预估**：3-4 天
**所属 Batch**：A

**Scope**：
- 新建 `internal/tools/safe_executor.go` 实现 SafeToolExecutor 类型，按 §3.5.2 契约
- 新建 `internal/tools/policies.go` 集中注册所有 Action 的 ToolExecutionPolicy
- 把现有 `engine.executeTool` 里的 6 项语义全部迁入：
  - `security.Check`（L0/L1/L2 等级判定）
  - L1 user confirm（保留现有 ConfirmFunc 接口）
  - `filterAllowedParams`（参数白名单）
  - sanitizer（结果脱敏）
  - monitor history guard（继承 `trackMonitorResult` / `guardMonitorTemporalFinalReply`）
  - dual-channel display（raw token / SSH command 等敏感数据走独立通道）
- 新增按 ActionClass 的 retry 策略
- engine 改为通过 SafeToolExecutor 调用工具

**Acceptance（结构性，非 grep 拍脑门）**：
- [ ] 全部既有单测 PASS（不允许改测试，只允许改源码达成同行为）
- [ ] `Engine` struct 不再持有任何裸 `tools.ToolExecutor` 字段，只持有 `SafeToolExecutor` interface
- [ ] 所有工具调用入口（`engine.executeTool` / `executeWorkflow` / `executeDiagnosis` / freshnessTracker / 任何 handler）经 SafeToolExecutor，**通过 spy inner executor 在集成测里逐一验证调用路径**：测试用 `spyInnerExecutor` 包裹真 inner，断言每条 path 上 spy 都被命中且经过了 Policy 检查
- [ ] policies.go 注册了所有现有 Action 的 Policy（DescribeCompShareInstance / GetCompShareInstanceMonitor / GetCompShareInstancePrice / Stop / Start / Reboot / Reset / Resize / Rename / SetStopScheduler / CancelStopScheduler / Diagnose* 全集）
- [ ] 新增单测：retry 行为（read 类网络错误重试 1 次、mutating 不重试、destructive 拒）
- [ ] 新增单测：Policy 缺失时的安全降级（拒绝调用 + audit 标 `policy_missing`，不静默放行）

**风险**：
- monitor history guard 现有实现散落在 4 处，迁移可能漏；用 phase0_smoke.json 里的历史窗口样例做 regression 兜底

---

### T-002 SecretBoundary + YAML 占位符化

**依赖**：无
**预估**：2 天
**所属 Batch**：A

**Scope**：
- 新建 `internal/security/secret_boundary.go`，提供：
  - `RedactForLLM(any) any`：清掉 PublicKey / PrivateKey / API Key / Password / SSH command / JupyterLab token 等
  - `RedactForTrace(any) any`：上面 + 余额 / 计费金额（hash 化）/ IP（部分掩码）
- YAML config schema 改为只接受占位符（如 `${COMPSHARE_PUBLIC_KEY}`），实际值仅从环境变量读
- 启动时 config loader 解析占位符，未设置环境变量则启动失败 + 友好 error message
- pre-commit hook 添加 secret 扫描（git-secrets 或 truffleHog）
- 现有 `eval/shadow_qa/*/agent.yaml` 全部迁到占位符 + 提供 `.env.example`
- 显式排除：runtime wire-up 不在 T-002 scope，由 T-001 SafeToolExecutor / T-006 trace writer 完成

**Acceptance**：
- [ ] `grep -rE "[A-Za-z0-9]{32,}" eval/shadow_qa/*.yaml` 0 命中（明文 key 全清）
- [ ] 新增单测：RedactForLLM 对所有定义的敏感字段全部正确脱敏
- [ ] 新增单测：config loader 占位符解析 + 缺失环境变量时的 fail-fast
- [ ] pre-commit hook 在测试仓库里能挡掉故意 commit 的明文 key
- [ ] 真实账号 E2E 跑通（证明环境变量注入正确生效）

**风险**：
- 现有运行依赖明文 YAML，迁移期间需要在文档里写清如何设环境变量

---

### T-003 CapabilityRegistry

**依赖**：无
**预估**：1.5 天
**所属 Batch**：A

**Scope**：
- 新建 `internal/llm/capability.go`
- 提供 `LookupCapability(baseURL, model string) Capability` 接口
- Capability 包含：`SupportsJSONSchema bool` / `SupportsJSONObject bool` / `IsThinkingMode bool` / `RequiresExtraBody map[string]any`
- 内置初始能力表（按调研：OpenAI gpt-4o+ json_schema、Anthropic 部分版本、Modelverse 各模型默认 json_object、Doubao via Volcengine 非 thinking、ds-v4-flash json_object）
- 表条目通过 YAML 配置文件可扩展（不需重新编译）

**Acceptance**：
- [ ] 表至少包含：`api.modelverse.cn/v1` × {deepseek-v4-flash, qwen3.6-plus, glm-5-turbo, doubao-seed-2-0-lite-260215}、`ark.cn-beijing.volces.com` × doubao-lite、OpenAI gpt-4o
- [ ] 单测覆盖每条目的 lookup
- [ ] 配置文件路径通过 env 可覆盖
- [ ] 表条目可热更新（开发友好）

---

### T-004a EntityRegistry parser / resolver（无外部执行依赖）

**依赖**：无（用接口注入 executor，可 mock 或暂用现 inner executor）
**预估**：1.5 天
**所属 Batch**：A（与 T-001/T-002/T-003 并行，加速 T-007a shadow planner 上线）

**Scope**：
- 新建 `internal/entity/registry.go`、`internal/entity/snapshot.go`、`internal/entity/resolve.go`
- 定义 `InstanceSnapshot` / `EntityRegistry` / `ResolveResult` 类型
- 实现纯逻辑层：`ResolveByID` / `ResolveByName`（含模糊匹配与 AMBIGUOUS 判定）/ `Filter`
- executor 通过 `interface{ Execute(ctx, action, args) (...) }` 注入，T-004a 阶段可用现 `tools.ToolExecutor` 直接喂或 mock
- sync_event / age 字段维护

**Acceptance**：
- [ ] 单测：ResolveByID HIT / NOT_FOUND_IN_ACCOUNT / RECENTLY_RELEASED_GUESS 三种结果（用 fixture data，不依赖网络）
- [ ] 单测：ResolveByName 多匹配返回 AMBIGUOUS、唯一匹配返回 HIT、模糊匹配的排序稳定
- [ ] 单测：Filter(state=Running) / Filter(gpu_type=4090) 准确
- [ ] 真实账号集成测（gated by env）：Init → 拉到当前账号实例 → ResolveByID 全部 HIT

---

### T-004b EntityRegistry 接入 SafeToolExecutor

**依赖**：T-001（SafeToolExecutor 就位）+ T-004a（registry 逻辑层就位）
**预估**：1.5 天
**所属 Batch**：B

**Scope**：
- 把 T-004a 的 executor 注入点切到 SafeToolExecutor，享受 retry / sanitizer / Policy 检查
- registry 整合进 engine init 流程但**不强制 enforce**（handler 仍可绕过；Phase 1 切流时再强制）
- **Phase 0 不实装的**（留 stub）：write-tool invalidate、async refresh、refresh_request 事件
- 加一组覆盖 SafeToolExecutor 路径的集成测

**Acceptance**：
- [ ] 集成测：registry sync 走 SafeToolExecutor，spy 验证 Policy / sanitizer 都生效
- [ ] 真实账号集成测：ResolveByID(已释放 ID) 返回 NOT_FOUND_IN_ACCOUNT

**风险**：
- 实例数 > 100 的账号需要分页，stage 2A Phase 0 先支持单页 100 上限，超过的告警；Phase 1 切流前补分页

---

### T-005 RateLimiter / QuotaManager

**依赖**：T-002（SecretBoundary，因 RateLimiter key = API Key 哈希需要 SecretBoundary 提供 hash 接口）
**预估**：2 天
**所属 Batch**：B

**Scope**：
- 新建 `internal/governance/ratelimit.go`
- 进程内 token bucket 实现（不引入 Redis；多实例支持留到 stage 3）
- 默认配额按 §3.9.4：
  - LLM QPS 5 / 日 5000
  - mutating QPS 1 / 日 50
- 配置项可覆盖：`internal/config/governance.yaml`
- 触发时返回 `ErrRateLimited`，engine 上层翻译为友好提示 + audit 标 `rate_limited`

**Acceptance**：
- [ ] 单测：QPS 限流 + 日额限流（用 fake clock）
- [ ] 单测：不同 API Key 独立计数
- [ ] 单测：触发后 audit 字段正确写入
- [ ] 集成：手动跑 6 个 LLM 调用快速触发 QPS=5，第 6 个收到友好提示

---

### T-006 Trace / Audit skeleton

**依赖**：T-001 (SafeToolExecutor 提供 tool latency / args hash)、T-002（SecretBoundary 提供 RedactForTrace）
**预估**：2 天
**所属 Batch**：B

**Scope**：
- 新建 `internal/observability/trace.go`
- 实现 §3.10 trace schema 的所有字段
- 输出格式：JSON Lines，写文件（`logs/agent-trace-YYYY-MM-DD.jsonl`），按日轮转
- 30 天自动清理（cron / 进程内 ticker 二选一，先做后者）
- Trace ACL：文件权限 0640，仅 ops user 可读（Linux/Mac；Windows 现有部署忽略此项）

**Acceptance**：
- [ ] 每轮 Chat() 后输出一条完整 trace 行
- [ ] 单测：trace 字段完整 + RedactForTrace 已应用（grep secret 0 命中）
- [ ] 30 天清理逻辑单测（fake clock）
- [ ] 真实账号跑一次 10-step E2E，确认 10 行 trace 写入

---

### T-007a IntentPlan types / Validator / Offline planner（早期上线）

**依赖**：T-003（CapabilityRegistry）+ T-004a（registry 类型用于 slot validator）
**预估**：3 天
**所属 Batch**：B（与 T-005 / T-006 并行，让 planner 风险尽早暴露）

**Scope**：
- 新建 `internal/intent/types.go`：定义 IntentPlan struct（schema_version / intent enum / scope / slots / required_tools / retrieval / hard_block_hint / confidence / reasoning）
- 新建 `internal/intent/validator.go`：JSON schema 验证 + intent enum 验证 + slot 类型验证 + EntityValidator substring 校验集成（用 T-004a 的 resolve API）
- 新建 `internal/intent/planner.go`：调用 LLM、按 CapabilityRegistry（T-003）选择 response_format、parse + retry
- **离线 fixtures eval**：`eval/intent/fixtures.jsonl` 含 ≥ 50 条 (user_msg, registry_snapshot, expected_plan) 三元组，覆盖 monitor_query / billing_instance / billing_account_unsupported 三类 + edge cases；offline runner 跑这套 fixtures 出准确率报告
- intent enum 全部 14 个意图都在 schema 里声明，Phase 0 只对上述 3 类做 prompt + few-shot；其他类先返回 unknown
- **不接 trace / 不接主流程**——T-007a 只验"planner 自身能产出合法 plan"

**Acceptance**：
- [ ] schema 单测：合法 plan 通过、非法 plan 在 5 个方向（schema_version / intent enum / slot 类型 / provenance / required_tools 非法）都被 reject
- [ ] EntityValidator 单测：`uhost_id_user_input.source_span` 在用户原文 substring 校验通过 / 失败两种情况
- [ ] Planner 单测：mock LLM 返回不同质量 JSON（合法 / 非法 / 部分缺失），都正确处理
- [ ] CapabilityRegistry 集成：thinking-mode 模型自动选 json_object 而非 json_schema（用 stub LLM 验证选 mode 正确）
- [ ] 离线 fixtures eval：合法 plan 输出率 ≥ 95%、目标 3 类意图分类准确率 ≥ 90%

**风险**：
- 离线 fixtures 难以覆盖真实流量分布；T-007b 接入线上 shadow 后会进一步暴露问题，这是计划的

---

### T-007b Shadow planner 接入线上 trace + dashboard

**依赖**：T-007a + T-001（SafeToolExecutor，让 planner 调用走统一 LLM client wrap）+ T-005（RateLimiter，planner 调用也算 LLM 配额）+ T-006（Trace skeleton）+ T-004b（registry sync 已经在 SafeToolExecutor 路径上，给 planner 喂 snapshot）
**预估**：2 天
**所属 Batch**：C

**Scope**：
- 新建 `internal/intent/shadow.go`：影子模式开关 `USE_INTENT_PLANNER=shadow`，planner 跑起来但**不影响主流程决策**，仅写 trace
- 给 trace skeleton 增加 `planner` block 字段（model / latency / input_tokens / output_tokens / schema_valid / intent / confidence / hard_block_hint）
- 新增 dashboard 脚本 `scripts/planner_vs_guard_diff.py`：从 trace 文件统计 planner 决策 vs 现有 guard 决策的一致率，按意图分类输出 markdown 报告

**Acceptance**：
- [ ] 影子模式跑 phase0_smoke.json 10 个样本，全部输出合法 plan 且与现有 guard 决策一致率 ≥ 80%
- [ ] dashboard 脚本能从 trace 文件生成 markdown 报告
- [ ] 影子模式打开 / 关闭通过环境变量切换，关闭时 planner 不发起任何 LLM 调用（验证开关有效）
- [ ] Monitor freshness acceptance 按 `docs/agent/plan/stage2a-t007b-monitor-acceptance.md` 执行：PR #12 的 3 个 monitor follow-up case 必须进入 shadow trace / Phase 1 handler promotion gate，mixed monitor+billing/diagnosis/operation scope 必须有可测边界。

**风险**：
- 线上一致率可能初期低于 80%（特别是 monitor 多步链路），需准备好 prompt + few-shot 的迭代窗口

---

## 跨工单约束

- **Stage 2A primary baseline：`deepseek-v4-flash`**。Phase 0 所有真实账号 E2E 默认以 Modelverse `deepseek-v4-flash` 为主基准；其他模型验证只作为兼容性参考，不阻塞主线。
- 若 `deepseek-v4-flash` 的真实生产 request shape 与 CapabilityRegistry 矩阵或历史 probe 结论冲突，以真实生产 request shape probe 为准，并在代码或 artifact 中记录 traceability comment（日期 / base_url / model / request shape / response status / response body 摘要）。
- 涉及 force-tool / `response_format` / thinking-mode 的工单，必须包含 `deepseek-v4-flash` 验证；无法验证时必须写明原因，且仅以下三类成立：① 测试 key 无该模型权限；② 工单测试的是 `deepseek-v4-flash` 明确不支持的 model-specific 特性；③ 预算或 rate-limit 已超出。
- Capability probe 必须使用 engine 实际生产 request shape，不能只用 isolated minimal request。至少包括完整 tools list、真实 system prompt、以及与目标场景一致的最近多轮 chat history 形状；probe artifact 必须保留足够信息用于复现。历史上已出现过 minimal probe 通过但 production CLI 失败的情况。
- Phase 0 默认不移除现有 force-tool guard；若 `deepseek-v4-flash` 的真实生产 request shape probe 证明某 guard 会稳定触发 400，且 auto routing 不劣于 forced routing，允许通过独立 probe artifact + traceability comment 移除或 capability-gate。此例外必须在 PR body 引用本条约束、probe artifact 路径和决策理由。永久 hard-block（如 `isAccountBillingUnsupported`）不在此例外内，Phase 1 切意图时也不得误删。
- **绝不**让 IntentPlan 在影子模式下决定主流程行为。即便 planner 给出更"对"的决策，也只能写 trace。
- 新模块所有外部依赖通过 interface 注入，便于单测 mock。

## 验证集：phase0_smoke.json

新增到 `eval/shadow_qa/2026-05-01-phase0-smoke/cases.json`，覆盖：

1. 列实例 → 走 DescribeCompShareInstance（验 SafeToolExecutor 接管）
2. 看监控 → 走 Monitor（验 monitor history guard 迁移成功）
3. 历史时间窗监控 → 验 retry 行为符合 baseline：正常成功路径不重试；fault-injection 模拟网络错误时至多重试 1 次；4xx 不重试
4. 关机操作（不实际执行，验 confirmFunc 链路）→ 验 L1 confirm 迁移
5. 释放操作 → 验 L2 拒绝
6. 账号余额 → 验 hard-block 仍生效
7. 账号下哪台实例消费最高 → 验 hijack 不被误拦
8. 包含明文 SSH 命令的回复 → 验 sanitizer 脱敏 + dual-channel display
9. 触发 RateLimiter 的快速连发 → 验限流提示
10. planner 跑起来后输出 plan + 与 guard 一致率（不算 PASS 标准，仅记录）

## 时间线

| Week | Day | 工单 | 负责人 |
|---|---|---|---|
| W1 | 1-2 | T-001 SafeToolExecutor 起步 | TBD |
| W1 | 1-2 | T-002 SecretBoundary 并行 | TBD |
| W1 | 1-2 | T-003 CapabilityRegistry 并行 | TBD |
| W1 | 1-2 | T-004a EntityRegistry parser 并行 | TBD |
| W1 | 3-4 | T-001 收尾 | TBD |
| W1 | 3-4 | T-005 RateLimiter | TBD |
| W1 | 3-4 | T-007a IntentPlan types/Validator/Offline 起步（最重要：planner 风险尽早暴露） | TBD |
| W1 | 5 | T-006 Trace skeleton + T-004b 接入 SafeToolExecutor | TBD |
| W2 | 1-2 | T-007a 收尾 + 离线 fixtures eval 出报告 | TBD |
| W2 | 3-4 | T-007b Shadow planner 接 trace + dashboard | TBD |
| W2 | 5 | phase0_smoke.json 全跑 + 退出标准核对 | TBD |

## Phase 0 完成后下一步

进入 Phase 1：将 monitor 类意图（`monitor_query` + `monitor_history`）从老 guard 切换到 planner-driven。切换前必须满足 §10.1 acceptance criteria（路由准确率 ≥ 95% / escaped_hallucinated == 0 等）。

## 附录 A：phase0_smoke.json 模板

完整 JSON 在 T-007 工单交付时落盘。结构遵循现有 `real_cli_golden_runner.py` 期望格式，每个 case 含：
- input / steps
- expect_tool_calls / reject_tool_calls
- reply_contains_any / reply_not_contains
- 新增字段：`expect_safe_executor_path`（验工具调用走 SafeToolExecutor）
- 新增字段：`expect_trace_fields`（验 trace 输出包含指定字段）
