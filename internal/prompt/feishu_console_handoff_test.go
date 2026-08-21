package prompt

import (
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/stretchr/testify/require"
)

func TestFeishuConsoleHandoffPromptIsOptInOnly(t *testing.T) {
	plain, plainSections := BuildSystemWithOptionsAndTrace("ctx", BuildOptions{})
	require.NotContains(t, plain, agentprotocol.FeishuConsoleHandoffMarker)
	require.NotContains(t, plain, agentprotocol.FeishuCustomerSupportMarker)
	require.NotContains(t, plainSections, "feishu_console_handoff")
	publicOnly, publicOnlySections := BuildSystemWithOptionsAndTrace("ctx", BuildOptions{FeishuPublicPlatformReadOnly: true})
	require.Contains(t, publicOnly, "GPU 规格、库存、平台/社区镜像目录、可用区、公共模型仓库和目录价")
	require.Contains(t, publicOnlySections, "feishu_public_platform_scope")
	require.NotContains(t, publicOnly, agentprotocol.FeishuConsoleHandoffMarker)
	require.NotContains(t, publicOnly, agentprotocol.FeishuCustomerSupportMarker)

	handoff, sections := BuildSystemWithOptionsAndTrace("ctx", BuildOptions{FeishuConsoleHandoff: true})
	require.Contains(t, handoff, agentprotocol.FeishuConsoleHandoffMarker)
	require.Contains(t, handoff, agentprotocol.FeishuCustomerSupportMarker)
	require.Contains(t, handoff, "不能读取、推断或操作任何用户账号、实例")
	require.Contains(t, handoff, "你只能依据已检索到的产品知识回答")
	require.Contains(t, handoff, "必须查看其日志、进程、端口或网络")
	require.Contains(t, handoff, "不要因问题模糊、资料不足或用户想继续咨询就交接")
	require.Contains(t, sections, "feishu_console_handoff")

	public, publicSections := BuildSystemWithOptionsAndTrace("ctx", BuildOptions{
		FeishuConsoleHandoff:         true,
		FeishuPublicPlatformReadOnly: true,
	})
	require.Contains(t, public, agentprotocol.FeishuConsoleHandoffMarker)
	require.Contains(t, public, agentprotocol.FeishuCustomerSupportMarker)
	require.Contains(t, public, "GPU 规格、库存、平台/社区镜像目录、可用区、公共模型仓库和目录价")
	require.Contains(t, public, "账号价格、自制/共享镜像或其他私有资源")
	require.Contains(t, public, "先用知识库和公开平台能力回答能可靠确认的部分")
	require.Contains(t, public, "不要因问题模糊、公开信息不足、账号内资源或用户想继续咨询就交接")
	require.NotContains(t, public, "你只能依据已检索到的产品知识回答")
	require.Contains(t, publicSections, "feishu_console_handoff")
}
