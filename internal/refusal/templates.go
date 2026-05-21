// Package refusal centralizes user-facing refusal templates that the
// agent returns when a hard-block / capability boundary fires. Reply
// text MUST be byte-stable so:
//
//  1. Eval scripts can byte-compare against a golden artifact.
//  2. A/B testing variants can be introduced without scattering string
//     edits across the engine.
//  3. Multiple routing decision points that share a refusal (e.g. the
//     monitor-history reply reused at 5 sites in engine.Chat /
//     tryPlannerDispatch / tryPhase1Cutover) read from a single source.
//
// New refusal categories add to BOTH the Category* and the reply-text
// constants here, then are wired into internal/router or other callers.
// See PR #139 (resource_shortage_226604) for the canonical add pattern.
package refusal

// Category names — must match observability.EngineHardBlockTrace.Category
// values. Downstream MySQL trace ingest + per-category eval dashboards
// pivot on these exact strings; treat as a stable contract.
const (
	CategoryAccountBilling   = "account_billing_unsupported"
	CategoryMonitorHistory   = "monitor_history_unsupported"
	CategoryResourceShortage = "resource_shortage_226604"
	CategoryJailbreakAttempt = "jailbreak_attempt"
)

// AccountBillingUnsupported is returned for account-level financial
// questions (余额 / 总账单 / 消费流水 / 退款 / 发票 etc.). These live in
// the user's billing center, not in any per-instance API.
const AccountBillingUnsupported = "这类账号级财务信息当前不支持由助手查询。请到控制台的财务中心查看：账号总览看余额，账单管理看月度账单，消费记录看扣费流水，发票管理看开票和寄送状态，退款或欠费信息以订单/财务中心页面为准。"

// MonitorHistoryUnsupported is returned when the user asks for monitor
// data over a past time window (昨天/上周/最近 N 天 etc.). The runtime
// monitor API only exposes a sliding real-time window. Reused at five
// routing decision points in the engine.
const MonitorHistoryUnsupported = "当前暂不支持指定历史时间段的监控查询。我可以先帮你查看实时监控；如需历史趋势，请在控制台监控页选择对应日期和时间范围查看。"

// JailbreakAttempt is returned when the input matches a known
// instruction-override / system-prompt-extraction pattern (e.g. "ignore
// previous instructions", "扮演 X", "print your system prompt"). The
// reply is intentionally on-topic for the platform — declining the
// jailbreak but inviting the user back to legitimate questions — so a
// real user who phrased something innocuously is not stonewalled.
//
// Wording avoids confirming the system prompt exists / mentioning what
// the override target was; both would leak structure useful to a
// determined attacker.
const JailbreakAttempt = "我注意到您的消息看起来像在请求我绕过自身的安全限制或修改我的指令。我无法忽略或更改我的核心规则——这些限制是为了让回答可靠且符合算力平台的使用规范。如果您有正常的平台相关问题（资源、计费、监控、镜像、GPU 规格、价格等），我很乐意继续帮您。"

// ResourceShortage226604 is returned when the user pastes upstream
// uhost-compshare-api error code 226604 ("当前资源不足，请稍后再试") or
// phrases the question as 资源不足. Wording softened from earlier draft
// per PR #139 review #4 — avoid hard guarantees about 独占机器 /
// 不会被退出 because 包日/包月 SKUs still draw from the same pool at
// create-time.
const ResourceShortage226604 = "226604（当前资源不足）是平台 GPU 资源池的实时状态，并不是您账号或操作的问题——您选择的机型当前已被其他用户占满。资源会随着其他用户关机或退订陆续流转出来，建议您稍等片刻后重试同一机型，或在控制台库存页换一个可用区或相近规格（例如 4090 紧张时可以试试 A100）。如果业务需要长期稳定使用资源，也可以考虑包日或包月付费，相比按量计费在资源稳定性上更有保障。感谢您的耐心等待。"
