package feishu

import (
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/require"
)

func TestValidateConfigConsoleHandoffRequiresHTTPSURL(t *testing.T) {
	cfg := config.FeishuConfig{
		AppID: "cli_test", AppSecret: "secret", AgentWSURL: "ws://127.0.0.1:7429/",
		CompanyID: 1, OrganizationID: 2, AllowedChatIDs: []string{"oc_test"},
		EnableConsoleHandoff: true,
	}
	require.ErrorContains(t, ValidateConfig(cfg), "console_assistant_url")
	cfg.ConsoleAssistantURL = "http://console.example.test/assistant"
	require.ErrorContains(t, ValidateConfig(cfg), "absolute HTTPS")
	cfg.ConsoleAssistantURL = "https://console.example.test/#/assistant"
	require.NoError(t, ValidateConfig(cfg))
	cfg.ClientDownloadURL = "http://www.example.test/client"
	require.ErrorContains(t, ValidateConfig(cfg), "client_download_url")
	cfg.ClientDownloadURL = "https://www.example.test/client"
	require.NoError(t, ValidateConfig(cfg))
}

func TestConsoleHandoffMarkerIsNeverRenderedAndIncludesClientWhenConfigured(t *testing.T) {
	answer, requested := consumeConsoleHandoffMarker("先给出排查步骤。\n" + agentprotocol.FeishuConsoleHandoffMarker)
	require.True(t, requested)
	require.Equal(t, "先给出排查步骤。", answer)

	reply := appendConsoleHandoff(answer, "https://console.example.test/#/assistant", "https://www.example.test/client")
	require.NotContains(t, reply, agentprotocol.FeishuConsoleHandoffMarker)
	require.Contains(t, reply, "优云智算控制台智能助手")
	require.Contains(t, reply, "https://console.example.test/#/assistant")
	require.Contains(t, reply, "优云智算客户端")
	require.Contains(t, reply, "https://www.example.test/client")
	require.Contains(t, reply, "CLI 和 SSH")
}

func TestConsoleHandoffKeepsWebOnlyBackwardCompatibility(t *testing.T) {
	reply := appendConsoleHandoff("", "https://console.example.test/#/assistant", "")
	require.Contains(t, reply, "优云智算控制台智能助手")
	require.NotContains(t, reply, "优云智算客户端")
}

func TestKnowledgeBoundaryViolationRequestsConsoleHandoff(t *testing.T) {
	require.True(t, isKnowledgeBoundaryViolation(&AgentError{Code: "KnowledgeBoundaryViolation"}))
	require.False(t, isKnowledgeBoundaryViolation(&AgentError{Code: "Internal"}))
}
