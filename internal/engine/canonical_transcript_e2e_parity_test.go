package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/prompt"
	"github.com/compshare-agent/internal/security"
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
// the sole semantic history:
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
	e.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(e, question, "turn-parity", fixedBuildAt)
	e.turnContextViewReady = true
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser, Content: question,
	})
	return e.buildMessagesForLLM(centralAgentToolWindow(false, false))
}

// runHotTurn holds one turn the way a live engine does and captures it.
func runHotTurn(turn []openai.ChatCompletionMessage) (*Engine, json.RawMessage, TranscriptStats) {
	e := &Engine{messages: append([]openai.ChatCompletionMessage{paritySystemMessage()}, turn...)}
	e.captureTurnTranscript()
	payload, stats := e.LastTurnTranscript()
	return e, payload, stats
}

// rebuildCold is a raw-row restart harness for transcript transforms. It uses
// exactly the endpoint strings passed by its caller; persistence-redaction
// parity belongs to TestHotAndColdReplayAcrossPersistenceRedactions below,
// which supplies the same role-specific form the HTTP handler stores.
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

// HTTP persists user and assistant rows through role-specific credential boundaries.
// Ordinary information remains intact; a cold reconstruction must still attach
// the canonical transcript when either display row changed at that boundary. Otherwise the
// session silently degrades from tool-backed history to a plain text pair after a
// restart even though the transcript itself is present and valid.
func TestHotAndColdReplayAcrossPersistenceRedactions(t *testing.T) {
	cases := []struct {
		name        string
		question    string
		answer      string
		changedRole string
	}{
		{
			name:     "user ordinary information",
			question: "请查 alice@example.com 和 13800138000 对应的实例",
			answer:   "已查到实例状态正常。",
		},
		{
			name:        "user operational token",
			question:    "请查 http://10.0.0.4:8888/lab?token=AKIAIOSFODNN7EXAMPLEbCDEF 对应的实例",
			answer:      "已查到实例状态正常。",
			changedRole: openai.ChatMessageRoleUser,
		},
		{
			name:     "assistant project UUID",
			question: "项目状态怎么样？",
			answer:   "关联项目 12345678-1234-1234-1234-1234567890ab 当前正常。",
		},
		{
			name:        "assistant credential placeholder",
			question:    "Jupyter 为什么无法登录？",
			answer:      "请使用 token=AKIAIOSFODNN7EXAMPLEbCDEF 重新登录。",
			changedRole: openai.ChatMessageRoleAssistant,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: tc.question},
				{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
					toolCall("c1", "DescribeCompShareInstance", `{}`),
				}},
				{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: `{"UHostId":"uhost-1","State":"Running"}`},
				{Role: openai.ChatMessageRoleAssistant, Content: tc.answer},
			}
			hot, metadata, stats := runHotTurn(turn)
			require.True(t, stats.Attempted, "precondition: the turn must have a canonical transcript")
			require.NotNil(t, metadata, "precondition: the transcript must persist")

			persistedQuestion := security.RedactUserConversationText(tc.question)
			persistedAnswer := security.RedactAssistantConversationText(tc.answer)
			switch tc.changedRole {
			case openai.ChatMessageRoleUser:
				require.NotEqual(t, tc.question, persistedQuestion, "precondition: the HTTP user persistence boundary changed this endpoint")
				require.Equal(t, tc.answer, persistedAnswer)
			case openai.ChatMessageRoleAssistant:
				require.NotEqual(t, tc.answer, persistedAnswer, "precondition: the HTTP assistant persistence boundary changed this endpoint")
				require.Equal(t, tc.question, persistedQuestion)
			default:
				require.Equal(t, tc.question, persistedQuestion, "ordinary user information must remain intact")
				require.Equal(t, tc.answer, persistedAnswer, "ordinary assistant information must remain intact")
			}

			cold := &Engine{}
			cold.RehydrateHistory([]HistoryMessage{
				{Role: openai.ChatMessageRoleUser, Content: persistedQuestion},
				{Role: openai.ChatMessageRoleAssistant, Content: persistedAnswer, Transcript: metadata},
			})

			hotAssembled := assembleNextTurn(hot, parityNextQuestion)
			coldAssembled := assembleNextTurn(cold, parityNextQuestion)
			require.Equal(t, hotAssembled, coldAssembled,
				"the persistence redaction boundary must not make a restart change model history")
			requireTranscriptWasReplayed(t, coldAssembled)
			replayed := renderReplayedRegion(t, hotAssembled)
			require.Contains(t, replayed, persistedQuestion)
			require.Contains(t, replayed, persistedAnswer)
			if tc.changedRole == openai.ChatMessageRoleUser {
				require.NotContains(t, replayed, tc.question)
			} else if tc.changedRole == openai.ChatMessageRoleAssistant {
				require.NotContains(t, replayed, tc.answer)
			}
			require.NotContains(t, replayed, "AKIAIOSFODNN7EXAMPLEbCDEF", "access credentials must not return through replay")
		})
	}
}

func TestHistoryUsesPersistenceAlignedEndpointText(t *testing.T) {
	const (
		question = "请查 alice@example.com 对应的实例"
		answer   = "关联项目 12345678-1234-1234-1234-1234567890ab 当前正常。"
	)
	eng := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: question},
		{Role: openai.ChatMessageRoleAssistant, Content: answer},
	}}

	on := eng.recentConversationPairs()
	require.Equal(t, []ConversationPair{{
		User: security.RedactUserConversationText(question), Assistant: security.RedactAssistantConversationText(answer),
	}}, on, "history uses the same endpoint forms that cold rehydration reads")
}

// Canonical-history compaction is a different axis: it keeps complete dialogue
// pairs and sheds only old tool detail. A budget must never create a half pair or
// a tool result whose declaring call is absent, and it must derive the same view
// before and after a restart.
func TestEndToEndHotColdParityWhenReplayBudgetCompactsDetail(t *testing.T) {
	type exchange struct {
		question string
		answer   string
		metadata json.RawMessage
	}

	hot := &Engine{messages: []openai.ChatCompletionMessage{paritySystemMessage()}}
	var history []exchange

	// Derive the fixture from the production budget so it always exercises
	// compaction when that budget changes.
	//
	// padRunes stays under maxTranscriptMessageRunes so the producer does not
	// truncate the result and change what is being measured.
	const padRunes = 4000
	require.Less(t, padRunes, maxTranscriptMessageRunes, "premise: the pad must survive capture intact")
	exchangeCount := maxReplayedHistoryRunes/padRunes + 2

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

	// Both source lists must still contain every exchange when replay compaction
	// starts, otherwise this test would measure an upstream retention limit.
	require.Len(t, hot.recentTurns, exchangeCount,
		"premise: budgetRecordedTurns must not be what drops an exchange here")

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

	// The budget must have bitten — otherwise this test proves nothing. The old
	// dialogue remains, while its re-queryable tool detail is compacted away.
	require.Contains(t, rendered, oldest+" 这台怎么了",
		"the earliest plain exchange must remain semantic history")
	require.NotContains(t, rendered, `"id":"`+oldest+`"`,
		"precondition: %d exchanges of %d-rune detail must exceed maxReplayedHistoryRunes=%d — "+
			"without a detail compaction every assertion below passes vacuously",
		exchangeCount, padRunes, maxReplayedHistoryRunes)
	// Newest tool evidence wins because it is most likely to resolve a follow-up.
	assert.Contains(t, rendered, newest+" 这台怎么了",
		"the newest exchange is the one 「它」/「刚才那个」 refers to; it is never the one dropped")
	assert.Contains(t, rendered, `"id":"`+newest+`"`, "with its tool evidence intact")

	// Every visible detail must still belong to a complete exchange. The reverse
	// does not hold by design: old exchanges may be represented by their plain
	// user/assistant pair after their tool detail is compacted.
	for _, tag := range tags {
		question := strings.Contains(rendered, tag+" 这台怎么了")
		answer := strings.Contains(rendered, tag+" 已确认。")
		evidence := strings.Contains(rendered, `"id":"`+tag+`"`)
		assert.Equal(t, question, answer, "%s: half an exchange reads as an unanswered question", tag)
		if evidence {
			assert.True(t, question, "%s: tool detail must never outlive its dialogue pair", tag)
		}
	}
	assert.Contains(t, renderTestMessages(hotAssembled), parityNextQuestion,
		"the current question is always retained")
	assert.NotContains(t, rendered, parityNextQuestion,
		"and it appears exactly once — not also inside the replayed region")
}
