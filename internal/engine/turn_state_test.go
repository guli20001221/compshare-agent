package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/llm"
)

// The previous turn's user-facing buffers must not be composed into this turn's
// reply. They used to be cleared by name in a reset block; now the turn is one
// value that entry replaces, so this covers every field in it at once.
func TestTurnEntryReplacesTheWholeTurn(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.turnState = turnState{
		verbatimBlocksThisTurn:            []string{"上一轮的账单明细"},
		committedWriteRepliesThisTurn:     []string{"上一轮已创建 uhost-leak"},
		sensitiveRepliesThisTurn:          []string{"上一轮返回的密码"},
		lastConfirmationTerminalReason:    "declined",
		actionProposalDispositionThisTurn: "confirmation",
		knowledgeOnlyThisTurn:             true,
		readExpensiveCallsThisTurn:        7,
	}

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)
	require.Equal(t, "ok", reply,
		"a previous turn's verbatim block must not be prepended to this turn's reply")
	require.Empty(t, eng.verbatimBlocksThisTurn)
	require.Empty(t, eng.committedWriteRepliesThisTurn)
	require.Empty(t, eng.sensitiveRepliesThisTurn)
	require.Empty(t, eng.lastConfirmationTerminalReason)
	require.Empty(t, eng.actionProposalDispositionThisTurn)
	require.False(t, eng.knowledgeOnlyThisTurn)
	require.Zero(t, eng.readExpensiveCallsThisTurn)
}

// newTurnState is the only place the turn's non-zero starting values are
// written, so a turn cannot begin with grounding already reported as passing or
// with a nil cache a caller would read as "not a turn".
func TestNewTurnStateStartsGroundingUnobservedAndCachesAllocated(t *testing.T) {
	state := newTurnState("帮我查一下", ChatOptions{ImageContext: "screenshot"})

	require.Equal(t, "帮我查一下", state.lastUserMsg)
	require.Equal(t, "screenshot", state.imageContextThisTurn)
	require.Equal(t, evidenceUpdateNone, state.verifiedEvidenceUpdateThisTurn)
	require.Equal(t, "unavailable", state.groundingOutcomeThisTurn)
	require.NotNil(t, state.verifiedInstanceEvidenceThisTurn)
	require.NotNil(t, state.toolResultsByCallThisTurn)
}

// KnowledgeOnly is the narrower authorization, so it wins; the support renderer
// is a delivery choice and follows the channel rather than the winner.
func TestNewTurnStateDerivesTheChannelReductionsOnce(t *testing.T) {
	state := newTurnState("", ChatOptions{KnowledgeOnly: true, PublicPlatformReadOnly: true})
	require.True(t, state.knowledgeOnlyThisTurn)
	require.False(t, state.publicPlatformReadOnlyThisTurn)
	require.True(t, state.feishuSupportRendererThisTurn)

	console := newTurnState("", ChatOptions{})
	require.False(t, console.knowledgeOnlyThisTurn)
	require.False(t, console.publicPlatformReadOnlyThisTurn)
	require.False(t, console.feishuConsoleHandoffThisTurn)
	require.False(t, console.feishuSupportRendererThisTurn)
}

// A public Feishu turn narrows the model's completion contract. The prompt is
// built under the turn's scope, and the two rebuilds that run outside any turn
// pass the zero scope — so neither a cold rebuild on a pooled engine nor the
// next console turn can inherit the private handoff contract.
func TestPublicChannelContractDoesNotOutliveItsTurn(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}, {Content: "ok"}}}, &mockExecutor{}, nil)
	eng.RehydrateHistory(nil)

	_, err := eng.ChatWithOptions(context.Background(), "抢占式实例是什么", noopStep, ChatOptions{
		FeishuConsoleHandoff: true,
	})
	require.NoError(t, err)
	require.Contains(t, eng.messages[0].Content, agentprotocol.FeishuConsoleHandoffMarker,
		"the Feishu turn itself must be built under its own contract")

	// The cold path: agentpool rebuilds a pooled engine's history between turns.
	eng.RehydrateHistory([]HistoryMessage{{Role: "user", Content: "帮我看下实例"}})
	require.NotContains(t, eng.messages[0].Content, agentprotocol.FeishuConsoleHandoffMarker,
		"a rebuild outside any turn must not inherit the previous turn's channel contract")

	_, err = eng.ChatWithOptions(context.Background(), "帮我看下实例", noopStep, ChatOptions{})
	require.NoError(t, err)
	require.NotContains(t, eng.messages[0].Content, agentprotocol.FeishuConsoleHandoffMarker,
		"an ordinary console turn must not inherit it either")
	require.False(t, eng.feishuConsoleHandoffThisTurn)
	require.False(t, eng.feishuSupportRendererThisTurn)
}
