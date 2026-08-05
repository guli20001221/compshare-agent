package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/prompt"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The narrow parity test next door compares ProjectTranscript output against the
// hot turn on a fixture built to be secret-free and under every budget, so
// redaction, truncation and round-shedding are all no-ops there. It isolates the
// rebuild, which is what it is for.
//
// This file covers the chain that actually runs once the canonical transcript is
// the sole semantic memory:
//
//	capture -> metadata -> Parse -> RehydrateHistory -> buildMessagesForLLM -> cap
//
// on turns where those transforms DO fire. Each case asserts two things, because
// either alone is worthless: that hot and cold agree, and that what they agree
// on is correct. Two paths can converge on the same wrong answer — the
// first-match-wins transcript bug did exactly that, and hot/cold parity passed
// throughout.

const parityNextQuestion = "那现在怎么办？"

// fixedBuildAt keeps AgentContext.BuiltAtUnix identical across the two engines.
// Without it the two views differ by a timestamp and every case fails for a
// reason that has nothing to do with the transcript.
var fixedBuildAt = time.Unix(1785552041, 0)

func paritySystemMessage() openai.ChatCompletionMessage {
	var e Engine
	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: prompt.BuildSystemWithOptions("", e.reactPromptBuildOptions()),
	}
}

// assembleNextTurn drives the engine one more turn and returns exactly what
// would go to the provider.
func assembleNextTurn(e *Engine, question string) []openai.ChatCompletionMessage {
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser, Content: question,
	})
	e.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(e, question, "turn-parity", fixedBuildAt)
	e.turnContextViewReady = true
	return e.buildMessagesForLLM()
}

// runHotTurn holds one turn the way a live engine does and captures it.
func runHotTurn(turn []openai.ChatCompletionMessage) (*Engine, json.RawMessage, TranscriptStats) {
	e := &Engine{messages: append([]openai.ChatCompletionMessage{paritySystemMessage()}, turn...)}
	e.captureTurnTranscript()
	payload, stats := e.LastTurnTranscript()
	return e, payload, stats
}

// rebuildCold is the restart: nothing but the persisted rows.
func rebuildCold(question, answer string, metadata json.RawMessage) *Engine {
	e := &Engine{}
	e.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: question},
		{Role: openai.ChatMessageRoleAssistant, Content: answer, Transcript: metadata},
	})
	return e
}

// renderReplayedRegion renders ONLY the replayed prior turns: everything after
// the leading system block and before the current question.
//
// Scoping matters more than it looks. The system prompt is ~20k runes of text
// that legitimately contains words these cases search for — internal/prompt
// ships the literal "截断" in a retrieval instruction — so a Contains assertion
// against the whole assembled request passes on the prompt and never inspects
// the transcript at all. A mutation that deleted the truncation notice outright
// went green that way.
func renderReplayedRegion(t *testing.T, assembled []openai.ChatCompletionMessage) string {
	t.Helper()
	start := 0
	for start < len(assembled) && assembled[start].Role == openai.ChatMessageRoleSystem {
		start++
	}
	end := currentTurnStart(assembled)
	require.GreaterOrEqual(t, end, start, "assembled request has no current question")
	return renderTestMessages(assembled[start:end])
}

// requireTranscriptWasReplayed fails unless the assembled request actually
// carries replayed tool traffic from a PRIOR turn. Without this, every parity
// assertion in this file is satisfied by two engines that both replayed nothing.
func requireTranscriptWasReplayed(t *testing.T, assembled []openai.ChatCompletionMessage) {
	t.Helper()
	currentStart := currentTurnStart(assembled)
	require.GreaterOrEqual(t, currentStart, 0, "assembled request has no user message")
	for _, msg := range assembled[:currentStart] {
		if msg.Role == openai.ChatMessageRoleTool {
			return
		}
	}
	t.Fatal("no replayed tool traffic before the current question: the transcript did not attach, " +
		"so hot and cold are equal only because both replayed a plain pair")
}

func TestEndToEndHotColdParityAcrossTransforms(t *testing.T) {
	prev := canonicalTranscriptEnabled
	SetCanonicalTranscriptEnabled(true)
	defer SetCanonicalTranscriptEnabled(prev)

	const (
		secret    = "abcdef0123456789abcdef0123456789"
		secretURL = "http://10.0.0.4:8888/lab?token=" + secret
		question  = "jupyter 打不开"
		answer    = "已确认，见上。"
		tailMark  = "TAIL_MARKER_MUST_NOT_SURVIVE"
	)

	cases := []struct {
		name           string
		turn           []openai.ChatCompletionMessage
		mustDropRounds bool
		// check runs against the assembled request (identical on both sides by
		// the time it is called) and the persisted metadata.
		check func(t *testing.T, assembled []openai.ChatCompletionMessage, metadata json.RawMessage)
	}{
		{
			name: "redaction fires in arguments, result and answer",
			turn: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: question},
				{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
					toolCall("c1", "DescribeCompShareInstance", `{"Note":"`+secretURL+`"}`),
				}},
				{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: `{"JupyterUrl":"` + secretURL + `"}`},
				{Role: openai.ChatMessageRoleAssistant, Content: "地址是 " + secretURL},
			},
			check: func(t *testing.T, assembled []openai.ChatCompletionMessage, metadata json.RawMessage) {
				replayed := renderReplayedRegion(t, assembled)
				assert.NotContains(t, string(metadata), secret, "the token must not be persisted")
				assert.NotContains(t, renderTestMessages(assembled), secret, "nor replayed into the next request")
				assert.Contains(t, replayed, "10.0.0.4:8888",
					"but the address and port are what the user needs; they must survive")
			},
		},
		{
			name: "per-message truncation fires",
			turn: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: question},
				{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
					toolCall("c1", "DescribeCompShareInstance", `{}`),
				}},
				{Role: openai.ChatMessageRoleTool, ToolCallID: "c1",
					Content: strings.Repeat("资", maxTranscriptMessageRunes) + tailMark},
				{Role: openai.ChatMessageRoleAssistant, Content: answer},
			},
			check: func(t *testing.T, assembled []openai.ChatCompletionMessage, _ json.RawMessage) {
				assert.Contains(t, renderReplayedRegion(t, assembled), "内容已截断",
					"a shortened body must say so, or the model reads a prefix as the whole result")
				assert.NotContains(t, renderTestMessages(assembled), tailMark,
					"and the cut-off tail must really be gone")
			},
		},
		{
			name:           "whole tool rounds are shed to fit the budget",
			mustDropRounds: true,
			turn: func() []openai.ChatCompletionMessage {
				out := []openai.ChatCompletionMessage{
					{Role: openai.ChatMessageRoleUser, Content: question},
				}
				// Each round is ~5000 runes; well past maxTranscriptTotalRunes.
				for _, id := range []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8", "c9", "c10"} {
					out = append(out,
						openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
							ToolCalls: []openai.ToolCall{toolCall(id, "DescribeCompShareInstance", `{"UHostId":"`+id+`"}`)}},
						openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id,
							Content: `{"id":"` + id + `","pad":"` + strings.Repeat("资", 5000) + `"}`},
					)
				}
				return append(out, openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleAssistant, Content: answer})
			}(),
			check: func(t *testing.T, assembled []openai.ChatCompletionMessage, _ json.RawMessage) {
				replayed := renderReplayedRegion(t, assembled)
				assertToolCallPairsValid(t, assembled)
				assert.Contains(t, replayed, answer,
					"shedding evidence must never shed the answer the user was given")
				// WHICH end is shed is the policy, and pairing validity alone
				// would be satisfied by shedding the newest instead. The closing
				// quote matters: `"id":"c1` is a prefix of `"id":"c10`.
				assert.NotContains(t, replayed, `"id":"c1"`,
					"the OLDEST round goes first")
				assert.Contains(t, replayed, `"id":"c10"`,
					"and the newest — the one the user is most likely referring to — stays")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hot, metadata, stats := runHotTurn(tc.turn)
			require.True(t, stats.Attempted, "precondition: this turn produces a transcript")
			require.NotNil(t, metadata, "precondition: it is persistable")
			if tc.mustDropRounds {
				require.Greater(t, stats.Dropped, 0,
					"precondition: this case exists to exercise round-shedding, and it did not fire")
			}

			cold := rebuildCold(question, tc.turn[len(tc.turn)-1].Content, metadata)

			hotAssembled := assembleNextTurn(hot, parityNextQuestion)
			coldAssembled := assembleNextTurn(cold, parityNextQuestion)

			require.Equal(t, coldAssembled, hotAssembled,
				"a session must behave the same after a restart as before it")

			// THE non-vacuity guard. If the transcript never attached, both
			// sides would fall back to the plain user/assistant pair, be
			// trivially equal, and this whole test would prove nothing about the
			// chain it claims to cover.
			requireTranscriptWasReplayed(t, hotAssembled)

			// Both sides are equal by here, so checking one checks both — but
			// equality alone would pass even if both were wrong.
			tc.check(t, hotAssembled, metadata)
			assertToolCallPairsValid(t, hotAssembled)
			assert.Contains(t, renderTestMessages(hotAssembled), parityNextQuestion,
				"the current question is always last and always present")
			assert.NotContains(t, renderReplayedRegion(t, hotAssembled), parityNextQuestion,
				"and it must appear exactly once — not also inside the replayed region")
		})
	}
}

// Replayed-exchange trimming is a different axis: it is about how many WHOLE
// exchanges survive the rune budget, not about what happens inside one. A budget
// that cut mid-exchange would leave the model reading an unanswered question, or
// worse, a tool result with no call.
func TestEndToEndHotColdParityWhenReplayBudgetTrims(t *testing.T) {
	prev := canonicalTranscriptEnabled
	SetCanonicalTranscriptEnabled(true)
	defer SetCanonicalTranscriptEnabled(prev)

	type exchange struct {
		question string
		answer   string
		metadata json.RawMessage
	}

	hot := &Engine{messages: []openai.ChatCompletionMessage{paritySystemMessage()}}
	var history []exchange

	// The fixture is SIZED OFF the budget rather than against a copy of it. Six
	// 3000-rune exchanges used to overflow because the budget was 12000; when it
	// was raised to 48000 this test kept passing every assertion about how a trim
	// behaves while no longer trimming at all, and only the "precondition" guard
	// below caught it. Deriving the count means the next change to either constant
	// re-sizes the fixture instead of silently disarming the test.
	//
	// padRunes stays under maxTranscriptMessageRunes so the producer does not
	// truncate the result and change what is being measured.
	const padRunes = 4000
	require.Less(t, padRunes, maxTranscriptMessageRunes, "premise: the pad must survive capture intact")
	exchangeCount := maxReplayedHistoryRunes/padRunes + 2
	require.LessOrEqual(t, exchangeCount, maxAgentContextPairs,
		"premise: the RUNE budget must be what trims here, not the pair count")

	tags := make([]string, 0, exchangeCount)
	for i := 1; i <= exchangeCount; i++ {
		tags = append(tags, fmt.Sprintf("e%d", i))
	}
	oldest, newest := tags[0], tags[len(tags)-1]

	for _, tag := range tags {
		q := tag + " 这台怎么了"
		a := tag + " 已确认。"
		hot.messages = append(hot.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: q},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{toolCall(tag, "DescribeCompShareInstance", `{"UHostId":"`+tag+`"}`)}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tag,
				Content: `{"id":"` + tag + `","pad":"` + strings.Repeat("资", padRunes) + `"}`},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: a},
		)
		hot.captureTurnTranscript()
		payload, stats := hot.LastTurnTranscript()
		require.True(t, stats.Attempted, "%s: precondition", tag)
		history = append(history, exchange{question: q, answer: a, metadata: payload})
	}

	cold := &Engine{}
	rows := make([]HistoryMessage, 0, len(history)*2)
	for _, ex := range history {
		rows = append(rows,
			HistoryMessage{Role: openai.ChatMessageRoleUser, Content: ex.question},
			HistoryMessage{Role: openai.ChatMessageRoleAssistant, Content: ex.answer, Transcript: ex.metadata},
		)
	}
	cold.RehydrateHistory(rows)

	hotAssembled := assembleNextTurn(hot, parityNextQuestion)
	coldAssembled := assembleNextTurn(cold, parityNextQuestion)

	require.Equal(t, coldAssembled, hotAssembled,
		"a session must behave the same after a restart as before it")

	rendered := renderReplayedRegion(t, hotAssembled)
	requireTranscriptWasReplayed(t, hotAssembled)
	assertToolCallPairsValid(t, hotAssembled)

	// The budget must have bitten — otherwise this test proves nothing.
	require.NotContains(t, rendered, oldest+" 这台怎么了",
		"precondition: %d exchanges of %d runes must exceed maxReplayedHistoryRunes=%d — "+
			"without a trim every assertion below passes vacuously",
		exchangeCount, padRunes, maxReplayedHistoryRunes)
	// And it must bite the correct end. Dropping the newest would satisfy every
	// other assertion here while destroying the context a follow-up depends on.
	assert.Contains(t, rendered, newest+" 这台怎么了",
		"the newest exchange is the one 「它」/「刚才那个」 refers to; it is never the one dropped")
	assert.Contains(t, rendered, `"id":"`+newest+`"`, "with its tool evidence intact")

	// Whatever survived, survived whole: question, answer and tool evidence
	// share one fate per exchange.
	for _, tag := range tags {
		question := strings.Contains(rendered, tag+" 这台怎么了")
		answer := strings.Contains(rendered, tag+" 已确认。")
		evidence := strings.Contains(rendered, `"id":"`+tag+`"`)
		assert.Equal(t, question, answer, "%s: half an exchange reads as an unanswered question", tag)
		assert.Equal(t, question, evidence, "%s: its tool evidence must share that fate", tag)
	}
	assert.Contains(t, renderTestMessages(hotAssembled), parityNextQuestion,
		"the current question is always retained")
	assert.NotContains(t, rendered, parityNextQuestion,
		"and it appears exactly once — not also inside the replayed region")
}
