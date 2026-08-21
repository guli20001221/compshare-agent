package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestContextCompilerPreservesCompleteFollowupContextAndLiveSelection(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	eng := &Engine{
		sessionStateHydrated: true,
		messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "system"},
			{Role: openai.ChatMessageRoleUser, Content: "Windows 终端怎么复制？"},
			{Role: openai.ChatMessageRoleAssistant, Content: "选中文本后按 Ctrl+Shift+C。"},
		},
		sessionState: SessionState{
			SelectedInstanceID:     "uhost-1",
			SelectedInstanceName:   "训练机",
			SelectedInstanceSource: SelectedInstanceSourceUser,
		},
	}

	view := (ContextCompiler{}).Compile(eng, "粘贴呢", now)
	require.Equal(t, "粘贴呢", view.CurrentQuestion)
	require.Equal(t, []ConversationPair{{User: "Windows 终端怎么复制？", Assistant: "选中文本后按 Ctrl+Shift+C。"}}, view.RecentConversation)
	require.NotEmpty(t, view.SelectedEntities)
	require.Equal(t, "训练机", view.SelectedEntities[0].Name)

	eng.sessionState.SelectedInstanceName = "被后续修改"
	eng.messages[1].Content = "被后续修改"
	require.Equal(t, "训练机", view.SelectedEntities[0].Name, "compiled context must be immutable")
	require.Equal(t, "Windows 终端怎么复制？", view.RecentConversation[0].User)
}

func TestContextCompilerRedactsSecretsAndNeverCarriesPriorRawToolJSON(t *testing.T) {
	token := "http://1.2.3.4:8888/lab?token=plain-token-123"
	eng := &Engine{
		sessionStateHydrated: true,
		messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "system"},
			{Role: openai.ChatMessageRoleUser, Content: "打开 " + token},
			{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "old-tool"}}},
			{Role: openai.ChatMessageRoleTool, ToolCallID: "old-tool", Content: `{"huge_secret_payload":"must-not-survive"}`},
			{Role: openai.ChatMessageRoleAssistant, Content: "已处理 " + token},
		},
		sessionState: SessionState{VerifiedEvidence: []VerifiedEvidenceTurn{{
			Question: "访问 " + token,
			Evidence: knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "c1", Snippet: token}}},
		}}},
	}

	view := (ContextCompiler{}).Compile(eng, "继续", time.Now())
	eng.messages = append(eng.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "继续"})
	rendered := renderTestMessages(messagesFromAgentContext(eng.messages, view, true))
	require.NotContains(t, rendered, "must-not-survive")
	require.NotContains(t, rendered, "plain-token-123")
	require.Contains(t, rendered, "[REDACTED]")
}

func TestContextCompilerHotColdSemanticEquivalence(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	state := SessionState{
		SelectedInstanceID: "uhost-1", SelectedInstanceName: "训练机", SelectedInstanceFreshness: ContinuityFreshnessFresh,
	}
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "CPU 为什么高"},
		{Role: openai.ChatMessageRoleAssistant, Content: "先查看最近五分钟监控。"},
	}
	hot := &Engine{sessionStateHydrated: true, sessionState: state, messages: append([]openai.ChatCompletionMessage(nil), messages...)}
	cold := &Engine{sessionStateHydrated: true, sessionState: state, messages: append([]openai.ChatCompletionMessage(nil), messages...)}

	require.Equal(t,
		(ContextCompiler{}).Compile(hot, "还是那台吗", now),
		(ContextCompiler{}).Compile(cold, "还是那台吗", now),
	)
}

func TestMessagesFromAgentContextIncludesCurrentQuestionExactlyOnce(t *testing.T) {
	view := AgentContext{
		CurrentQuestion: "粘贴呢",
		RecentConversation: []ConversationPair{{
			User: "怎么复制", Assistant: "按 Ctrl+Shift+C",
		}},
	}
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "怎么复制"},
		{Role: openai.ChatMessageRoleAssistant, Content: "按 Ctrl+Shift+C"},
		{Role: openai.ChatMessageRoleUser, Content: "粘贴呢"},
	}
	out := messagesFromAgentContext(messages, view, true)
	require.Equal(t, 1, strings.Count(renderTestMessages(out), "粘贴呢"))
	require.Equal(t, openai.ChatMessageRoleUser, out[len(out)-1].Role)
}

func TestChatModelHistoryComesFromAgentContextNotRetainedRawTranscript(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "继续处理"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "怎么复制"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "old", Function: openai.FunctionCall{Name: "SearchKnowledge"}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "old", Content: `{"raw_tool_payload":"must-not-reach-next-turn"}`},
		{Role: openai.ChatMessageRoleAssistant, Content: "复制用 Ctrl+Shift+C"},
	}

	_, err := eng.Chat(context.Background(), "粘贴呢", noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)
	modelInput := renderTestMessages(mock.calls[0].Messages)
	require.Contains(t, modelInput, "怎么复制")
	require.Contains(t, modelInput, "复制用 Ctrl+Shift+C")
	require.Contains(t, modelInput, "粘贴呢")
	require.NotContains(t, modelInput, "must-not-reach-next-turn")
}
