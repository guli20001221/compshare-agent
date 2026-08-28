package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestCentralAgentPromptHasOneRuntimeContract(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("context", BuildOptions{MutatingToolsEnabled: true})
	require.Equal(t, []string{"identity", "scope_boundary", "behavior", "tool_observation_contract", "knowledge_turn_policy", "reply_style", "user_state"}, ids)

	for _, action := range workflow.RegisteredWorkflowActions() {
		require.NotContains(t, text, action, "workflow routing belongs in tool descriptions")
	}
	for _, header := range []string{"## 工作方式", "## 工具结果", "## 知识来源与检索规则", "## 回复要求"} {
		require.Equal(t, 1, strings.Count(text, header), header)
	}
	for _, required := range []string{
		"本轮唯一的业务判断者",
		"工作流负责补齐、确认、复查和执行",
		"Request* 成功前不得声称已发起操作",
		"next_step=correct_tool_call 时用户无需补充",
		"实时平台事实（目录、状态、价格、库存、实例详情等）必须查询对应只读工具",
		"知识库仅用于稳定规则和用法，不作为当前值依据",
		"选择与事实所在层一致的观察面",
		"实例声明的软件与平台入口使用实时实例能力",
		"不用另一个观察面的目录、经验或推测补成已确认结论",
		"端口号或路径名本身不证明协议、服务身份或所有权",
		"产品、资源形态、运行形态、区域、计费/存储条件和作用域/所有者",
		"不同 Pod/虚机、平台托管/Guest 自建、控制面/Guest/应用/管理器",
		"Guest 内关机、进程、监听或文件状态只证明 Guest 状态",
		"控制面结论必须来自实时平台能力或工作流",
		"普通实例回收",
		"随之回收的盘内数据无法找回",
		"抢占式实例的系统回收按其专属规则回答",
	} {
		require.Contains(t, text, required)
	}
}
