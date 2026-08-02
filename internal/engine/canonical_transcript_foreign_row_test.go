package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Rows this binary did not write.
//
// Every other transcript test writes a row with the producer and reads it back
// with the consumer, so it can only ever exercise shapes the producer can make.
// That is the one class of input a self-written fixture structurally cannot
// cover, and it is the class that arrives in production: a row from a newer
// binary during a rolling deploy, a row from a rolled-back one, a row another
// writer put its own keys next to, a row corrupted in transit.
//
// The contract under test is narrow and absolute: whatever the row says, the
// assembled request must be legal. A half round — an assistant tool_call with no
// result, or a tool result with no call — is a 400 on the WHOLE request, so it
// does not degrade the turn, it destroys it.

// requireProviderLegal asserts the message list could be sent to a provider.
//
// These five rules are the ones a malformed transcript can actually violate; a
// stricter schema check would pass on shapes that still 400 and fail on shapes
// that are fine.
func requireProviderLegal(t *testing.T, msgs []openai.ChatCompletionMessage) {
	t.Helper()
	declared := map[string]int{}
	answered := map[string]int{}

	for i, msg := range msgs {
		switch msg.Role {
		case openai.ChatMessageRoleTool:
			require.NotEmpty(t, msg.ToolCallID, "message %d: tool result with no tool_call_id", i)
			require.Contains(t, declared, msg.ToolCallID,
				"message %d: tool result answers %q, which no PRECEDING assistant message declared", i, msg.ToolCallID)
			answered[msg.ToolCallID]++
		case openai.ChatMessageRoleAssistant:
			require.False(t, msg.Content == "" && len(msg.ToolCalls) == 0,
				"message %d: assistant message with neither content nor tool_calls", i)
			for _, call := range msg.ToolCalls {
				require.NotEmpty(t, call.ID, "message %d: tool_call with no id", i)
				require.NotContains(t, declared, call.ID,
					"message %d: tool_call id %q declared twice", i, call.ID)
				require.True(t, validToolArguments(call.Function.Arguments),
					"message %d: tool_call %q carries unparseable arguments %q", i, call.ID, call.Function.Arguments)
				declared[call.ID] = i
			}
		}
	}
	for id := range declared {
		require.Equal(t, 1, answered[id],
			"tool_call %q must be answered exactly once; %d results present", id, answered[id])
	}
}

// foreignRow is a metadata document as it would arrive from the database.
type foreignRow struct {
	name string
	json string
	// parses says whether this binary should recognise a transcript in it at all.
	parses bool
}

func foreignRows() []foreignRow {
	return []foreignRow{
		{
			name:   "forward schema version",
			json:   `{"agent_transcript_v1":{"v":2,"messages":[{"role":"assistant","content":"从更新的二进制来"}]}}`,
			parses: false,
		},
		{
			name:   "unknown sibling keys beside a readable transcript",
			json:   `{"some_other_writer":{"k":1},"agent_transcript_v1":{"v":1,"messages":[{"role":"user","content":"查一下"},{"role":"assistant","content":"好的"}]},"trailing":"x"}`,
			parses: true,
		},
		{
			name: "unknown fields inside the transcript and its messages",
			json: `{"agent_transcript_v1":{"v":1,"future_field":true,"messages":[` +
				`{"role":"assistant","content":"a","not_a_field":{"deep":[1,2]}}]}}`,
			parses: true,
		},
		{
			name: "the same tool_call id declared twice",
			json: `{"agent_transcript_v1":{"v":1,"messages":[` +
				`{"role":"user","content":"查一下"},` +
				`{"role":"assistant","tool_calls":[{"id":"dup","name":"DescribeCompShareInstance","arguments":"{}"},{"id":"dup","name":"DescribeCompShareInstance","arguments":"{}"}]},` +
				`{"role":"tool","tool_call_id":"dup","content":"ok"},` +
				`{"role":"assistant","content":"已确认。"}]}}`,
			parses: true,
		},
		{
			name: "a tool result ordered BEFORE the call it answers",
			json: `{"agent_transcript_v1":{"v":1,"messages":[` +
				`{"role":"user","content":"查一下"},` +
				`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
				`{"role":"assistant","tool_calls":[{"id":"c1","name":"DescribeCompShareInstance","arguments":"{}"}]},` +
				`{"role":"assistant","content":"已确认。"}]}}`,
			parses: true,
		},
		{
			name: "the same result present twice",
			json: `{"agent_transcript_v1":{"v":1,"messages":[` +
				`{"role":"user","content":"查一下"},` +
				`{"role":"assistant","tool_calls":[{"id":"c1","name":"DescribeCompShareInstance","arguments":"{}"}]},` +
				`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
				`{"role":"tool","tool_call_id":"c1","content":"ok again"},` +
				`{"role":"assistant","content":"已确认。"}]}}`,
			parses: true,
		},
		{
			name: "a tool result whose call is absent entirely",
			json: `{"agent_transcript_v1":{"v":1,"messages":[` +
				`{"role":"user","content":"查一下"},` +
				`{"role":"tool","tool_call_id":"ghost","content":"ok"},` +
				`{"role":"assistant","content":"已确认。"}]}}`,
			parses: true,
		},
		{
			name: "a tool result with an empty tool_call_id",
			json: `{"agent_transcript_v1":{"v":1,"messages":[` +
				`{"role":"user","content":"查一下"},` +
				`{"role":"tool","tool_call_id":"","content":"ok"},` +
				`{"role":"assistant","content":"已确认。"}]}}`,
			parses: true,
		},
		{
			name: "arguments that are not JSON",
			json: `{"agent_transcript_v1":{"v":1,"messages":[` +
				`{"role":"user","content":"查一下"},` +
				`{"role":"assistant","tool_calls":[{"id":"c1","name":"SearchKnowledge","arguments":"<query>价格</query>"}]},` +
				`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
				`{"role":"assistant","content":"已确认。"}]}}`,
			parses: true,
		},
		{
			name:   "an assistant message that is empty once its calls are dropped",
			json:   `{"agent_transcript_v1":{"v":1,"messages":[{"role":"assistant","tool_calls":[{"id":"orphan","name":"X","arguments":"{}"}]}]}}`,
			parses: true,
		},
		{
			name:   "a transcript that is not an object",
			json:   `{"agent_transcript_v1":[1,2,3]}`,
			parses: false,
		},
		{
			name:   "metadata that is not an object",
			json:   `"just a string"`,
			parses: false,
		},
		{
			name:   "truncated JSON",
			json:   `{"agent_transcript_v1":{"v":1,"messa`,
			parses: false,
		},
	}
}

func TestForeignRowsNeverProduceAnIllegalRequest(t *testing.T) {
	enableCanonicalTranscriptForTest(t)

	for _, row := range foreignRows() {
		t.Run(row.name, func(t *testing.T) {
			transcript := ParseTranscriptMetadata(json.RawMessage(row.json))
			require.Equal(t, row.parses, transcript != nil,
				"parse recognition changed for %s", row.name)

			// Projector in isolation.
			requireProviderLegal(t, ProjectTranscript(transcript))

			// And through the whole cold path, which is where a row like this
			// actually arrives: rehydrate, compile the context, assemble the
			// request the provider would receive.
			cold := &Engine{}
			cold.RehydrateHistory([]HistoryMessage{
				{Role: openai.ChatMessageRoleUser, Content: "查一下"},
				{Role: openai.ChatMessageRoleAssistant, Content: "已确认。", Transcript: json.RawMessage(row.json)},
			})
			assembled := assembleNextTurn(cold, "那现在呢")
			requireProviderLegal(t, assembled)
		})
	}
}

// The producer cannot emit these, so nothing else asserts the projector is what
// removes them rather than the fixture never containing them.
func TestForeignRowIllegalShapesAreActuallyPresentBeforeProjection(t *testing.T) {
	enableCanonicalTranscriptForTest(t)

	shapes := map[string]func(*TranscriptV1) bool{
		"a tool result ordered BEFORE the call it answers": func(tr *TranscriptV1) bool {
			seen := map[string]bool{}
			for _, m := range tr.Messages {
				for _, c := range m.ToolCalls {
					seen[c.ID] = true
				}
				if m.Role == openai.ChatMessageRoleTool && m.ToolCallID != "" && !seen[m.ToolCallID] {
					return true
				}
			}
			return false
		},
		"the same tool_call id declared twice": func(tr *TranscriptV1) bool {
			seen := map[string]int{}
			for _, m := range tr.Messages {
				for _, c := range m.ToolCalls {
					seen[c.ID]++
				}
			}
			for _, n := range seen {
				if n > 1 {
					return true
				}
			}
			return false
		},
		"a tool result whose call is absent entirely": func(tr *TranscriptV1) bool {
			declared := map[string]bool{}
			for _, m := range tr.Messages {
				for _, c := range m.ToolCalls {
					declared[c.ID] = true
				}
			}
			for _, m := range tr.Messages {
				if m.Role == openai.ChatMessageRoleTool && !declared[m.ToolCallID] {
					return true
				}
			}
			return false
		},
		"arguments that are not JSON": func(tr *TranscriptV1) bool {
			for _, m := range tr.Messages {
				for _, c := range m.ToolCalls {
					if !validToolArguments(c.Arguments) {
						return true
					}
				}
			}
			return false
		},
	}

	byName := map[string]foreignRow{}
	for _, row := range foreignRows() {
		byName[row.name] = row
	}

	for name, present := range shapes {
		row, ok := byName[name]
		require.True(t, ok, "fixture %q disappeared; the sweep above no longer covers it", name)
		transcript := ParseTranscriptMetadata(json.RawMessage(row.json))
		require.NotNil(t, transcript, "fixture %q must parse for this to mean anything", name)
		require.True(t, present(transcript),
			"fixture %q does not actually contain the shape it is named for — "+
				"TestForeignRowsNeverProduceAnIllegalRequest is passing on nothing", name)
	}
}

// A row from a newer binary must not cost the turn its history. Degrading to
// "no transcript" is correct; degrading to "no conversation" would be the
// amnesia this whole program exists to remove.
func TestForeignVersionRowStillReplaysItsExchange(t *testing.T) {
	enableCanonicalTranscriptForTest(t)

	cold := &Engine{}
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "第一台叫什么"},
		{Role: openai.ChatMessageRoleAssistant, Content: "叫 web-01。",
			Transcript: json.RawMessage(`{"agent_transcript_v1":{"v":99,"messages":[{"role":"assistant","content":"x"}]}}`)},
	})

	assembled := assembleNextTurn(cold, "那第二台呢")
	replayed := renderReplayedRegion(t, assembled)
	require.Contains(t, replayed, "叫 web-01。",
		"an unreadable transcript took the exchange down with it")
	requireProviderLegal(t, assembled)
}

// Bounding markers the producer writes must survive a round trip through a
// foreign reader too — a v1 row from another binary may carry them.
func TestForeignRowTruncationMarkerIsReplayedAsAnOmission(t *testing.T) {
	enableCanonicalTranscriptForTest(t)

	raw := fmt.Sprintf(`{"agent_transcript_v1":{"v":1,"messages":[`+
		`{"role":"user","content":"列一下"},`+
		`{"role":"assistant","tool_calls":[{"id":"c1","name":"DescribeCompShareInstance","arguments":"{}"}]},`+
		`{"role":"tool","tool_call_id":"c1","content":"前 20 台","truncated":true,"orig_runes":%d},`+
		`{"role":"assistant","content":"见上。"}]}}`, 91234)

	transcript := ParseTranscriptMetadata(json.RawMessage(raw))
	require.NotNil(t, transcript)
	projected := ProjectTranscript(transcript)
	requireProviderLegal(t, projected)

	require.Contains(t, renderTestMessages(projected), "91234",
		"a truncated result replayed as if it were complete: the model reads a prefix as the whole list")
}
