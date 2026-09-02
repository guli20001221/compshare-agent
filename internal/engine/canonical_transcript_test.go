package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	openai "github.com/sashabaranov/go-openai"
)

func userMsg(content string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content}
}

func assistantCalls(calls ...openai.ToolCall) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: calls}
}

// call mirrors what a provider actually returns, Type included. Leaving Type
// empty made the parity fixture unlike a real turn and hid a round-trip
// difference behind a fixture bug.
func call(id, name, args string) openai.ToolCall {
	return openai.ToolCall{
		ID:       id,
		Type:     openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: name, Arguments: args},
	}
}

func toolMsg(callID, content string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: callID, Content: content}
}

func finalMsg(content string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content}
}

// The turn boundary must be the same one the model assembler uses. If these
// ever diverge, the persisted transcript stops being a record of what the model
// saw, which is the entire point of storing it.
func TestCurrentTurnStartIsSharedWithModelAssembler(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		userMsg("first turn"),
		finalMsg("first answer"),
		userMsg("second turn"),
		assistantCalls(call("c1", "DescribeCompShareInstance", `{}`)),
		toolMsg("c1", "result"),
		finalMsg("second answer"),
	}
	start := currentTurnStart(messages)
	if got := messages[start].Content; got != "second turn" {
		t.Fatalf("turn start = %q, want the last user message", got)
	}

	// The assembler must slice from exactly this index.
	assembled := messagesFromAgentContext(messages, AgentContext{}, true)
	tail := assembled[len(assembled)-4:]
	if tail[0].Content != "second turn" || tail[1].ToolCalls[0].ID != "c1" ||
		tail[2].ToolCallID != "c1" || tail[3].Content != "second answer" {
		t.Fatalf("assembler tail does not match the shared turn boundary: %+v", tail)
	}
}

// A plain Q&A turn is already fully represented by the user and assistant rows.
// Storing it again would be duplication, so it must produce nothing.
func TestBuildTranscriptSkipsTurnsWithoutToolTraffic(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		userMsg("多少钱"),
		finalMsg("答案"),
	}
	if got := buildTranscriptV1(messages); got != nil {
		t.Fatalf("expected no transcript for a tool-free turn, got %+v", got)
	}
}

func TestBuildTranscriptPreservesOrderPairingAndToolNames(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		userMsg("4090 掉卡了"),
		// Round 1: two tool calls in ONE assistant message.
		assistantCalls(
			call("c1", "SearchKnowledge", `{"query":"掉卡"}`),
			call("c2", "DescribeCompShareInstance", `{"id":"x"}`),
		),
		toolMsg("c1", "kb result"),
		toolMsg("c2", "instance result"),
		// Round 2.
		assistantCalls(call("c3", "DiagnoseInstanceInternals", `{}`)),
		toolMsg("c3", "diag result"),
		finalMsg("结论"),
	}
	transcript := buildTranscriptV1(messages)
	if transcript == nil {
		t.Fatal("expected a transcript")
	}
	if transcript.V != transcriptSchemaVersion {
		t.Fatalf("schema version = %d, want %d", transcript.V, transcriptSchemaVersion)
	}
	if len(transcript.Messages) != len(messages) {
		t.Fatalf("kept %d messages, want all %d", len(transcript.Messages), len(messages))
	}
	if transcript.DroppedRounds != 0 {
		t.Fatalf("dropped %d rounds unexpectedly", transcript.DroppedRounds)
	}

	// Multiple calls in one assistant message keep their order.
	first := transcript.Messages[1]
	if len(first.ToolCalls) != 2 || first.ToolCalls[0].ID != "c1" || first.ToolCalls[1].ID != "c2" {
		t.Fatalf("tool_calls order/count wrong: %+v", first.ToolCalls)
	}
	if first.ToolCalls[0].Arguments != `{"query":"掉卡"}` {
		t.Fatalf("arguments not preserved: %q", first.ToolCalls[0].Arguments)
	}

	// A tool message carries no name on the wire; the builder must correlate it.
	for _, tc := range []struct {
		idx              int
		wantID, wantName string
	}{
		{2, "c1", "SearchKnowledge"},
		{3, "c2", "DescribeCompShareInstance"},
		{5, "c3", "DiagnoseInstanceInternals"},
	} {
		got := transcript.Messages[tc.idx]
		if got.ToolCallID != tc.wantID || got.Name != tc.wantName {
			t.Fatalf("message %d: got (%s,%s), want (%s,%s)",
				tc.idx, got.ToolCallID, got.Name, tc.wantID, tc.wantName)
		}
	}
}

// Only the CURRENT turn belongs in the record. Earlier turns are already
// persisted on their own rows.
func TestBuildTranscriptExcludesEarlierTurns(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		userMsg("old turn"),
		assistantCalls(call("old", "OldTool", `{}`)),
		toolMsg("old", "old result"),
		finalMsg("old answer"),
		userMsg("new turn"),
		assistantCalls(call("new", "NewTool", `{}`)),
		toolMsg("new", "new result"),
		finalMsg("new answer"),
	}
	transcript := buildTranscriptV1(messages)
	raw, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "old") {
		t.Fatalf("transcript leaked a previous turn: %s", raw)
	}
}

// This is a synthetic size-boundary fixture, not a recorded search result. A
// SearchKnowledge observation can combine three full bodies with six snippets
// from query fan-out; its envelope costs more than a three-item ReadChunk result.
func TestCanonicalTranscriptPreservesExpandedSearchEnvelope(t *testing.T) {
	hits := make([]knowledge.RetrievalHit, 0, 9)
	fullBodies := make([]string, 0, 3)
	expandedIDs := make([]string, 0, 3)
	for i := 0; i < 9; i++ {
		suffix := string(rune('a' + i))
		body := strings.Repeat("摘", knowledge.DefaultEvidenceSnippetMaxRunes)
		if i < 3 {
			// Quotes and backslashes consume extra JSON runes even when each
			// source body remains within the corpus's 4000-rune bound.
			body = strings.Repeat("文", 3495) + strings.Repeat("\"\\", 250) + "END_" + suffix
			if got := len([]rune(body)); got != knowledge.MaxKnowledgeContentRunes {
				t.Fatalf("body %d has %d runes, want %d", i, got, knowledge.MaxKnowledgeContentRunes)
			}
			fullBodies = append(fullBodies, body)
			expandedIDs = append(expandedIDs, "v2-synthetic-evidence-"+suffix)
		}
		hits = append(hits, knowledge.RetrievalHit{
			Kept: true, Score: 0.95,
			Chunk: knowledge.KBChunk{
				ChunkID: "v2-synthetic-evidence-" + suffix,
				Title:   strings.Repeat("标题", 39) + suffix, SourceType: "faq", Content: body,
			},
		})
	}
	ledger := knowledge.BuildSubstantiveEvidenceLedger("读取完整规则", hits, len(hits), 0)
	for i, body := range fullBodies {
		ledger.Items[i].Snippet = body
	}
	observation := agentToolObservation("SearchKnowledge", searchKnowledgeResultJSON(ledger, "", map[string]any{
		"auto_expanded_chunk_ids": expandedIDs,
	}))
	if size := len([]rune(observation)); size <= 16000 || size > maxTranscriptMessageRunes {
		t.Fatalf("fixture must exceed the former 16000-rune cap and fit the current cap: got %d", size)
	}
	transcript := buildTranscriptV1([]openai.ChatCompletionMessage{
		userMsg("读取完整规则"),
		assistantCalls(call("search", "SearchKnowledge", `{"query":"读取完整规则"}`)),
		toolMsg("search", observation),
		finalMsg("已读取。"),
	})
	raw, err := marshalTranscriptMetadata(transcript)
	if err != nil {
		t.Fatal(err)
	}
	restored := ParseTranscriptMetadata(raw)
	if restored == nil || len(restored.Messages) != 4 {
		t.Fatalf("expected the complete search turn after persistence: %+v", restored)
	}
	if restored.DroppedRounds != 0 || transcriptRunes(restored.Messages) > maxTranscriptTotalRunes || len(raw) > maxTranscriptBytes {
		t.Fatal("the fixture must fit the unchanged aggregate and byte budgets")
	}
	if result := restored.Messages[2]; result.Truncated || result.OrigRunes != 0 {
		t.Fatalf("expanded search envelope was truncated: %+v", result)
	}
	projected := ProjectTranscript(restored)
	if len(projected) != 4 || projected[2].Content != observation || !json.Valid([]byte(projected[2].Content)) {
		t.Fatal("the full SearchKnowledge envelope must survive persistence and replay unchanged")
	}
	for _, tail := range []string{"END_a", "END_b", "END_c"} {
		if !strings.Contains(projected[2].Content, tail) {
			t.Fatalf("replayed observation lost body tail %q", tail)
		}
	}
}

func TestBoundContentMarksTruncationWithOriginalLength(t *testing.T) {
	long := strings.Repeat("字", maxTranscriptMessageRunes+500)
	messages := []openai.ChatCompletionMessage{
		userMsg("q"),
		assistantCalls(call("c1", "T", `{}`)),
		toolMsg("c1", long),
		finalMsg("a"),
	}
	transcript := buildTranscriptV1(messages)
	result := transcript.Messages[2]
	if !result.Truncated {
		t.Fatal("oversized tool result must be marked truncated")
	}
	if result.OrigRunes != maxTranscriptMessageRunes+500 {
		t.Fatalf("orig_runes = %d, want %d", result.OrigRunes, maxTranscriptMessageRunes+500)
	}
	if got := len([]rune(result.Content)); got != maxTranscriptMessageRunes {
		t.Fatalf("truncated content = %d runes, want %d", got, maxTranscriptMessageRunes)
	}
	// A short message must stay verbatim and unmarked — a reader has to be able
	// to tell a genuinely short result from a shortened one.
	if transcript.Messages[3].Truncated || transcript.Messages[3].OrigRunes != 0 {
		t.Fatal("short message must not be marked truncated")
	}
	raw, err := marshalTranscriptMetadata(transcript)
	if err != nil {
		t.Fatal(err)
	}
	restored := ParseTranscriptMetadata(raw)
	if restored == nil || !restored.Messages[2].Truncated || restored.Messages[2].OrigRunes != len([]rune(long)) {
		t.Fatal("persistence must retain the oversized result's truncation metadata")
	}
	projected := ProjectTranscript(restored)
	if got, want := projected[2].Content, result.Content+truncationNotice(len([]rune(long))); got != want {
		t.Fatal("replay must explicitly disclose the shortened result, not present its prefix as complete")
	}
}

// The shedding unit is a whole round. An assistant tool_call without its result,
// or a result whose call is gone, is not a replayable message list.
func TestSheddingDropsWholeRoundsAndKeepsPairing(t *testing.T) {
	big := strings.Repeat("x", maxTranscriptMessageRunes)
	messages := []openai.ChatCompletionMessage{userMsg("q")}
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		messages = append(messages,
			assistantCalls(call(id, "Tool"+id, `{}`)),
			toolMsg(id, big),
		)
	}
	messages = append(messages, finalMsg("final answer"))

	transcript := buildTranscriptV1(messages)
	if transcript.DroppedRounds == 0 {
		t.Fatal("expected rounds to be shed for an oversized turn")
	}
	if got := transcriptRunes(transcript.Messages); got > maxTranscriptTotalRunes {
		t.Fatalf("still over budget after shedding: %d runes", got)
	}

	// Every surviving tool result must still have its assistant tool_call, and
	// every surviving tool_call must still have its result.
	declared := map[string]bool{}
	for _, msg := range transcript.Messages {
		for _, c := range msg.ToolCalls {
			declared[c.ID] = false
		}
	}
	for _, msg := range transcript.Messages {
		if msg.Role != openai.ChatMessageRoleTool {
			continue
		}
		seen, ok := declared[msg.ToolCallID]
		if !ok {
			t.Fatalf("orphan tool result %q: its tool_call was shed", msg.ToolCallID)
		}
		if seen {
			t.Fatalf("duplicate tool result for %q", msg.ToolCallID)
		}
		declared[msg.ToolCallID] = true
	}
	for id, answered := range declared {
		if !answered {
			t.Fatalf("tool_call %q survived without its result", id)
		}
	}

	// The question and the final answer are never shed.
	if transcript.Messages[0].Content != "q" {
		t.Fatalf("user message was shed: %+v", transcript.Messages[0])
	}
	last := transcript.Messages[len(transcript.Messages)-1]
	if last.Content != "final answer" {
		t.Fatalf("final assistant message was shed: %+v", last)
	}
}

func TestMarshalTranscriptMetadataShape(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		userMsg("q"),
		assistantCalls(call("c1", "T", `{}`)),
		toolMsg("c1", "r"),
		finalMsg("a"),
	}
	raw, err := marshalTranscriptMetadata(buildTranscriptV1(messages))
	if err != nil {
		t.Fatal(err)
	}
	// The envelope must be a wrapper object so future consumers can add sibling
	// keys without a schema migration.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("metadata is not a JSON object: %v", err)
	}
	if _, ok := envelope["agent_transcript_v1"]; !ok {
		t.Fatalf("missing agent_transcript_v1 key: %s", raw)
	}

	// Nothing to store must be nil, not an empty document, so the writer can
	// skip the round trip entirely.
	empty, err := marshalTranscriptMetadata(nil)
	if err != nil || empty != nil {
		t.Fatalf("nil transcript = (%v, %v), want (nil, nil)", empty, err)
	}
}

func TestCaptureTurnTranscriptReportsStats(t *testing.T) {
	e := &Engine{messages: []openai.ChatCompletionMessage{
		userMsg("q"),
		assistantCalls(call("c1", "T", `{}`)),
		toolMsg("c1", "r"),
		finalMsg("a"),
	}}
	e.captureTurnTranscript()
	raw, stats := e.LastTurnTranscript()
	if raw == nil {
		t.Fatal("expected a captured transcript")
	}
	if !stats.Attempted || stats.Bytes != len(raw) || stats.Messages != 4 {
		t.Fatalf("stats = %+v, want attempted with 4 messages and %d bytes", stats, len(raw))
	}

	// A tool-free turn must report "nothing to store", NOT a failure — a rollout
	// has to tell those apart.
	e.messages = []openai.ChatCompletionMessage{userMsg("q"), finalMsg("a")}
	e.captureTurnTranscript()
	raw, stats = e.LastTurnTranscript()
	if raw != nil || stats.Attempted || stats.Invalid || stats.Oversized {
		t.Fatalf("tool-free turn = (%v, %+v), want no payload and no attempt", raw, stats)
	}
}
