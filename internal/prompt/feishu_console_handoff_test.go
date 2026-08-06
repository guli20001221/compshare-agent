package prompt

import (
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/stretchr/testify/require"
)

func TestFeishuConsoleHandoffPromptIsOptInOnly(t *testing.T) {
	plain, plainSections := BuildSystemWithOptionsAndTrace("ctx", BuildOptions{})
	require.NotContains(t, plain, agentprotocol.FeishuConsoleHandoffMarker)
	require.NotContains(t, plainSections, "feishu_console_handoff")

	handoff, sections := BuildSystemWithOptionsAndTrace("ctx", BuildOptions{FeishuConsoleHandoff: true})
	require.Contains(t, handoff, agentprotocol.FeishuConsoleHandoffMarker)
	require.Contains(t, handoff, "不能读取、推断或操作任何用户账号、实例")
	require.Contains(t, handoff, "你只能依据已检索到的产品知识回答")
	require.Contains(t, handoff, "前面的知识库回复仍未解决")
	require.Contains(t, sections, "feishu_console_handoff")
}
