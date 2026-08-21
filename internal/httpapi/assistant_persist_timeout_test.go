package httpapi

import (
	"context"
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
