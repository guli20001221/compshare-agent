package httpapi

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/store"
)

// deadlineRecordingStore captures the context handed to UpdateAssistant.
type deadlineRecordingStore struct {
	noopMessageStore
	hadLimit bool
	deadline time.Time
	calls    int
}

func (s *deadlineRecordingStore) UpdateAssistant(ctx context.Context, _ store.Owner, _ string, _ store.AssistantPatch) error {
	s.calls++
	s.deadline, s.hadLimit = ctx.Deadline()
	return nil
}

// TestPersistAssistant_IsBounded pins the deadline itself. Without it a stalled
// database withholds `done` (success path) or the error frame (error path) for
// as long as it stalls — the client is told nothing, and the session's engine
// lease is held the whole time.
func TestPersistAssistant_IsBounded(t *testing.T) {
	st := &deadlineRecordingStore{}
	h := &Handlers{messages: st}

	if err := h.persistAssistant(store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "msg-1",
		store.AssistantPatch{Status: "ok"}); err != nil {
		t.Fatalf("persistAssistant returned %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("store called %d times, want 1", st.calls)
	}
	if !st.hadLimit {
		t.Fatal("assistant-row write was handed a context with no deadline")
	}
	if remaining := time.Until(st.deadline); remaining <= 0 || remaining > assistantPersistTimeout {
		t.Fatalf("deadline %v out of range, want (0, %v]", remaining, assistantPersistTimeout)
	}
}

// TestNoUnboundedAssistantWritesRemain is the structural half. The three turn-path
// writes are easy to reintroduce unbounded — the old form reads perfectly
// natural — so this fails on any UpdateAssistant call that passes a bare
// Background context, rather than trusting the three current sites to stay
// converted.
func TestNoUnboundedAssistantWritesRemain(t *testing.T) {
	src, err := os.ReadFile("handlers_chat.go")
	if err != nil {
		t.Fatalf("read handlers_chat.go: %v", err)
	}
	unbounded := regexp.MustCompile(`UpdateAssistant\(\s*context\.Background\(\)`)
	if loc := unbounded.FindIndex(src); loc != nil {
		line := 1 + strings.Count(string(src[:loc[0]]), "\n")
		t.Fatalf("handlers_chat.go:%d writes the assistant row on an unbounded context; "+
			"route it through persistAssistant so a stalled database cannot withhold done/error", line)
	}
}
