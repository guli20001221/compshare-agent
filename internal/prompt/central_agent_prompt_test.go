package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestCentralAgentPromptContainsOneContractAndNoLegacyWorkflowCatalog(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("context", BuildOptions{MutatingToolsEnabled: true})
	require.Equal(t, []string{"identity", "scope_boundary", "behavior", "knowledge_turn_policy", "reply_style", "user_state"}, ids)
	for _, action := range workflow.RegisteredWorkflowActions() {
		require.NotContains(t, text, action)
	}
	require.Equal(t, 1, strings.Count(text, "需要平台文档或新的技术证据时再检索"))
	require.Contains(t, text, "本轮唯一的业务判断者")
	require.Contains(t, text, "只有用户明确要求实际改变资源")
	require.Contains(t, text, "动作建议本身不会执行操作")
}
