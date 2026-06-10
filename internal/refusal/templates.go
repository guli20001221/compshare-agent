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
// See CategoryExistingDiskAttachUnsupported for the canonical add pattern.
package refusal

// Category names — must match observability.EngineHardBlockTrace.Category
// values. Downstream MySQL trace ingest + per-category eval dashboards
// pivot on these exact strings; treat as a stable contract.
const (
	CategoryMonitorHistory                = "monitor_history_unsupported"
	CategoryJailbreakAttempt              = "jailbreak_attempt"
	CategoryOffTopic                      = "off_topic_refused"
	CategoryExistingDiskAttachUnsupported = "existing_disk_attach_unsupported"
)

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
