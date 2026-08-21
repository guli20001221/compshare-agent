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
	return validateHandoffHTTPSURL("agent.feishu.console_assistant_url", raw, true)
}

// validateClientDownloadURL accepts an optional official desktop-client
// download page. Unlike the console URL, it is optional so existing
// deployments can continue to offer the web-only handoff.
func validateClientDownloadURL(raw string) (string, error) {
	return validateHandoffHTTPSURL("agent.feishu.client_download_url", raw, false)
}

func validateHandoffHTTPSURL(field, raw string, required bool) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		if required {
			return "", fmt.Errorf("%s is required", field)
		}
		return "", nil
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("%s must be an absolute HTTPS URL without user credentials", field)
	}
	return parsed.String(), nil
}

func consoleHandoffEnabled(cfg config.FeishuConfig) bool {
	return strings.TrimSpace(cfg.ConsoleAssistantURL) != ""
}

// consumeConsoleHandoffMarker removes the protocol-only marker before reply
// rendering. It remains defensive even when the feature is disabled: an
// accidental marker must never be shown to a user.
func consumeConsoleHandoffMarker(answer string) (string, bool) {
	withoutMarker := strings.ReplaceAll(answer, agentprotocol.FeishuConsoleHandoffMarker, "")
	return strings.TrimSpace(withoutMarker), withoutMarker != answer
}

// consumeCustomerSupportMarker removes the protocol-only support marker
// before reply rendering. It remains defensive even when the feature is
// disabled: an accidental marker must never be shown to a user.
func consumeCustomerSupportMarker(answer string) (string, bool) {
	withoutMarker := strings.ReplaceAll(answer, agentprotocol.FeishuCustomerSupportMarker, "")
	return strings.TrimSpace(withoutMarker), withoutMarker != answer
}

func customerSupportReply() string {
	return "这属于账号、认证、页面加载或平台服务问题，建议直接联系优云智算客服处理。请附上截图和发生时间；请勿在群内发送验证码、密码或证件信息。"
}

func consoleHandoffReply(consoleURL, clientURL string) string {
	reply := "## 需要在已登录环境中继续排查\n\n" +
		"这个问题需要结合您账号下实例的实时状态、日志或进程信息判断；飞书群里的知识问答无法读取或操作这些信息。\n\n" +
		"你可以任选一种方式继续：\n\n" +
		"1. **网页控制台**：打开[优云智算控制台智能助手](" + consoleURL + ")，重新描述问题；如需实例内诊断，请提供对应实例 ID（或实例名称），也可以重新上传截图。"
	if strings.TrimSpace(clientURL) != "" {
		reply += "\n\n2. **桌面客户端（适合直接处理实例问题）**：下载并登录[优云智算客户端](" + clientURL + ")。客户端中的 Agent 可在您的已登录账号下使用 CLI 和 SSH 读取实例状态、排查并执行修复；涉及释放实例、删除数据等敏感操作仍需您确认。"
	}
	return reply
}

func appendConsoleHandoff(answer, consoleURL, clientURL string) string {
	notice := consoleHandoffReply(consoleURL, clientURL)
	if strings.TrimSpace(answer) == "" {
		return notice
	}
	return strings.TrimSpace(answer) + "\n\n---\n\n" + notice
}

func isKnowledgeBoundaryViolation(err error) bool {
	var agentErr *AgentError
	return errors.As(err, &agentErr) && agentErr.Code == "KnowledgeBoundaryViolation"
}
