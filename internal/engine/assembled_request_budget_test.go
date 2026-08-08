package engine

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/prompt"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deepestProductionSession is the most exchanges a shipped session can reach:
// agent.http.max_session_turns may not exceed config.MaxSessionTurnsCeiling.
// These fixtures anchor to that rather than to a replay-window constant, because
// there is no replay window any more — the whole point below is that a size
// budget, not a count, decides what survives.
const deepestProductionSession = config.MaxSessionTurnsCeiling

// The failure this pins: maxReplayedHistoryRunes is applied at turn ENTRY, when
// the turn has run no tools, and is never re-checked while the turn accumulates
// up to maxReadExpensiveCallsPerTurn results that are re-sent every round. The
// message-count cap that stood beside it counted messages, not size, and
// maxTranscriptTotalRunes only bounds what is persisted after the turn — so
// nothing bounded the live request.
//
// Measured before the fix, with history at budget and a full window of
// transcript-bearing turns: 20 reads at the p90 result size (4142 runes)
// assembled 142,856 runes and 30 assembled 184,696, against a context window
// whose measured FLOOR is 130,000. That is a provider rejection of the whole
// request, not a degraded answer.
//
// Sizes are measured, not invented: across 182 persisted transcript messages the
// p90 is 4,142 runes and 2 messages hit the 6,000 per-message capture cap.
func TestAssembledRequestStaysUnderBudgetOnHighFanoutTurns(t *testing.T) {
	for _, perResult := range []int{2000, 4142, 6000} {
		t.Run(fmt.Sprintf("result=%drunes", perResult), func(t *testing.T) {
			e := highFanoutEngine(t, maxReadExpensiveCallsPerTurn, perResult)
			tools := productionToolWindow()

			raw := messagesFromAgentContext(e.messages, e.turnContextViewThisTurn, e.turnContextViewReady)
			require.Greater(t, assembledRequestRunes(raw)+toolWindowRunes(tools), maxAssembledRequestRunes,
				"premise: the unbounded request must actually overflow, or this test proves nothing")

			out := e.buildMessagesForLLM(tools)

			assert.LessOrEqual(t, assembledRequestRunes(out)+toolWindowRunes(tools), maxAssembledRequestRunes,
				"%d reads x %d runes assembled past the request ceiling (messages=%d + tools=%d)",
				maxReadExpensiveCallsPerTurn, perResult,
				assembledRequestRunes(out), toolWindowRunes(tools))
			assertToolCallPairsValid(t, out)

			// The current question always survives, and it is the last user message.
			rendered := renderTestMessages(out)
			assert.Contains(t, rendered, currentFanoutQuestion)

			// Shedding takes the OLDEST first. The newest in-turn tool result is the
			// one the model just asked for; dropping that instead would satisfy the
			// size bound and destroy the turn.
			assert.Contains(t, rendered, fmt.Sprintf("结果-%d", maxReadExpensiveCallsPerTurn-1),
				"the newest tool result must survive")
		})
	}
}

// The other direction: an ordinary turn must not be trimmed at all. Without this,
// a bound that simply shed everything would pass the test above.
func TestOrdinaryTurnIsNotTrimmedByTheRequestBudget(t *testing.T) {
	// p90 across 648 replayed production turns is 2 tool calls.
	e := highFanoutEngine(t, 2, 4142)

	raw := messagesFromAgentContext(e.messages, e.turnContextViewThisTurn, e.turnContextViewReady)
	out := e.buildMessagesForLLM(productionToolWindow())

	require.Equal(t, len(raw), len(out),
		"a p90-shaped turn was trimmed; the budget is sized for the tail, not for ordinary traffic")
	assert.LessOrEqual(t, assembledRequestRunes(out)+toolWindowRunes(productionToolWindow()),
		maxAssembledRequestRunes)

	// The detail budget is applied before assembly, but it compacts old tool
	// details rather than throwing away their user/assistant exchanges. A normal
	// 20-turn session must therefore retain its complete conversational thread.
	rendered := renderTestMessages(out)
	survivors := 0
	for i := 0; i < deepestProductionSession; i++ {
		if strings.Contains(rendered, fmt.Sprintf("问题%d", i)) {
			survivors++
		}
	}
	assert.Equal(t, deepestProductionSession, survivors,
		"only %d of %d exchanges reached an ordinary turn's request", survivors, deepestProductionSession)
	assert.Contains(t, rendered, fmt.Sprintf("问题%d", deepestProductionSession-1),
		"and the newest exchange is never the one dropped")
}

// History is shed before in-turn tool results: it is context, and the turn can be
// answered without it, whereas a result the model requested this turn may have to
// be fetched again.
// Stated as the precedence rule itself, not as "some history is missing". The
// compile-time maxReplayedHistoryRunes budget ALREADY removes the oldest
// exchanges before assembly, so an assertion like NotContains("问题0") holds
// whether or not the assembly pass sheds anything — a first version asserted
// exactly that and survived a mutation which disabled history shedding outright.
//
// The invariant that distinguishes them: if the assembly pass had to shed an
// in-turn tool group, it must first have shed ALL replayed history.
func TestHistoryIsShedBeforeThisTurnsToolResults(t *testing.T) {
	e := highFanoutEngine(t, maxReadExpensiveCallsPerTurn, 4142)

	raw := messagesFromAgentContext(e.messages, e.turnContextViewThisTurn, e.turnContextViewReady)
	out := e.buildMessagesForLLM(productionToolWindow())

	rawHistory, rawGroups := replayedExchanges(raw), inTurnToolGroups(raw)
	gotHistory, gotGroups := replayedExchanges(out), inTurnToolGroups(out)

	require.Greater(t, rawHistory, 0, "premise: history reached assembly at all")
	require.Less(t, gotGroups, rawGroups,
		"premise: this turn must be heavy enough that in-turn groups had to be shed")

	assert.Equal(t, 0, gotHistory,
		"an in-turn tool group was shed (%d of %d survive) while %d replayed exchanges were kept. "+
			"History is context and the turn survives without it; a result the model requested this "+
			"turn may have to be fetched again, so history goes first and goes entirely",
		gotGroups, rawGroups, gotHistory)
}

// replayedExchanges counts user messages before the current question — one per
// restored exchange, whether it is a plain pair or a projected transcript.
func replayedExchanges(msgs []openai.ChatCompletionMessage) int {
	end := currentTurnStart(msgs)
	if end < 0 {
		return 0
	}
	n := 0
	for _, msg := range msgs[:end] {
		if msg.Role == openai.ChatMessageRoleUser {
			n++
		}
	}
	return n
}

// inTurnToolGroups counts assistant messages carrying tool_calls at or after the
// current question — one per round of this turn's own tool work.
func inTurnToolGroups(msgs []openai.ChatCompletionMessage) int {
	start := currentTurnStart(msgs)
	if start < 0 {
		return 0
	}
	n := 0
	for _, msg := range msgs[start:] {
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0 {
			n++
		}
	}
	return n
}

const currentFanoutQuestion = "帮我全面排查一下"

// productionToolWindow is the window the deploy config actually ships:
// mutating_tools: true and ssh_ops.enabled: true. It is 40 schemas and ~22,800
// serialized runes — an order of magnitude larger than the system prompt, and
// invisible in the message list, which is how it went unbudgeted.
func productionToolWindow() []openai.Tool { return centralAgentToolWindow(true, true) }

// The tool window is not one size. Its two extremes bracket what a request can
// be asked to carry, so the budget is exercised against both rather than against
// whichever one a fixture happened to pick.
func toolWindowShapes() map[string][]openai.Tool {
	return map[string][]openai.Tool{
		"production (mutating + ssh-ops)": productionToolWindow(),
		"read-only":                       centralAgentToolWindow(false, false),
		"knowledge-only":                  centralAgentKnowledgeToolWindow(),
	}
}

// The tool window is charged at its serialized size, and that size is real
// enough to matter: the production window alone is over a fifth of the request
// budget. This is the arithmetic the previous version of the budget omitted
// entirely — messages were trimmed to 100,000 and then ~22,800 runes of schemas
// were appended by llm.ChatRequest.Tools, outside anything that had counted.
func TestToolWindowIsChargedAgainstTheRequestBudget(t *testing.T) {
	for name, tools := range toolWindowShapes() {
		cost := toolWindowRunes(tools)
		require.Greater(t, cost, 0, "%s: premise — a non-empty window must cost something", name)
		assert.Less(t, cost, maxAssembledRequestRunes/2,
			"%s window costs %d runes, over half the %d request budget: the messages it exists to "+
				"serve would have almost nothing left", name, cost, maxAssembledRequestRunes)
		t.Logf("%-32s %2d tools, %6d runes (%.1f%% of the request budget)",
			name, len(tools), cost, 100*float64(cost)/float64(maxAssembledRequestRunes))
	}
	assert.Equal(t, 0, toolWindowRunes(nil), "an empty window costs nothing")
}

// The high-fanout bound must hold for whichever window the turn is running, not
// just the one a fixture picked.
func TestRequestBudgetHoldsAcrossToolWindowShapes(t *testing.T) {
	for name, tools := range toolWindowShapes() {
		t.Run(name, func(t *testing.T) {
			e := highFanoutEngine(t, maxReadExpensiveCallsPerTurn, 4142)
			out := e.buildMessagesForLLM(tools)
			total := assembledRequestRunes(out) + toolWindowRunes(tools)
			assert.LessOrEqual(t, total, maxAssembledRequestRunes,
				"messages=%d + tools=%d = %d", assembledRequestRunes(out), toolWindowRunes(tools), total)
			assertToolCallPairsValid(t, out)
			assert.Contains(t, renderTestMessages(out), currentFanoutQuestion)
		})
	}
}

// highFanoutEngine builds an engine whose history is at budget (a full replay
// window of transcript-bearing turns at the measured median) and whose current
// turn has issued `reads` expensive reads of `perResult` runes each.
func highFanoutEngine(t *testing.T, reads, perResult int) *Engine {
	t.Helper()
	withCanonicalTranscript(t, true)

	e := &Engine{}
	e.sessionStateHydrated = true
	// The REAL system prompt, not a stand-in. A 15,000-rune block of filler stood
	// here and was wrong twice over: the real prompt is ~1,900 runes, and the tool
	// schemas it was pretending to include do not live in the message list at all —
	// they travel as llm.ChatRequest.Tools and are charged separately below.
	e.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: prompt.BuildSystemWithOptions("", e.reactPromptBuildOptions())},
	}
	for i := 0; i < deepestProductionSession; i++ {
		q, a := fmt.Sprintf("问题%d", i), fmt.Sprintf("回答%d", i)
		e.messages = append(e.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: q},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: a},
		)
		e.recentTurns = append(e.recentTurns, recordedTurn{
			User: q, Assistant: a,
			Transcript: &TranscriptV1{V: 1, Messages: []TranscriptMessage{
				{Role: openai.ChatMessageRoleUser, Content: q},
				{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{
					ID: "h" + q, Name: "DescribeCompShareInstance", Arguments: `{}`,
				}}},
				// The measured median transcript cost of a real replayed turn.
				{Role: openai.ChatMessageRoleTool, ToolCallID: "h" + q, Content: strings.Repeat("史", 5486)},
				{Role: openai.ChatMessageRoleAssistant, Content: a},
			}},
		})
	}

	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser, Content: currentFanoutQuestion,
	})
	for i := 0; i < reads; i++ {
		id := fmt.Sprintf("c%d", i)
		e.messages = append(e.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
				ID: id, Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: "DescribeCompShareInstance", Arguments: `{"UHostId":"u-1"}`},
			}}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id,
				Content: fmt.Sprintf("结果-%d", i) + strings.Repeat("资", perResult)},
		)
	}

	e.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		e, currentFanoutQuestion, "t", time.Unix(1_800_000_000, 0))
	e.turnContextViewReady = true
	return e
}
