// Package agentprotocol contains small, transport-neutral protocol constants
// shared by the HTTP/WS server and adapter clients.
package agentprotocol

const (
	// FeatureFeishuConsoleHandoff asks the Agent to decide whether a
	// knowledge-only answer needs a separate, authenticated console diagnosis.
	// It grants no tools or identity; only the Feishu adapter interprets the
	// resulting marker and adds the configured console link.
	FeatureFeishuConsoleHandoff = "feishu_console_handoff_v1"

	// FeishuConsoleHandoffMarker is an adapter-private completion marker. It is
	// stripped before a reply reaches Feishu, so users never see protocol text.
	FeishuConsoleHandoffMarker = "[[COMPSHARE_CONSOLE_DIAGNOSIS_REQUIRED]]"
)
