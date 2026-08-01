package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// recordingMetaStore implements both MessageStore and the optional
// AssistantMetadataStore.
type recordingMetaStore struct {
	noopMessageStore
	calls  int
	got    json.RawMessage
	gotID  string
	gotOwn store.Owner
	err    error
}

func (s *recordingMetaStore) UpdateAssistantMetadata(_ context.Context, owner store.Owner, msgID string, metadata json.RawMessage) error {
	s.calls++
	s.got = metadata
	s.gotID = msgID
	s.gotOwn = owner
	return s.err
}

// noopMessageStore satisfies store.MessageStore without implementing the
// optional metadata interface, so it doubles as the "store that predates the
// shadow write" case.
type noopMessageStore struct{}

func (noopMessageStore) Append(context.Context, store.Message) error { return nil }
func (noopMessageStore) UpdateAssistant(context.Context, store.Owner, string, store.AssistantPatch) error {
	return nil
}
func (noopMessageStore) ListBySession(context.Context, string, int, string) ([]store.Message, string, error) {
	return nil, "", nil
}
func (noopMessageStore) GetWithOwnerCheck(context.Context, store.Owner, string) (store.Message, error) {
	return store.Message{}, nil
}

func resetShadowStats() {
	transcriptShadowCounters.Attempted.Store(0)
	transcriptShadowCounters.Succeeded.Store(0)
	transcriptShadowCounters.NoStore.Store(0)
	transcriptShadowCounters.NoRowMatch.Store(0)
	transcriptShadowCounters.Oversized.Store(0)
	transcriptShadowCounters.Invalid.Store(0)
	transcriptShadowCounters.WriteError.Store(0)
}

// fakeTranscriptSource stands in for *engine.Engine's producer side.
type fakeTranscriptSource struct {
	payload json.RawMessage
	stats   engine.TranscriptStats
}

func (f fakeTranscriptSource) LastTurnTranscript() (json.RawMessage, engine.TranscriptStats) {
	return f.payload, f.stats
}

// engineWithToolTurn returns a source shaped like a turn that made tool calls.
func engineWithToolTurn(t *testing.T) turnTranscriptSource {
	t.Helper()
	payload := json.RawMessage(`{"agent_transcript_v1":{"v":1,"messages":[` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","name":"T","arguments":"{}"}]},` +
		`{"role":"tool","tool_call_id":"c1","name":"T","content":"r"},` +
		`{"role":"assistant","content":"a"}]}}`)
	return fakeTranscriptSource{
		payload: payload,
		stats:   engine.TranscriptStats{Attempted: true, Bytes: len(payload), Messages: 4},
	}
}

// engineWithPlainTurn returns a source shaped like a turn with no tool traffic.
func engineWithPlainTurn(t *testing.T) turnTranscriptSource {
	t.Helper()
	return fakeTranscriptSource{}
}

// The load-bearing invariant: a store with no metadata support must not panic,
// must not error, and must be counted distinctly. A silent no-op that reports
// success would make a broken rollout look healthy.
func TestShadowPersistDegradesWhenStoreLacksMetadataSupport(t *testing.T) {
	resetShadowStats()
	h := &Handlers{messages: noopMessageStore{}}
	agent := engineWithToolTurn(t)

	h.shadowPersistTranscript(store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "msg-1", agent, nil)

	got := TranscriptShadowSnapshot()
	if got.Attempted != 1 || got.NoStore != 1 || got.Succeeded != 0 {
		t.Fatalf("stats = %+v, want one attempt counted as NoStore", got)
	}
}

// Every write failure shape must be swallowed and counted, never propagated.
func TestShadowPersistSwallowsEveryWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want func(TranscriptShadowStats) bool
	}{
		{"no row matched", sql.ErrNoRows, func(s TranscriptShadowStats) bool { return s.NoRowMatch == 1 && s.Succeeded == 0 }},
		{"driver error", errors.New("jsonb: invalid input"), func(s TranscriptShadowStats) bool { return s.WriteError == 1 && s.Succeeded == 0 }},
		{"success", nil, func(s TranscriptShadowStats) bool { return s.Succeeded == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetShadowStats()
			st := &recordingMetaStore{err: tc.err}
			h := &Handlers{messages: st}

			// The call must not panic and returns nothing to propagate.
			h.shadowPersistTranscript(store.Owner{TopOrganizationID: 7, OrganizationID: 8}, "msg-9", engineWithToolTurn(t), nil)

			if st.calls != 1 {
				t.Fatalf("store called %d times, want 1", st.calls)
			}
			got := TranscriptShadowSnapshot()
			if got.Attempted != 1 || !tc.want(got) {
				t.Fatalf("stats = %+v", got)
			}
		})
	}
}

// The shadow write must never stamp metadata onto a row whose reply failed to
// persist — that row is not a valid turn record.
func TestShadowPersistSkipsWhenReplyDidNotPersist(t *testing.T) {
	resetShadowStats()
	st := &recordingMetaStore{}
	h := &Handlers{messages: st}

	h.shadowPersistTranscript(store.Owner{}, "msg-1", engineWithToolTurn(t), errors.New("reply update failed"))

	if st.calls != 0 {
		t.Fatalf("wrote metadata for a turn whose reply never landed (%d calls)", st.calls)
	}
	if got := TranscriptShadowSnapshot(); got.Attempted != 0 {
		t.Fatalf("stats = %+v, want no attempt", got)
	}
}

// A turn with no tool traffic is not a failure and must not be counted as an
// attempt — otherwise the success rate is diluted by turns that had nothing to
// write, and a real regression hides inside it.
func TestShadowPersistDoesNotCountToolFreeTurns(t *testing.T) {
	resetShadowStats()
	st := &recordingMetaStore{}
	h := &Handlers{messages: st}

	h.shadowPersistTranscript(store.Owner{}, "msg-1", engineWithPlainTurn(t), nil)

	if st.calls != 0 {
		t.Fatalf("wrote metadata for a tool-free turn (%d calls)", st.calls)
	}
	if got := TranscriptShadowSnapshot(); got.Attempted != 0 || got.Succeeded != 0 {
		t.Fatalf("stats = %+v, want a silent skip", got)
	}
}

func TestShadowPersistForwardsOwnerAndPayload(t *testing.T) {
	resetShadowStats()
	st := &recordingMetaStore{}
	h := &Handlers{messages: st}
	owner := store.Owner{TopOrganizationID: 66391350, OrganizationID: 64404856}

	h.shadowPersistTranscript(owner, "assistant-msg", engineWithToolTurn(t), nil)

	if st.gotOwn != owner || st.gotID != "assistant-msg" {
		t.Fatalf("owner/id = (%+v,%s), want (%+v,assistant-msg)", st.gotOwn, st.gotID, owner)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(st.got, &envelope); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if _, ok := envelope["agent_transcript_v1"]; !ok {
		t.Fatalf("payload missing agent_transcript_v1: %s", st.got)
	}
}

// TestShadowCounters_SurviveConcurrentTurns guards the counter type. The HTTP
// handler is concurrent, so plain int64 fields incremented with ++ lose updates
// under load — and they lose them silently, in exactly the number a rollout is
// reading to decide whether the shadow write works. This asserts the total is
// exact, which a lost update breaks. (It is a behavioural check: `go test -race`
// cannot build on the CI/dev toolchain here, so the structural guarantee is
// atomic.Int64 having no ++ at all, and this is the regression net.)
func TestShadowCounters_SurviveConcurrentTurns(t *testing.T) {
	resetShadowStats()
	const goroutines, perGoroutine = 64, 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				transcriptShadowCounters.Attempted.Add(1)
				transcriptShadowCounters.Succeeded.Add(1)
			}
		}()
	}
	wg.Wait()

	got := TranscriptShadowSnapshot()
	want := int64(goroutines * perGoroutine)
	if got.Attempted != want || got.Succeeded != want {
		t.Fatalf("stats = %+v, want Attempted=Succeeded=%d; a lost increment here is a misreported rollout number", got, want)
	}
}
