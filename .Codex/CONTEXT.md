---
last_updated: 2026-05-07T00:00:00+08:00
---
# Project Context

## 当前状态
资源基础信息问答的核心 guard 已落到 engine 层，并用 Modelverse `deepseek-v4-flash` 做过真实账号回归。最新补丁覆盖到期/续费、显式历史区间、历史无监控数据回退、监控+扣费混合意图，并继续 hard-block 账号级余额/总账单/流水。

## 进行中
- [ ] 分支集成 — 当前 worktree 有多处无关脏文件；只把 resource-info guard/tool/eval 相关变更视为本任务范围，除非用户明确要求处理其他文件。

## 最近完成
- [x] 监控时间上下文注入 — `internal/engine/engine.go`, `internal/engine/engine_test.go`；当用户问“昨天下午两点/过去30分钟”等监控时间窗时，LLM 每轮请求前会收到确定的北京时间窗口和 `StartTime`/`EndTime`，包括只查实例列表后需要追问“哪台”的澄清阶段。
- [x] 监控最终回复日期 guard — 最后一轮 LLM 文案若输出与 engine 解析窗口不一致的 ISO 日期（如 `2025-06-30`），返回前改写为系统解析日期，并避免把历史窗口称为“当前实时监控”。
- [x] 监控无意义确认 guard — 历史监控意图已查到实例后，若模型停在“需要我继续查吗/确认一下”，engine 继续同一轮并强制 `GetCompShareInstanceMonitor`。
- [x] 监控最终回复时间范围 guard — 最终文案里的 `14:00 ~ 14:30` 等范围会改写为实际请求窗口，如 `13:45 ~ 14:15`。
- [x] 监控首轮实例发现 guard — 监控意图但未指定 `uhost-` 时，首轮强制 `DescribeCompShareInstance`，避免 ds v4 flash 把“4090监控”误判为 4090 规格/可售配置查询。
- [x] 监控历史单实例参数归一化 — 单实例 `GetCompShareInstanceMonitor` 调用前由 engine 重写 `StartTime`/`EndTime`，防止 LLM 自己算错日期或 Unix timestamp。
- [x] 监控历史批量拦截 — 多实例历史监控请求在 engine 层拦截，要求逐台单实例调用，避免后端返回最新 60 秒却被误称为历史数据。
- [x] 监控追问刷新 guard — 相邻监控指标追问强制重新调用 `GetCompShareInstanceMonitor`；明确“基于刚才/不用重新”时不触发。
- [x] 账号账单边界 — 余额、总账单、流水、账号月度汇总 hard-block；实例/机器/关机扣费类问题强制 `DiagnoseBilling`。
- [x] 监控 API 传输修复 — `GetCompShareInstanceMonitor` 使用 JSON body，保留 `UHostIds` 数组结构。
- [x] 到期/续费查询 guard — `internal/engine/engine.go`, `internal/prompt/builder.go`；到期/续费类问题首轮强制 `DescribeCompShareInstance`，并追加 `ResourceInfoSummary`，避免长 `UHostSet` 截断导致 `ExpireTime` / `AutoRenew` / `ChargeType` 缺失。
- [x] 历史无数据 deterministic fallback — 历史监控窗口没有有效采样点时，engine 直接返回“没有返回有效监控数据”，禁止用实时值或编造 CPU/GPU/显存数值。
- [x] 混合监控+扣费意图 — “哪台监控异常+哪台扣费多”强制执行 `DescribeCompShareInstance -> GetCompShareInstanceMonitor -> DiagnoseBilling`。
- [x] 空 tool arguments 兼容 — 模型发空 `arguments` 时按 `{}` 解析，避免 `unexpected end of JSON input` 噪声。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| 相对时间 | system prompt 时间注入 + engine 确定性时间窗注入/参数重写 | prompt-only 不稳定，ds v4 flash 会把“昨天”算错 | 2026-04-30 |
| 监控首轮路由 | 未指定实例的监控意图强制先查实例列表 | 避免 `4090监控` 走 GPU 规格/库存工具 | 2026-04-30 |
| 历史监控 | 多实例历史批量禁止，逐台单实例查 | 上游多实例监控只返回最近 60 秒快照 | 2026-04-30 |
| 账号财务 | 账号级余额/总账单/流水/月度汇总 hard-block | 产品范围不支持账号级财务中心数据 | 2026-04-30 |
| 实例费用 | 实例口径费用/扣费/关机收费走 `DiagnoseBilling` | 实例成本可由现有实例与价格接口解释 | 2026-04-30 |
| 资源基础信息 | 到期/续费问题走实例列表，并追加 compact summary | 让 LLM 在长列表截断后仍能看到所有实例的 `ExpireTime` / `AutoRenew` | 2026-04-30 |
| 混合意图 | engine 顺序强制监控与计费工具 | prompt-only 下模型可能只查其中一类 | 2026-04-30 |
| Stage 2 方向 | 不继续在 `Chat()` 内堆 guard，改为结构化意图/显式状态机 | 当前 `force*` 分支已接近隐式 FSM，操作类再扩展会难调试 | 2026-05-01 |

## 验证记录
- `go test ./internal/engine -run 'TestResourceInfoGuard|TestMonitorTimeArgNormalizer|TestMonitorHistoricalNoData|TestMixedMonitorBillingIntent|TestMonitorIntentGuard|TestMonitorHistoryBatchGuard' -count=1` PASS
- `go test ./internal/engine -run 'TestExecuteTool_TreatsEmptyArgumentsAsEmptyObject|TestResourceInfoGuard|TestMonitorTimeArgNormalizer|TestMonitorHistoricalNoData|TestMixedMonitorBillingIntent|TestAccountBillingUnsupported' -count=1` PASS
- `go test ./internal/prompt -run 'TestBuildSystem_ContainsExpiryRenewalGuidance|TestBuildSystem_ContainsMonitorWindowGuidance' -count=1` PASS
- `go test ./... -count=1` PASS
- 真实 ds v4 flash 报告：`eval/shadow_qa/2026-04-30-resource-info-edge-cases-ds-v4-flash/edge_cases_report.md`

## 已知问题
- [medium] live monitor integration 默认仍是 opt-in；普通 CI 覆盖请求形状、guard 和签名逻辑，不保证线上接口当天可达。
- [low] 账号财务 hard-block 仍基于关键词，极少数非财务语义可能被保守拦截。
- [medium] `Chat()` 内 tool_choice guard 已明显膨胀，`forceMonitorAfterDiscovery` / `forceBillingAfterMonitor` 属于跨 ReAct 轮隐式状态；stage 2 引入操作类能力前应优先重构成结构化意图分类层或显式 FSM。
- [medium] `guardMonitorTemporalFinalReply` 目前会正则改写 LLM 最终文本中的日期/时间和“当前实时监控”等措辞；这是 stage 1 的兜底，不适合作为长期主路径。后续应把 `target_date` / `target_time_range` 放进 system/tool result，让模型一次写对，并把输出层 guard 改为校验/重试或窄范围兜底。
- [high] Modelverse 上部分 thinking-mode 模型（已观察到 Doubao-Seed-2-0-Lite-260215）不支持 object 形式 `tool_choice`，会在命中 engine 强制工具 guard 时返回 400：`tool_choice parameter does not support being set to required or object in thinking mode`。短期测试可换非 thinking 模型；长期应给 LLM/model 加能力位，或在 stage 2 结构化意图层中避免依赖 object `tool_choice`。
- [medium] 当前 `go-openai@v1.36.1` 的 `ChatCompletionRequest` 没有 arbitrary `extra_body` 字段；若要给 Modelverse/Doubao 透传 `thinking disabled`，需要升级/扩展 LLM 客户端或绕过 SDK 自己组请求，不能只改 YAML。
- [medium] worktree 有无关脏文件和历史 eval 产物；不要擅自 revert。
