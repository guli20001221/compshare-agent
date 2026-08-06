package feishu

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/config"
)

// validateConsoleAssistantURL accepts the configured, user-facing console URL.
// It deliberately permits a fragment because the console may be a single-page
// application. The URL is deployment configuration, never taken from a Feishu
// message.
func validateConsoleAssistantURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("agent.feishu.console_assistant_url is required when enable_console_handoff is true")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("agent.feishu.console_assistant_url must be an absolute HTTPS URL without user credentials")
	}
	return parsed.String(), nil
}

func consoleHandoffEnabled(cfg config.FeishuConfig) bool {
	return cfg.EnableConsoleHandoff && strings.TrimSpace(cfg.ConsoleAssistantURL) != ""
}

// consumeConsoleHandoffMarker removes the protocol-only marker before reply
// rendering. It remains defensive even when the feature is disabled: an
// accidental marker must never be shown to a user.
func consumeConsoleHandoffMarker(answer string) (string, bool) {
	withoutMarker := strings.ReplaceAll(answer, agentprotocol.FeishuConsoleHandoffMarker, "")
	return strings.TrimSpace(withoutMarker), withoutMarker != answer
}

func consoleHandoffReply(consoleURL string) string {
	return "## 需要在控制台继续排查\n\n" +
		"这个问题需要结合您账号下实例的实时状态、日志或进程信息判断；飞书群里的知识问答无法读取这些信息。\n\n" +
		"请打开[优云智算控制台智能助手](" + consoleURL + ")，重新描述问题；如需实例内诊断，请在对话中提供对应实例 ID（或实例名称），也可以重新上传截图。"
}

func appendConsoleHandoff(answer, consoleURL string) string {
	notice := consoleHandoffReply(consoleURL)
	if strings.TrimSpace(answer) == "" {
		return notice
	}
	return strings.TrimSpace(answer) + "\n\n---\n\n" + notice
}

func isKnowledgeBoundaryViolation(err error) bool {
	var agentErr *AgentError
	return errors.As(err, &agentErr) && agentErr.Code == "KnowledgeBoundaryViolation"
}
