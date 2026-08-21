package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The canonical transcript changes the shape of a restored exchange. Before it,
// every exchange in the replayed region was exactly {user, assistant}; with it,
// an exchange can be {user, assistant(tool_calls), tool, ..., assistant}. These
// tests pin the places that assumed the old fixed shape, plus the two boundaries
// the transcript path bypassed.

// transcriptExchange builds one restored exchange whose assistant answer was
// produced through a tool round — the shape the cap arithmetic did not expect.
func transcriptExchange(tag string) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: tag + "-u"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall(tag+"-call", "DescribeCompShareInstance", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: tag + "-call", Content: `{"RetCode":0,"tag":"` + tag + `"}`},
		{Role: openai.ChatMessageRoleAssistant, Content: tag + "-a"},
	}
}

func renderTranscript(transcript *TranscriptV1) string {
	raw, _ := json.Marshal(transcript)
	return string(raw)
}

// TestTrimAssembledRequest_NeverOrphansToolResultsInReplayedRegion is the
// blocking one: phase 1 shed a fixed two messages per "pair", so cutting into a
// four-message exchange left its tool result behind with no call declaring it.
// A provider rejects that whole request with a 400 — the turn fails outright, it
// does not degrade.
//
// It used to sweep MESSAGE limits, which is the dimension trimAssembledRequest no
// longer has. Sweeping budgets that admit exactly 0..5 whole exchanges puts the
// cut in the same places — including, deliberately, between them.
func TestTrimAssembledRequest_NeverOrphansToolResultsInReplayedRegion(t *testing.T) {
	const tags = 5
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
	}
	for _, tag := range []string{"e1", "e2", "e3", "e4", "e5"} {
		msgs = append(msgs, transcriptExchange(tag)...)
	}
	msgs = append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current-question"})

	head := assembledRequestRunes([]openai.ChatCompletionMessage{msgs[0], msgs[1], msgs[len(msgs)-1]})
	perExchange := assembledRequestRunes(transcriptExchange("e1"))
	for keep := 0; keep <= tags; keep++ {
		// Half an exchange over: the budget cannot be met by a clean boundary, so
		// the trim has to round DOWN to one rather than cut into an exchange.
		for _, budget := range []int{head + keep*perExchange, head + keep*perExchange + perExchange/2} {
			out := trimAssembledRequest(msgs, budget)
			assertToolCallPairsValid(t, out)
			assert.LessOrEqual(t, assembledRequestRunes(out), budget, "budget=%d", budget)
			assert.Contains(t, renderTestMessages(out), "current-question", "budget=%d", budget)
		}
	}
}

// TestTrimAssembledRequest_ShedsWholeExchangesNotMessagePrefixes pins the
// stronger property behind the fix: a replayed exchange is present in full or
// absent in full. A user turn kept without its answer is worse than dropping
// both, because the model reads it as an unanswered question.
func TestTrimAssembledRequest_ShedsWholeExchangesNotMessagePrefixes(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
	}
	for _, tag := range []string{"e1", "e2", "e3"} {
		msgs = append(msgs, transcriptExchange(tag)...)
	}
	msgs = append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current-question"})

	// Room for one exchange and a fraction of another — the shape that tempts a
	// trim into taking the fraction.
	head := assembledRequestRunes([]openai.ChatCompletionMessage{msgs[0], msgs[1], msgs[len(msgs)-1]})
	perExchange := assembledRequestRunes(transcriptExchange("e1"))
	rendered := renderTestMessages(trimAssembledRequest(msgs, head+perExchange+perExchange/2))

	for _, tag := range []string{"e1", "e2", "e3"} {
		question := strings.Contains(rendered, tag+"-u")
		answer := strings.Contains(rendered, tag+"-a")
		evidence := strings.Contains(rendered, `"tag":"`+tag+`"`)
		assert.Equal(t, question, answer, "%s: question and answer must share a fate", tag)
		assert.Equal(t, question, evidence, "%s: the tool evidence must share that fate too", tag)
	}
}

// TestAttachRecordedTranscripts_DoesNotReuseOneRecordForRepeatedExchanges pins
// that matching consumes. Two turns can carry identical text — "再试一次" against
// two different instances is ordinary — and first-match-wins attached the first
// turn's tool evidence to both. The hot/cold parity test cannot see this because
// both sides make the same mistake.
func TestAttachRecordedTranscripts_DoesNotReuseOneRecordForRepeatedExchanges(t *testing.T) {

	record := func(tag string) recordedTurn {
		return recordedTurn{
			User:      "再试一次",
			Assistant: "已确认。",
			Transcript: &TranscriptV1{V: transcriptSchemaVersion, Messages: []TranscriptMessage{
				{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{ID: tag, Name: "DescribeCompShareInstance", Arguments: `{"UHostId":"` + tag + `"}`}}},
				{Role: openai.ChatMessageRoleTool, ToolCallID: tag, Name: "DescribeCompShareInstance", Content: `{"RetCode":0,"UHostId":"` + tag + `"}`},
			}},
		}
	}
	eng := &Engine{recentTurns: []recordedTurn{record("uhost-aaa"), record("uhost-bbb")}}

	pairs := eng.attachRecordedTranscripts([]ConversationPair{
		{User: "再试一次", Assistant: "已确认。"},
		{User: "再试一次", Assistant: "已确认。"},
	})

	require.Len(t, pairs, 2)
	first := renderTestMessages(pairs[0].Transcript)
	second := renderTestMessages(pairs[1].Transcript)
	require.NotEmpty(t, first)
	require.NotEmpty(t, second)
	assert.NotEqual(t, first, second,
		"each replayed exchange must carry its own tool evidence; reusing one record tells the model a diagnosis ran against an instance it never ran against")
	assert.Contains(t, first, "uhost-aaa")
	assert.Contains(t, second, "uhost-bbb")
}

// TestBuildTranscriptV1_RedactsOperationalTokens pins that the transcript
// carrier does not route around the cross-turn redaction boundary. Ordinary
// replayed history goes through safeConversationText; the transcript stored the
// same text raw, so a Jupyter token redacted out of the assistant line survived
// verbatim in metadata and came back on the next turn.
func TestBuildTranscriptV1_RedactsOperationalTokens(t *testing.T) {
	const secret = "abcdef0123456789abcdef0123456789"
	const secretURL = "http://10.0.0.4:8888/lab?token=" + secret
	turn := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "jupyter 地址是多少"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
			toolCall("c1", "DescribeCompShareInstance", `{"Note":"`+secretURL+`"}`),
		}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: `{"JupyterUrl":"` + secretURL + `"}`},
		{Role: openai.ChatMessageRoleAssistant, Content: "地址是 " + secretURL},
	}

	transcript := buildTranscriptV1(turn)
	require.NotNil(t, transcript)

	assert.NotContains(t, renderTranscript(transcript), secret,
		"the stored transcript must clear the same redaction boundary as replayed history")
	assert.NotContains(t, renderTestMessages(ProjectTranscript(transcript)), secret,
		"and so must anything projected back into model context")
}

// TestProjectTranscript_SurfacesTruncation pins that a shortened body is
// labelled. Storing Truncated and then projecting only Content hands the model a
// prefix of a list and no way to know it is a prefix — it reads a partial
// instance list as the complete one.
func TestProjectTranscript_SurfacesTruncation(t *testing.T) {
	long := strings.Repeat("资", maxTranscriptMessageRunes+500)
	turn := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "列一下实例"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("c1", "DescribeCompShareInstance", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: long},
	}

	transcript := buildTranscriptV1(turn)
	require.NotNil(t, transcript)

	var truncated bool
	for _, msg := range transcript.Messages {
		if msg.Role == openai.ChatMessageRoleTool && msg.Truncated {
			truncated = true
		}
	}
	require.True(t, truncated, "precondition: this body was shortened")

	var projectedTool string
	for _, msg := range ProjectTranscript(transcript) {
		if msg.Role == openai.ChatMessageRoleTool {
			projectedTool = msg.Content
		}
	}
	require.NotEmpty(t, projectedTool)
	assert.Contains(t, projectedTool, "截断",
		"the model must be told the body was shortened, not handed a silent prefix")
}

// TestBuildTranscriptV1_DoesNotSilentlyTruncateToolArguments pins the other half
// of the truncation defect: the producer discarded the flag for arguments
// outright, so an over-long argument string was cut with nothing recording it
// anywhere — not in storage, not in projection.
func TestBuildTranscriptV1_DoesNotSilentlyTruncateToolArguments(t *testing.T) {
	longArgs := `{"Note":"` + strings.Repeat("资", maxTranscriptMessageRunes+500) + `"}`
	turn := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "列一下实例"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("c1", "DescribeCompShareInstance", longArgs)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: `{"RetCode":0}`},
	}

	transcript := buildTranscriptV1(turn)
	require.NotNil(t, transcript)

	for _, msg := range transcript.Messages {
		for _, call := range msg.ToolCalls {
			if len([]rune(call.Arguments)) < len([]rune(longArgs)) {
				assert.True(t, json.Valid([]byte(call.Arguments)),
					"a shortened argument string must stay parseable, or the model reads a broken call it believes it made")
			}
		}
	}
}

// TestAttachRecordedTranscripts_ToolFreeRecordStillConsumesItsSlot is the same
// misattribution as above by a second route. captureTurnTranscript records every
// exchange, including one that called no tools, as a record with a nil
// Transcript. Skipping those before comparing text let a later same-text record
// slide forward into the earlier turn's slot — so turn 1's answer was shown
// turn 2's tool evidence, and turn 2 was shown none. The sibling test cannot
// catch this: both of its records carry a transcript.
func TestAttachRecordedTranscripts_ToolFreeRecordStillConsumesItsSlot(t *testing.T) {

	eng := &Engine{recentTurns: []recordedTurn{
		{User: "再试一次", Assistant: "已确认。", Transcript: nil}, // a turn that called no tools
		{User: "再试一次", Assistant: "已确认。", Transcript: &TranscriptV1{V: transcriptSchemaVersion, Messages: []TranscriptMessage{
			{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{{ID: "c1", Name: "DescribeCompShareInstance", Arguments: `{"UHostId":"uhost-bbb"}`}}},
			{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Name: "DescribeCompShareInstance", Content: `{"RetCode":0,"UHostId":"uhost-bbb"}`},
		}}},
	}}

	pairs := eng.attachRecordedTranscripts([]ConversationPair{
		{User: "再试一次", Assistant: "已确认。"},
		{User: "再试一次", Assistant: "已确认。"},
	})

	require.Len(t, pairs, 2)
	assert.Empty(t, pairs[0].Transcript,
		"the first exchange called no tools; showing it the second turn's evidence attributes a lookup to the wrong turn")
	assert.Contains(t, renderTestMessages(pairs[1].Transcript), "uhost-bbb",
		"and the turn that did call the tool must keep its own evidence")
}

// TestProjectTranscript_DropsRoundsWithUnparseableArguments is a provider-validity
// guard for the commonest malformed call, not a hypothetical one. executeToolOnce
// records that roughly 4% of SearchKnowledge calls arrive with a leaked tag or a
// bare query string where a JSON object belongs. Those rounds sit in e.messages
// like any other and, replayed verbatim, hand the model back a call it could not
// have meant — and tell it the malformed form was accepted.
func TestProjectTranscript_DropsRoundsWithUnparseableArguments(t *testing.T) {
	transcript := &TranscriptV1{V: transcriptSchemaVersion, Messages: []TranscriptMessage{
		{Role: openai.ChatMessageRoleUser, Content: "查一下"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{
			{ID: "bad", Name: "SearchKnowledge", Arguments: `<search>显存不够</search>`},
		}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "bad", Name: "SearchKnowledge",
			Content: "parameter parse error: invalid character '<'"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []TranscriptToolCall{
			{ID: "good", Name: "DescribeCompShareInstance", Arguments: `{"UHostId":"uhost-ok"}`},
		}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "good", Name: "DescribeCompShareInstance",
			Content: `{"RetCode":0}`},
		{Role: openai.ChatMessageRoleAssistant, Content: "已确认。"},
	}}

	projected := ProjectTranscript(transcript)
	rendered := renderTestMessages(projected)

	assert.NotContains(t, rendered, "显存不够",
		"a tool_call whose arguments do not parse must not be replayed")
	assert.NotContains(t, rendered, "parameter parse error",
		"and its tool result must leave with it, or the result is orphaned")
	var survivingIDs []string
	for _, msg := range projected {
		for _, call := range msg.ToolCalls {
			survivingIDs = append(survivingIDs, call.ID)
			assert.True(t, json.Valid([]byte(call.Function.Arguments)),
				"every projected tool_call must carry parseable arguments, got %q", call.Function.Arguments)
		}
	}
	assert.Equal(t, []string{"good"}, survivingIDs,
		"the well-formed round in the same turn survives; only the malformed one leaves")
	assertProjectedPairsValid(t, projected)
}

// assertProjectedPairsValid restates the pairing invariant on projector output:
// every tool result answers a declared call, and every declared call is answered.
func assertProjectedPairsValid(t *testing.T, msgs []openai.ChatCompletionMessage) {
	t.Helper()
	declared := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			declared[c.ID] = true
		}
	}
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == openai.ChatMessageRoleTool {
			require.True(t, declared[m.ToolCallID], "orphaned tool result %q", m.ToolCallID)
			answered[m.ToolCallID] = true
		}
	}
	for id := range declared {
		require.True(t, answered[id], "tool_call %q was declared but never answered", id)
	}
}

// TestCaptureTurnTranscript_RejectsOversizedRawTurnWithoutScanningIt pins the
// latency guard. The storage limits bound the OUTPUT; without this the input is
// unbounded, and redaction plus []rune conversion run over the whole body before
// a single character is discarded — on the path the user is waiting on.
func TestCaptureTurnTranscript_RejectsOversizedRawTurnWithoutScanningIt(t *testing.T) {
	huge := strings.Repeat("x", maxRawTurnBytes+1)
	e := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "列一下"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("c1", "DescribeCompShareInstance", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: huge},
		{Role: openai.ChatMessageRoleAssistant, Content: "结果太大，已省略。"},
	}}

	e.captureTurnTranscript()
	payload, stats := e.LastTurnTranscript()

	assert.Nil(t, payload, "an oversized turn must not be persisted")
	assert.True(t, stats.Attempted, "it is still an attempt — a rollout must see it")
	assert.True(t, stats.Oversized, "and it must be attributable to size, not to a generic failure")

	require.Len(t, e.recentTurns, 1, "the exchange itself is still recorded")
	assert.Nil(t, e.recentTurns[0].Transcript, "but with no transcript attached")
}

// A turn just under the raw limit is unaffected — the guard must not be a
// tightening of the ordinary path.
func TestCaptureTurnTranscript_KeepsTurnsUnderTheRawLimit(t *testing.T) {
	body := strings.Repeat("y", 4096)
	e := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "列一下"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("c1", "DescribeCompShareInstance", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c1", Content: body},
		{Role: openai.ChatMessageRoleAssistant, Content: "已列出。"},
	}}

	e.captureTurnTranscript()
	payload, stats := e.LastTurnTranscript()

	require.NotNil(t, payload, "an ordinary turn must still be persisted")
	assert.False(t, stats.Oversized)
	assert.True(t, stats.Attempted)
}
