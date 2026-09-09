package agentpool

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/require"
)

const sampleTranscript = `{"agent_transcript_v1":{"v":1,"messages":[` +
	`{"role":"user","content":"q"},` +
	`{"role":"assistant","tool_calls":[{"id":"c1","name":"T","arguments":"{}"}]},` +
	`{"role":"tool","tool_call_id":"c1","name":"T","content":"r"},` +
	`{"role":"assistant","content":"a"}]}}`

type memoryRecentHistoryStore struct {
	store.MessageStore
	messages  []store.Message
	listCalls int
}

func (s *memoryRecentHistoryStore) ListRecentBySessionPage(_ context.Context, _ string, limit int, cursor string) ([]store.Message, string, error) {
	s.listCalls++
	end := len(s.messages)
	if cursor != "" {
		var err error
		end, err = strconv.Atoi(cursor)
		if err != nil {
			return nil, "", err
		}
	}
	if limit <= 0 {
		limit = 50
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	page := make([]store.Message, 0, end-start)
	for i := end - 1; i >= start; i-- {
		page = append(page, s.messages[i])
	}
	next := ""
	if start > 0 {
		next = strconv.Itoa(start)
	}
	return page, next, nil
}

func loadHistoryForTest(t *testing.T, rows []store.Message) ([]engine.HistoryMessage, *memoryRecentHistoryStore) {
	t.Helper()
	messages := &memoryRecentHistoryStore{messages: rows}
	history, err := loadRecentHistory(context.Background(), messages, "session", 1_000_000)
	require.NoError(t, err)
	return history, messages
}

// The cold rebuild flattens store.Message down to the engine's history type;
// this assertion prevents that boundary from dropping transcript metadata.
func TestLoadRecentHistoryCarriesAssistantTranscript(t *testing.T) {
	history, _ := loadHistoryForTest(t, []store.Message{
		{Role: "user", Status: "ok", Content: "q"},
		{Role: "assistant", Status: "ok", Content: "a", Metadata: []byte(sampleTranscript)},
	})
	if len(history) != 2 {
		t.Fatalf("kept %d messages, want 2", len(history))
	}
	if len(history[0].Transcript) != 0 {
		t.Fatal("user rows must not carry a transcript")
	}
	if got := engine.ParseTranscriptMetadata(history[1].Transcript); got == nil {
		t.Fatalf("assistant transcript did not survive: %q", history[1].Transcript)
	}
}

func TestLoadRecentHistoryBudgetChargesCanonicalTranscriptMetadata(t *testing.T) {
	rows := []store.Message{
		{Role: "user", Status: "ok", Content: "older question"},
		{Role: "assistant", Status: "ok", Content: "older answer", Metadata: []byte(sampleTranscript)},
		{Role: "user", Status: "ok", Content: "newer question"},
		{Role: "assistant", Status: "ok", Content: "newer answer", Metadata: []byte(sampleTranscript)},
	}
	newerUser, ok := historyMessage(rows[2])
	require.True(t, ok)
	newerAssistant, ok := historyMessage(rows[3])
	require.True(t, ok)
	budget := historyMessageSourceRunes(newerUser) + historyMessageSourceRunes(newerAssistant)

	messages := &memoryRecentHistoryStore{messages: rows}
	history, err := loadRecentHistory(context.Background(), messages, "session", budget)

	require.NoError(t, err)
	require.Equal(t, []engine.HistoryMessage{newerUser, newerAssistant}, history)
	// Content-only accounting would keep both tiny exchanges because it would
	// never charge either assistant's canonical transcript.
	require.Greater(t, len(sampleTranscript), len(newerUser.Content)+len(newerAssistant.Content))
}

func TestLoadRecentHistoryTinyRowsCannotCauseUnboundedPaging(t *testing.T) {
	rows := make([]store.Message, 0, 4000)
	for i := 0; i < 2000; i++ {
		rows = append(rows,
			store.Message{Role: "user", Status: "ok", Content: "u"},
			store.Message{Role: "assistant", Status: "ok", Content: "a"},
		)
	}
	messages := &memoryRecentHistoryStore{messages: rows}

	history, err := loadRecentHistory(context.Background(), messages, "session", 1000)

	require.NoError(t, err)
	require.LessOrEqual(t, messages.listCalls, 2,
		"the serialized row structure must consume budget even when content is one rune")
	require.Less(t, len(history), len(rows))
}

// Rows written before transcript persistence existed have NULL metadata. They must
// rebuild exactly as they always did.
func TestRowsWithoutTranscriptRebuildUnchanged(t *testing.T) {
	history, _ := loadHistoryForTest(t, []store.Message{
		{Role: "user", Status: "ok", Content: "q"},
		{Role: "assistant", Status: "ok", Content: "a"},
	})
	for _, msg := range history {
		if len(msg.Transcript) != 0 {
			t.Fatalf("invented a transcript for a legacy row: %+v", msg)
		}
		if engine.ParseTranscriptMetadata(msg.Transcript) != nil {
			t.Fatal("legacy row parsed to a non-nil transcript")
		}
	}
}

// Production case 006: the first request contained only the target instance ID, then the outer
// assistant row was aborted. A cold pool rebuild must retain that successful user row even though
// it has no successful assistant pair; otherwise the next vague complaint loses its target.
func TestLoadRecentHistoryKeepsUnpairedUserAcrossAnAbortedAssistant(t *testing.T) {
	history, _ := loadHistoryForTest(t, []store.Message{
		{Role: "user", Status: "ok", Content: "uhost-1uha5i7jetgm"},
		{Role: "assistant", Status: "aborted", Content: ""},
		{Role: "user", Status: "ok", Content: "都开始收费还是进不去"},
	})
	if len(history) != 3 {
		t.Fatalf("kept %d messages, want both user endpoints and the interrupted boundary", len(history))
	}
	if history[0].Role != "user" || history[0].Content != "uhost-1uha5i7jetgm" {
		t.Fatalf("lost the unpaired target user row: %+v", history)
	}
	if history[1].Role != "assistant" || history[1].Content != "" {
		t.Fatalf("interrupted placeholder became an answer: %+v", history)
	}
	if history[2].Role != "user" || history[2].Content != "都开始收费还是进不去" {
		t.Fatalf("lost the continuation user row: %+v", history)
	}
}

func TestLoadRecentHistoryCarriesInterruptedTranscriptButNotDisplayPlaceholder(t *testing.T) {
	for _, status := range []string{"aborted", "error"} {
		history, _ := loadHistoryForTest(t, []store.Message{
			{Role: "user", Status: "ok", Content: "q"},
			{Role: "assistant", Status: status, Content: "本次回复已中止，未完整生成。", Metadata: []byte(sampleTranscript)},
		})
		if len(history) != 2 || history[1].Content != "" || string(history[1].Transcript) != sampleTranscript {
			t.Fatalf("%s did not carry evidence separately from display text: %+v", status, history)
		}
	}
}

func TestLoadRecentHistoryStatusGatingAndInterruptedTurns(t *testing.T) {
	rows := []store.Message{
		{Role: "user", Content: "检查旧实例", Status: "ok"},
		{Role: "assistant", Content: "已检查", Status: "ok"},
		{Role: "user", Content: "改为检查新实例", Status: "ok"},
		{Role: "assistant", Content: "partial reply must not become completed history", Status: "aborted"},
		{Role: "user", Content: "继续", Status: "ok"},
		{Role: "assistant", Content: "failed reply", Status: "error"},
		{Role: "user", Content: "继续", Status: "ok"},
		{Role: "assistant", Content: "pending reply", Status: "pending"},
		{Role: "user", Content: "pending user", Status: "pending"},
		{Role: "system", Content: "system ok", Status: "ok"},
		{Role: "tool", Content: "tool ok", Status: "ok"},
	}
	history, _ := loadHistoryForTest(t, rows)
	eng := &engine.Engine{}
	eng.RehydrateHistory(history)
	view := (engine.ContextCompiler{}).Compile(eng, "继续", time.Now())
	require.Equal(t, []engine.ConversationPair{
		{User: "检查旧实例", Assistant: "已检查"},
		{User: "改为检查新实例"},
		{User: "继续"},
		{User: "继续"},
	}, view.RecentConversation)
}

func TestLoadRecentHistoryKeepsAssistantBoundaryAcrossReversePages(t *testing.T) {
	for _, status := range []string{"pending", "aborted", "error"} {
		t.Run(status, func(t *testing.T) {
			rows := []store.Message{
				{Role: "user", Status: "ok", Content: "boundary user"},
				{Role: "assistant", Status: status, Content: "display placeholder", Metadata: []byte(sampleTranscript)},
			}
			// There are exactly 127 newer rows, so the target assistant is the
			// final row of page one and its user is the first row of page two.
			for i := 0; i < 63; i++ {
				marker := strconv.Itoa(i)
				rows = append(rows,
					store.Message{Role: "user", Status: "ok", Content: "filler user " + marker},
					store.Message{Role: "assistant", Status: "ok", Content: "filler assistant " + marker},
				)
			}
			rows = append(rows, store.Message{Role: "user", Status: "ok", Content: "newest unanswered user"})

			history, messages := loadHistoryForTest(t, rows)
			require.Equal(t, 2, messages.listCalls)
			require.NotEmpty(t, history)
			require.Equal(t, engine.HistoryMessage{Role: "user", Content: "boundary user"}, history[0])
			if status == "pending" {
				require.Equal(t, "user", history[1].Role, "a pending assistant must remain excluded across a page boundary")
				return
			}
			require.Equal(t, "assistant", history[1].Role)
			require.Empty(t, history[1].Content, "an interrupted display placeholder must not become an answer")
			require.Equal(t, sampleTranscript, string(history[1].Transcript))
		})
	}
}
