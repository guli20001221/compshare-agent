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

func TestContextCompilerPreservesCompleteFollowupContextAndStructuredTask(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	eng := &Engine{
		sessionStateHydrated: true,
		messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "system"},
			{Role: openai.ChatMessageRoleUser, Content: "Windows 终端怎么复制？"},
			{Role: openai.ChatMessageRoleAssistant, Content: "选中文本后按 Ctrl+Shift+C。"},
		},
		sessionState: SessionState{
			TaskSnapshot: TaskSnapshot{
				Goal: "给训练机扩容", Status: TaskSnapshotStatusActive,
				MissingSlots: []string{"目标容量"},
				Entities:     []SemanticEntityHint{{Kind: "instance", ID: "uhost-1", Name: "训练机", Freshness: ContinuityFreshnessFresh}},
			},
			SelectedInstanceID: "uhost-1",
		},
	}

	view := (ContextCompiler{}).Compile(eng, "粘贴呢", now)
	require.Equal(t, "粘贴呢", view.CurrentQuestion)
	require.Equal(t, []ConversationPair{{User: "Windows 终端怎么复制？", Assistant: "选中文本后按 Ctrl+Shift+C。"}}, view.RecentConversation)
	require.NotNil(t, view.ActiveTask)
	require.Equal(t, []string{"目标容量"}, view.ActiveTask.MissingSlots)
	require.NotEmpty(t, view.SelectedEntities)

	eng.sessionState.TaskSnapshot.MissingSlots[0] = "被后续修改"
	eng.messages[1].Content = "被后续修改"
	require.Equal(t, "目标容量", view.ActiveTask.MissingSlots[0], "compiled context must be immutable")
	require.Equal(t, "Windows 终端怎么复制？", view.RecentConversation[0].User)
}

func TestContextCompilerExpiresVolatileFactsWithoutPresentingTheirValues(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	eng := &Engine{sessionStateHydrated: true, sessionState: SessionState{RecentFacts: []ToolFact{
		{
			Kind: FactKindPriceQuote, SubjectID: "gpu-4090", Payload: map[string]any{"price": 9.99},
			ProducedAtUnix: now.Add(-10 * time.Minute).Unix(), TTLSeconds: 300,
		},
		{
			Kind: FactKindStockSnapshot, SubjectID: "gpu-5090", Payload: map[string]any{"count": 3},
			ProducedAtUnix: now.Add(-time.Minute).Unix(), TTLSeconds: 300,
		},
	}}}

	view := (ContextCompiler{}).Compile(eng, "现在呢", now)
	require.Len(t, view.RecentObservations, 2)
	require.Empty(t, view.RecentObservations[0].Summary)
	require.True(t, view.RecentObservations[0].RefreshRequired)
	require.Contains(t, strings.Join(view.ContinuityNotices, "\n"), "必须重新查询")
	require.Contains(t, view.RecentObservations[1].Summary, "数量 3")
	require.NotContains(t, renderAgentContextCard(view), "9.99")
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
		sessionState: SessionState{VerifiedKnowledge: []VerifiedKnowledgeTurn{{
			Question: "访问 " + token,
			Answer:   "使用 " + token,
			Evidence: knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "c1", Snippet: token}}},
		}}},
	}

	view := (ContextCompiler{}).Compile(eng, "继续", time.Now())
	eng.messages = append(eng.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "继续"})
	rendered := renderTestMessages(messagesFromAgentContext(eng.messages, view, true))
	require.NotContains(t, rendered, "must-not-survive")
	require.NotContains(t, rendered, "plain-token-123")
	require.Contains(t, rendered, "[REDACTED]")
	// The VerifiedKnowledge assertion that used to sit here checked the CARD copy,
	// which no longer exists: the model-facing projection was deleted with the
	// semantic injection. The stored entry survives for the verifier ledger, which
	// the model never reads, and the assertions above still cover everything it does.
}

func TestContextCompilerHotColdSemanticEquivalence(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	state := SessionState{
		ConversationDigest: ConversationDigest{Narrative: "用户要排查训练机", Decisions: []string{"先看监控"}},
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
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, TaskSnapshot: TaskSnapshot{
		Goal: "保留这个结构化任务", Status: TaskSnapshotStatusActive, MissingSlots: []string{"下一步"},
	}}, 1)
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
	require.Contains(t, modelInput, "保留这个结构化任务")
	require.NotContains(t, modelInput, "must-not-reach-next-turn")
}
