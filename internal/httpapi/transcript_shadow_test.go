package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// statelessMetaStore accepts every write and remembers nothing, so it is safe
// to share across goroutines. Used by the concurrency test, which asserts on
// the log and the counters rather than on what was written.
type statelessMetaStore struct{ noopMessageStore }

func (statelessMetaStore) UpdateAssistantMetadata(context.Context, store.Owner, string, json.RawMessage) error {
	return nil
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

// blockingMetaStore records the context it was handed and blocks until released,
// so a test can observe the deadline the caller imposed.
type blockingMetaStore struct {
	noopMessageStore
	entered  chan struct{}
	release  chan struct{}
	deadline time.Time
	hadLimit bool
}

func (s *blockingMetaStore) UpdateAssistantMetadata(ctx context.Context, _ store.Owner, _ string, _ json.RawMessage) error {
	s.deadline, s.hadLimit = ctx.Deadline()
	close(s.entered)
	<-s.release
	return ctx.Err()
}

// TestShadowPersist_BoundsTheWriteWithADeadline pins the half of the isolation
// fix that "the error is swallowed" never provided. Swallowing covers failure;
// it does nothing about a store that simply does not return. Without a deadline
// the turn holds its session's engine lease for as long as the database stalls.
func TestShadowPersist_BoundsTheWriteWithADeadline(t *testing.T) {
	resetShadowStats()
	st := &blockingMetaStore{entered: make(chan struct{}), release: make(chan struct{})}
	h := &Handlers{messages: st}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.shadowPersistTranscript(store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "msg-1", engineWithToolTurn(t), nil)
	}()

	select {
	case <-st.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("shadow write never reached the store")
	}
	close(st.release)
	<-done

	if !st.hadLimit {
		t.Fatal("the shadow write was handed a context with no deadline; a stalled store would hold the turn open indefinitely")
	}
	if remaining := time.Until(st.deadline); remaining <= 0 || remaining > shadowPersistTimeout {
		t.Fatalf("deadline %v out of range, want (0, %v]", remaining, shadowPersistTimeout)
	}
}

// TestShadowPersist_RunsAfterDoneAndAfterSessionState pins the ORDER, which is
// the property that actually protects the client. It reads the source rather
// than the runtime because the alternative — driving chatStream with a blocking
// store — would deadlock on the very ordering it is trying to assert.
func TestShadowPersist_RunsAfterDoneAndAfterSessionState(t *testing.T) {
	src, err := os.ReadFile("handlers_chat.go")
	if err != nil {
		t.Fatalf("read handlers_chat.go: %v", err)
	}
	body := string(src)

	doneAt := strings.Index(body, `sw.WriteEvent("done"`)
	stateAt := strings.Index(body, "if prep.sessionStatePersistable {")
	shadowAt := strings.Index(body, "h.shadowPersistTranscript(")
	if doneAt < 0 || stateAt < 0 || shadowAt < 0 {
		t.Fatalf("anchors moved: done=%d state=%d shadow=%d", doneAt, stateAt, shadowAt)
	}
	if shadowAt < doneAt {
		t.Fatal("shadow write precedes the done frame: a blocked database withholds the client's reply")
	}
	if shadowAt < stateAt {
		t.Fatal("shadow write precedes SessionState persistence: an optional migration probe must not delay the write the next turn depends on")
	}
}

// TestShadowMilestone_LogsExactlyOncePerHundred closes the evidence gap on the
// ticket change: the implementation is only correct if the milestone fires once
// per shadowReportEvery attempts and not once per concurrent completion. The
// counter-based version could log twice for one milestone or skip one entirely,
// and neither shows up in a counter assertion — only in the log.
func TestShadowMilestone_LogsExactlyOncePerHundred(t *testing.T) {
	resetShadowStats()

	var buf bytes.Buffer
	var mu sync.Mutex
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&lockedWriter{w: &buf, mu: &mu})
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	// A STATELESS store on purpose. recordingMetaStore keeps the last payload,
	// id and owner for the sequential tests; sharing it across 250 goroutines
	// would be a race in the test double itself — and one that says nothing
	// about the code under test, since this test reads only the log and the
	// atomic counters. Locking the recorder would hide that mismatch rather
	// than remove it.
	h := &Handlers{messages: statelessMetaStore{}}
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	// Built once, outside the goroutines: the source is read-only per call, and
	// constructing it 250 times would only add allocation to the measurement.
	source := engineWithToolTurn(t)

	// Drive 250 attempts concurrently: milestones fall at 100 and 200.
	const attempts = 250
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			h.shadowPersistTranscript(owner, "msg", source, nil)
		}()
	}
	wg.Wait()

	mu.Lock()
	got := strings.Count(buf.String(), "transcript shadow: attempted=")
	mu.Unlock()

	if want := attempts / shadowReportEvery; got != want {
		t.Fatalf("milestone logged %d times over %d attempts, want exactly %d "+
			"(re-reading the global counter instead of the ticket both duplicates and skips milestones)", got, attempts, want)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
