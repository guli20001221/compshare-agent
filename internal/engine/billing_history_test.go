package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

const (
	billingQuestion = "这台实例现在每小时多少钱？"
	billingFollowUp = "那一个月大概多少钱？"
	billingPrice    = "3.20"
)

func billingHistoryExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId":       "uhost-bill-001",
				"State":         "Running",
				"ChargeType":    "Dynamic",
				"InstancePrice": float64(3.2),
				"DiskPrice":     float64(0.1),
			}},
		},
	}}
}

// runBillingHistoryTurn exercises the real Agent path rather than fabricating a
// transcript: DiagnoseBilling renders a user card, compose adds it to reply,
// capture records the model view, and rebuildCold applies the actual persistence
// transforms before RehydrateHistory reads it back.
func runBillingHistoryTurn(t *testing.T, tail string) (*Engine, string, json.RawMessage) {
	t.Helper()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`)}},
		{Content: tail},
	}}
	hot := NewWithDeps(mock, billingHistoryExecutor(), nil)
	hot.mutatingToolsEnabled = false
	hot.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	reply, err := hot.Chat(context.Background(), billingQuestion, noopStep)
	require.NoError(t, err)
	metadata, stats := hot.LastTurnTranscript()
	require.True(t, stats.Attempted, "precondition: billing tool traffic must produce a canonical transcript")
	require.NotNil(t, metadata, "precondition: the canonical transcript must persist")
	return hot, reply, metadata
}

// rebuildColdAfterHTTPPersistence follows the actual storage boundary for the
// one class of turn where display text and model history intentionally differ.
// The generic canonical-transcript fixtures exercise raw transcript transforms;
// this helper exercises Chat -> compose -> persistence -> rehydrate.
func rebuildColdAfterHTTPPersistence(reply string, metadata json.RawMessage) *Engine {
	cold := NewWithDeps(&mockLLM{}, billingHistoryExecutor(), nil)
	cold.mutatingToolsEnabled = false
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: guardrails.RedactPII(billingQuestion)},
		{Role: openai.ChatMessageRoleAssistant, Content: guardrails.RedactOutputLeak(reply), Transcript: metadata},
	})
	return cold
}

func nextModelRequest(t *testing.T, engine *Engine) []openai.ChatCompletionMessage {
	t.Helper()
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "已收到。"}}}
	engine.llmClient = mock
	_, err := engine.Chat(context.Background(), billingFollowUp, noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)
	return mock.calls[0].Messages
}

func TestPureBillingCardNeverEntersColdModelHistory(t *testing.T) {
	hot, reply, metadata := runBillingHistoryTurn(t, "")
	require.Contains(t, reply, billingPrice, "precondition: the user-facing card must retain the authoritative amount")
	require.Len(t, hot.verbatimBlocksThisTurn, 1, "precondition: this is the verbatim billing path")
	require.Equal(t, strings.TrimSpace(hot.verbatimBlocksThisTurn[0]), strings.TrimSpace(reply),
		"the pure-billing UI reply remains exactly the card")
	require.NotContains(t, reply, verbatimBillingHistoryCompletion,
		"the model-only completion must never add user-visible prose")

	hotAssembled := nextModelRequest(t, hot)
	cold := rebuildColdAfterHTTPPersistence(reply, metadata)
	coldAssembled := nextModelRequest(t, cold)

	require.Equal(t, hotAssembled, coldAssembled,
		"a restart must not change the model context after a pure billing card")
	replayed := renderReplayedRegion(t, hotAssembled)
	requireTranscriptWasReplayed(t, hotAssembled)
	require.Contains(t, replayed, verbatimBillingHistoryCompletion,
		"the completed model-only billing turn must survive to the next request")
	require.Contains(t, replayed, verbatimBlockObservation,
		"the model retains the amount-free billing contract, not the rendered card")
	require.NotContains(t, replayed, billingPrice,
		"the server-rendered amount must never re-enter the model after a restart")
}

func TestBillingHistoryTellsTheAgentToRefreshRatherThanRefuseARepeatQuestion(t *testing.T) {
	hot, _, _ := runBillingHistoryTurn(t, "")
	assembled := nextModelRequest(t, hot)
	replayed := renderReplayedRegion(t, assembled)

	require.Contains(t, replayed, "用户再次询问当前报价时调用 DiagnoseBilling",
		"the model sees an actionable fresh-query instruction, not a blanket no-repeat rule")
	require.Contains(t, replayed, "询问计费规则时检索知识",
		"a policy follow-up must not be treated as another request for the current quote")
	require.NotContains(t, replayed, "不要复述、计算或推断金额",
		"the old rule caused a repeat billing question to fail despite a live billing tool")
}

func TestMixedBillingCardRehydratesTheModelTailNotTheDisplayCard(t *testing.T) {
	const tail = "CPU 100% 的原因是 3 个 kworkerd 进程占满了核心。"
	hot, reply, metadata := runBillingHistoryTurn(t, tail)
	require.Contains(t, reply, billingPrice, "precondition: the persisted display reply contains the card")
	require.Contains(t, reply, tail, "precondition: the model also answered the non-billing part")

	hotAssembled := nextModelRequest(t, hot)
	cold := rebuildColdAfterHTTPPersistence(reply, metadata)
	coldAssembled := nextModelRequest(t, cold)

	require.Equal(t, hotAssembled, coldAssembled,
		"a restart must replay the model tail, not the composed display reply")
	replayed := renderReplayedRegion(t, coldAssembled)
	requireTranscriptWasReplayed(t, coldAssembled)
	require.Contains(t, replayed, tail, "the non-billing answer remains useful cross-turn context")
	require.Contains(t, replayed, verbatimBlockObservation,
		"the billing card's amount-free observation remains in the transcript")
	require.NotContains(t, replayed, billingPrice,
		"the card amount is display data, not model context")
}

func TestOlderPureBillingTranscriptFailsClosedWithoutReplayingItsCard(t *testing.T) {
	observation := agentToolObservation("DiagnoseBilling", fmt.Sprintf(
		`{"observation":%q,"verbatim_delivered":true}`, verbatimBlockObservation))
	transcript := &TranscriptV1{V: transcriptSchemaVersion, Messages: []TranscriptMessage{
		{Role: openai.ChatMessageRoleUser, Content: billingQuestion},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{ID: "tc1", Name: "DiagnoseBilling", Arguments: `{}`}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "tc1", Name: "DiagnoseBilling", Content: observation},
	}}
	metadata, err := marshalTranscriptMetadata(transcript)
	require.NoError(t, err)

	cold := NewWithDeps(&mockLLM{}, billingHistoryExecutor(), nil)
	cold.mutatingToolsEnabled = false
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: guardrails.RedactPII(billingQuestion)},
		{Role: openai.ChatMessageRoleAssistant, Content: guardrails.RedactOutputLeak("【费用明细】每小时 " + billingPrice + " 元"), Transcript: metadata},
	})
	assembled := assembleNextTurn(cold, billingFollowUp)
	replayed := renderReplayedRegion(t, assembled)

	require.Contains(t, replayed, verbatimBillingHistoryCompletion,
		"older tool-terminal billing rows get the same safe completion")
	require.NotContains(t, replayed, billingPrice,
		"an old UI card must fail closed instead of becoming model memory")
}

func TestOnlyDiagnoseBillingVerbatimMarkerChangesColdAssistantContent(t *testing.T) {
	observation := agentToolObservation("DescribeCompShareInstance", `{"verbatim_delivered":true}`)
	transcript := &TranscriptV1{V: transcriptSchemaVersion, Messages: []TranscriptMessage{
		{Role: openai.ChatMessageRoleUser, Content: "查实例"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{ID: "tc1", Name: "DescribeCompShareInstance", Arguments: `{}`}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "tc1", Name: "DescribeCompShareInstance", Content: observation},
		{Role: openai.ChatMessageRoleAssistant, Content: "原样保留的普通答案"},
	}}
	metadata, err := marshalTranscriptMetadata(transcript)
	require.NoError(t, err)

	cold := NewWithDeps(&mockLLM{}, billingHistoryExecutor(), nil)
	cold.mutatingToolsEnabled = false
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "查实例"},
		{Role: openai.ChatMessageRoleAssistant, Content: "展示层答案", Transcript: metadata},
	})
	assembled := assembleNextTurn(cold, "然后呢？")
	replayed := renderReplayedRegion(t, assembled)

	require.Contains(t, replayed, "展示层答案",
		"an unrelated tool's data flag must not rewrite ordinary assistant history")
	require.NotContains(t, replayed, verbatimBillingHistoryCompletion)
}
