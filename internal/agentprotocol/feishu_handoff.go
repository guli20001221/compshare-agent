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

	// FeishuConsoleHandoffMarker is an adapter-private completion marker. It is
	// stripped before a reply reaches Feishu, so users never see protocol text.
	FeishuConsoleHandoffMarker = "[[COMPSHARE_CONSOLE_DIAGNOSIS_REQUIRED]]"

	// FeishuCustomerSupportMarker is an adapter-private completion marker for
	// account, verification, page-loading, and platform-service issues. It is
	// rendered as a concise customer-support recommendation by the Feishu
	// adapter, never shown verbatim to a user.
	FeishuCustomerSupportMarker = "[[COMPSHARE_CUSTOMER_SUPPORT_REQUIRED]]"
)
