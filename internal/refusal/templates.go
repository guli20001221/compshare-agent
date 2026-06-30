// Package refusal centralizes user-facing refusal templates that the
// agent returns when a hard-block / route boundary fires. Reply
// text MUST be byte-stable so:
//
//  1. Eval scripts can byte-compare against a golden artifact.
//  2. A/B testing variants can be introduced without scattering string
//     edits across the engine.
//  3. Multiple routing decision points that share a refusal (e.g. the
//     monitor-history reply reused at 5 sites in engine.Chat /
//     tryPlannerDispatch / tryRouteDispatch) read from a single source.
//
// New refusal categories add to BOTH the Category* and the reply-text
// constants here, then are wired into internal/router or other callers.
// See CategoryMonitorHistory for the canonical add pattern.
package refusal

// Category names — must match observability.EngineHardBlockTrace.Category
// values. Downstream MySQL trace ingest + per-category eval dashboards
// pivot on these exact strings; treat as a stable contract.
const (
	CategoryMonitorHistory   = "monitor_history_unsupported"
	CategoryAccountBilling   = "account_billing_unsupported"
	CategoryJailbreakAttempt = "jailbreak_attempt"
	CategoryOffTopic         = "off_topic_refused"
	CategoryHumanAgent       = "human_agent_transfer"
)

// MonitorHistoryUnsupported is returned when the user asks for a historical
// monitor shape the agent cannot safely execute yet (missing target, missing
// concrete time window, multiple instances, or a window beyond the supported
// limit). Single-instance historical monitor with an explicit <=24h window is
// handled by the monitor workflow.
const MonitorHistoryUnsupported = "历史监控目前一次只支持查询一台实例，且需要明确 24 小时内的时间范围。请补充实例和时间段，例如“查询 uhost-xxx 昨天 8 点到 10 点的 CPU 监控”。"

// AccountBillingUnsupported is returned when the planner classifies the user
// request as account-level finance data that the agent cannot query safely.
// Instance-scoped pricing, billing diagnosis, and refund estimation are still
// supported by dedicated read-only routes/tools.
const AccountBillingUnsupported = "当前不支持直接查询账号余额、账号总账单、消费流水、发票状态、余额提现等账号级财务数据。你可以在控制台费用中心查看这些信息，或联系人工客服确认。\n\n我可以继续帮你查询实例价格、实例费用诊断、资源退费估算等已支持内容。"

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

// OffTopic is returned when the input matches a topic the agent is not
// scoped to handle — personal medical advice, political opinion, stock
// recommendations, severe-emotional-distress (suicide ideation), etc.
// The wording redirects rather than just refuses, naming professional
// channels (medical/legal/financial/mental-health) so the user knows
// where to go. CompShare-platform scope (GPU/billing/monitor/image/
// price) is reaffirmed at the end as the inviting redirect.
//
// Suicide-ideation case is deliberately bundled — a separate hotline-
// specific reply was considered but rejected: a stale or jurisdiction-
// mismatched phone number in code is worse than the generic
// "professional help" redirect. If/when we add a maintainer-curated
// hotline table, that's a follow-up with proper sourcing + review.
const OffTopic = "我是 CompShare 算力平台助手，这类问题超出了我的回答范围。建议您咨询相应领域的专业人士（医生 / 律师 / 财务顾问 / 心理咨询师 等）。如果您有算力平台相关问题（GPU 规格、计费、监控、镜像、价格等），我很乐意继续帮您。"

// HumanAgentTransfer is returned when the user explicitly asks to reach a
// human agent (转人工 / 人工客服 / 联系人工 / 找人工 / 叫人工). The reply
// carries the official customer-service QR code as a markdown image so a
// markdown-capable frontend (the HTTP/WS chat client) renders it inline; the
// CLI prints the image link verbatim. The QR URL is byte-pinned here as the
// single source — refresh the image by editing this constant only.
const HumanAgentTransfer = "好的，已为您转接人工客服。请扫描下方二维码添加客服微信，会有专人为您服务。\n\n![客服二维码](https://ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png)"
