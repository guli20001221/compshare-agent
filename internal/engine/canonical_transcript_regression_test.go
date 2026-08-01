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

// TestCapAssembledRequestMessages_NeverOrphansToolResultsInReplayedRegion is the
// blocking one: phase 1 shed a fixed two messages per "pair", so cutting into a
// four-message exchange left its tool result behind with no call declaring it.
// A provider rejects that whole request with a 400 — the turn fails outright, it
// does not degrade.
func TestCapAssembledRequestMessages_NeverOrphansToolResultsInReplayedRegion(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
	}
	for _, tag := range []string{"e1", "e2", "e3", "e4", "e5"} {
		msgs = append(msgs, transcriptExchange(tag)...)
	}
	msgs = append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current-question"})

	for _, limit := range []int{8, 10, 12, 14, 16, 18, 20} {
		out := capAssembledRequestMessages(msgs, limit)
		assertToolCallPairsValid(t, out)
		assert.LessOrEqual(t, len(out), limit, "cap=%d", limit)
		assert.Contains(t, renderTestMessages(out), "current-question", "cap=%d", limit)
	}
}

// TestCapAssembledRequestMessages_ShedsWholeExchangesNotMessagePrefixes pins the
// stronger property behind the fix: a replayed exchange is present in full or
// absent in full. A user turn kept without its answer is worse than dropping
// both, because the model reads it as an unanswered question.
func TestCapAssembledRequestMessages_ShedsWholeExchangesNotMessagePrefixes(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
	}
	for _, tag := range []string{"e1", "e2", "e3"} {
		msgs = append(msgs, transcriptExchange(tag)...)
	}
	msgs = append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "current-question"})

	rendered := renderTestMessages(capAssembledRequestMessages(msgs, 10))

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
	prev := canonicalTranscriptEnabled
	SetCanonicalTranscriptEnabled(true)
	defer SetCanonicalTranscriptEnabled(prev)

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
	prev := canonicalTranscriptEnabled
	SetCanonicalTranscriptEnabled(true)
	defer SetCanonicalTranscriptEnabled(prev)

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
