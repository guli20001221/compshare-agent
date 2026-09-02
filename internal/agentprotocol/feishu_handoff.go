// Package agentprotocol contains small, transport-neutral protocol constants
// shared by the HTTP/WS server and adapter clients.
package agentprotocol

const (
	// FeatureFeishuPublicPlatformReadOnly asks the Agent to expose the
	// fail-closed public-platform query window used by the Feishu adapter. It
	// never grants account, instance, diagnostic, or mutating capabilities.
	FeatureFeishuPublicPlatformReadOnly = "feishu_public_platform_readonly_v1"

	// FeatureFeishuConsoleHandoff asks the Agent to decide whether a
	// public Feishu answer needs a separate, authenticated console diagnosis.
	// It grants no tools or identity; only the Feishu adapter interprets the
	// resulting marker and adds the configured console link.
	FeatureFeishuConsoleHandoff = "feishu_console_handoff_v1"

	// FeishuConsoleHandoffMarker is an adapter-private completion marker. The
	// angle-bracket envelope is deliberately disjoint from the Agent's
	// [[chunk_id]] citation syntax: handoff control must survive knowledge-answer
	// citation cleanup before the Feishu adapter consumes it.
	FeishuConsoleHandoffMarker = "<<COMPSHARE_CONSOLE_DIAGNOSIS_REQUIRED>>"

	// FeishuCustomerSupportMarker is an adapter-private result emitted by the
	// customer-support handoff tool. The model never authors or sees it. The
	// Feishu adapter renders it as a concise support recommendation and never
	// shows it verbatim. It uses the same reserved control envelope as the
	// console marker, outside the citation namespace.
	FeishuCustomerSupportMarker = "<<COMPSHARE_CUSTOMER_SUPPORT_REQUIRED>>"

	// CustomerSupportHistoryCompletion is the channel-neutral completion kept in
	// model history when the user-facing channel rendered a support handoff.
	// Adapter markers and Web QR markup are display projections, not model facts.
	CustomerSupportHistoryCompletion = "已提供客服联系入口，未确认接通或受理。"
)
