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
		"实例及盘内数据均无法找回",
	} {
		require.Contains(t, text, required)
	}
}
