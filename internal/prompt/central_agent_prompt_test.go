package prompt

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestCentralAgentPromptContainsOneContractAndNoLegacyWorkflowCatalog(t *testing.T) {
	text, ids := BuildSystemWithOptionsAndTrace("context", BuildOptions{MutatingToolsEnabled: true, CentralAgentRuntime: true})
	require.Equal(t, []string{"identity", "scope_boundary", "behavior", "knowledge_turn_policy", "reply_style", "user_state"}, ids)
	for _, action := range workflow.RegisteredWorkflowActions() {
		require.NotContains(t, text, action)
	}
	require.Equal(t, 1, strings.Count(text, "需要新事实或确认时效时，调用 SearchKnowledge"))
	require.Contains(t, text, "本轮唯一的业务判断者")
	require.Contains(t, text, "结构化候选")
}
