package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// The ReAct round ceiling used to manufacture its own amnesia.
//
// Every terminal exit in ChatWithOptions appends its reply to e.messages before
// returning — except one. The bare "轮次超限" refusal returned without appending.
// The HTTP layer stores whatever Chat returns, so that refusal DID reach the
// database. The consequence was a session whose two histories disagreed:
//
//	hot engine (still in the pool):  ... tool, tool, tool          <- no answer
//	cold rebuild (read from the DB): ... user, assistant(refusal)  <- has it
//
// On the hot engine the user's NEXT message therefore landed directly after a
// run of tool results with no assistant turn in between — a malformed
// conversation the model then had to make sense of — while a session that had
// been evicted and rebuilt saw the correct one. Same session, same user, two
// different pasts, decided by whether the LRU happened to evict.
//
// None of the storage work can fix this: the divergence is created entirely in
// memory, on the write side, before any of it runs.
// ---------------------------------------------------------------------------

// prose projects an engine's history down to what the messages table can hold:
// non-empty user/assistant turns. This is precisely what RehydrateHistory reads
// back (engine.go: it skips empty content and every non-user/assistant role), so
// it is the only surface on which "hot" and "cold" can be compared at all.
func prose(msgs []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	var out []openai.ChatCompletionMessage
	for _, m := range msgs {
		if m.Content == "" {
			continue
		}
		if m.Role != openai.ChatMessageRoleUser && m.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		out = append(out, openai.ChatCompletionMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

// exhaustTheRoundCeiling drives a turn that burns every ReAct round on tool
// calls and never produces an answer. The tool is deliberately NOT
// SearchKnowledge, so the evidence ledger stays empty and synthesizeOnBudget-
// Exceeded declines to recover — which is what lands the turn on the bare
// refusal instead of a synthesized answer. (It used to be GetGPUSpecs, deleted
// with the static GPU table; any non-SearchKnowledge read serves the purpose.)
func exhaustTheRoundCeiling(t *testing.T, userMsg string) (*Engine, string) {
	t.Helper()
	responses := make([]llm.ChatResponse, maxReActRounds+1)
	for i := range responses {
		responses[i] = llm.ChatResponse{
			ToolCalls: []openai.ToolCall{toolCall("tc", "DescribeCompShareInstance", `{}`)},
		}
	}
	eng := NewWithDeps(&mockLLM{responses: responses}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), userMsg, noopStep)
	require.NoError(t, err)
	require.Equal(t, reactCeilingRefusal, reply,
		"precondition: the turn must land on the bare ceiling refusal (no evidence to recover from)")
	require.Empty(t, eng.searchKnowledgeHitsThisTurn,
		"precondition: an empty ledger is what prevents the recovery paths from firing")
	return eng, reply
}

// The invariant, stated directly: the answer the user is given is the answer the
// engine remembers giving.
//
// Mutation: drop the e.messages append before `return reactCeilingRefusal` and
// this fails — the last message is a tool result.
func TestChat_ReactCeiling_TheRefusalTheUserSeesIsTheOneTheEngineRemembers(t *testing.T) {
	eng, reply := exhaustTheRoundCeiling(t, "4090的规格是什么")

	last := eng.messages[len(eng.messages)-1]
	assert.Equal(t, openai.ChatMessageRoleAssistant, string(last.Role),
		"the turn must not end on a tool result — the next user message would attach straight to it")
	assert.Equal(t, reply, last.Content,
		"the reply returned to the caller (and written to the DB) must also be in the engine's own history")
}

// The invariant restated as the failure it actually caused: hot and cold must
// see the same conversation.
//
// This is the stronger of the two gates. It does not assert on an
// implementation detail (an append); it reconstructs what the HTTP layer stores
// for this turn, rebuilds the engine from it the way agentpool does after an
// eviction, and demands the two agree. If they do not, the same session gives
// different answers depending on whether the LRU evicted it — which is the
// user-visible bug, and the reason none of the storage work would have caught
// this one.
//
// Mutation: drop the same append and the two histories differ by exactly the
// assistant refusal.
func TestChat_ReactCeiling_HotEngineAndColdRebuildSeeTheSameConversation(t *testing.T) {
	const userMsg = "4090的规格是什么"
	hot, reply := exhaustTheRoundCeiling(t, userMsg)

	// What the HTTP layer persists for this turn: the user row, then the
	// assistant row patched with whatever Chat returned. Nothing else — the
	// messages table has no tool columns.
	dbRows := []HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: userMsg},
		{Role: openai.ChatMessageRoleAssistant, Content: reply},
	}

	// What agentpool does after the entry is evicted: build a fresh engine and
	// replay those rows.
	cold := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	cold.RehydrateHistory(dbRows)

	assert.Equal(t, prose(cold.messages), prose(hot.messages),
		"the conversation the hot engine remembers must equal the one a cold rebuild reads back — "+
			"otherwise this session's past depends on whether the LRU happened to evict it")
}
