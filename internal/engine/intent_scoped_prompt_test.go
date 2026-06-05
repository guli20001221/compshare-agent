package engine

import (
	"testing"

	"github.com/compshare-agent/internal/intent"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMessagesForLLM_InsertsIntentScopedDiagnosisCardEphemerally(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetIntentScopedReActPromptEnabled(true)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "stable system"},
		{Role: openai.ChatMessageRoleUser, Content: "ssh 连不上"},
	}
	eng.lastPlannerIntentThisTurn = intent.IntentDiagnosis

	msgs := eng.buildMessagesForLLM()

	require.Len(t, eng.messages, 2, "ephemeral card must not persist to engine history")
	require.Len(t, msgs, 3)
	assert.Equal(t, "stable system", eng.messages[0].Content)
	assert.Equal(t, openai.ChatMessageRoleSystem, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "本轮 ReAct 诊断卡片")
	assert.Contains(t, msgs[1].Content, "DiagnoseSSH")
	assert.NotContains(t, msgs[1].Content, "CreateInstanceWorkflow")
	assert.Equal(t, openai.ChatMessageRoleUser, msgs[2].Role)
}

func TestBuildMessagesForLLM_IntentScopedOperationCardRequiresMutatingMode(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetIntentScopedReActPromptEnabled(true)
	eng.SetMutatingToolsEnabled(false)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "stable system"},
		{Role: openai.ChatMessageRoleUser, Content: "帮我关机"},
	}
	eng.lastPlannerIntentThisTurn = intent.IntentOperationLifecycle

	msgs := eng.buildMessagesForLLM()

	require.Len(t, msgs, 3)
	assert.Contains(t, msgs[1].Content, "本轮 ReAct 只读操作卡片")
	assert.NotContains(t, msgs[1].Content, "CreateInstanceWorkflow")
}
