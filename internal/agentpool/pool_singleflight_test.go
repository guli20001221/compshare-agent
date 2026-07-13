package agentpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// One session, one engine — including in the window where the session has no
// entry yet.
//
// PR #445 pinned entries so a session with a turn on it could not be evicted.
// That is necessary and it is not sufficient: a pin protects an ENTRY, and the
// worst race lives in the window where the entry DOES NOT EXIST. Two concurrent
// first-requests for a cold session both missed, both read the message history,
// and both built an engine. The loser threw its copy away only if the winner was
// still in the map when it looked — so if the winner had run its turn and then
// been evicted for capacity (legally: it was idle by then), the loser found an
// empty slot and installed the engine IT had built, from a snapshot taken BEFORE
// that turn. The turn that had just completed vanished from the session's
// memory. Reproduced 5/5 before this change.
//
// This file is an INTERNAL test (package agentpool, not agentpool_test) for one
// reason: the queued-lease pin cannot be gated any other way. See
// TestPool_AQueuedLeasePinsItsEntry.
// ---------------------------------------------------------------------------

var sfOwner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}

// gatedStore lets a test hold every cold load open until it says otherwise, which
// is what makes the cold-load race deterministic instead of a coin flip.
type gatedStore struct {
	mu      sync.Mutex
	calls   map[string]int
	entered chan string   // one send per load that reaches the store
	open    chan struct{} // closed to let the loads proceed
	err     error         // when set, every load fails with it
}

func newGatedStore() *gatedStore {
	return &gatedStore{
		calls:   map[string]int{},
		entered: make(chan string, 64),
		open:    make(chan struct{}),
	}
}

func (g *gatedStore) Append(context.Context, store.Message) error { return nil }
func (g *gatedStore) UpdateAssistant(context.Context, store.Owner, string, store.AssistantPatch) error {
	return nil
}
func (g *gatedStore) GetWithOwnerCheck(context.Context, store.Owner, string) (store.Message, error) {
	return store.Message{}, nil
}

func (g *gatedStore) ListBySession(ctx context.Context, sessionID string, _ int, _ string) ([]store.Message, string, error) {
	g.mu.Lock()
	g.calls[sessionID]++
	err := g.err
	g.mu.Unlock()

	g.entered <- sessionID

	select {
	case <-g.open:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, "", err
	}
	return nil, "", nil
}

func (g *gatedStore) release()               { close(g.open) }
func (g *gatedStore) setErr(err error)       { g.mu.Lock(); g.err = err; g.mu.Unlock() }
func (g *gatedStore) loads(sid string) int   { g.mu.Lock(); defer g.mu.Unlock(); return g.calls[sid] }
func (g *gatedStore) awaitLoad(t *testing.T) { //nolint:thelper
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no load ever reached the store")
	}
}

func sfPool(t *testing.T, ms store.MessageStore, capacity int) *Pool {
	t.Helper()
	deps := &engine.SharedDeps{
		LLMClient:        stubLLM{},
		RateLimiter:      governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
		ExternalExecutor: tools.ToolExecutor(stubExecutor{}),
	}
	p := NewWithDeps(deps, ms, Options{Capacity: capacity, IdleTTL: time.Hour})
	t.Cleanup(p.Close)
	return p
}

type stubLLM struct{}

func (stubLLM) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "ok"}, nil
}

type stubExecutor struct{}

func (stubExecutor) Execute(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

var _ tools.ToolExecutor = stubExecutor{}

// inUseOf reads the entry's pin count. Test-only, and the reason this file is an
// internal test.
func inUseOf(p *Pool, sessionID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	el, ok := p.items[entryKey{Owner: sfOwner, SessionID: sessionID}]
	if !ok {
		return -1
	}
	return el.Value.(*entry).inUse
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// ---------------------------------------------------------------------------

// GATE 1 — the cold-load race. Concurrent first-requests for one session must
// produce exactly ONE load and ONE engine.
//
// Before single-flight every concurrent miss ran its own buildEngine, and the
// losers only deferred to the winner if the winner was still in the map. That is
// the whole bug: engines built from a pre-turn snapshot were still alive,
// waiting for a chance to be installed.
//
// Mutation: delete the placeholder insert (build first, insert after) and the
// load count goes to N.
func TestPool_ColdLoadRace_ASessionIsLoadedExactlyOnce(t *testing.T) {
	const sid = "sess-cold"
	const callers = 8

	gs := newGatedStore()
	p := sfPool(t, gs, 200)

	type got struct {
		eng *engine.Engine
		err error
	}
	results := make(chan got, callers)
	for i := 0; i < callers; i++ {
		go func() {
			eng, release, err := p.Lease(context.Background(), sfOwner, sid)
			if err == nil {
				release()
			}
			results <- got{eng, err}
		}()
	}

	gs.awaitLoad(t) // one load has reached the store; the rest must be queued behind it
	gs.release()

	var engines []*engine.Engine
	for i := 0; i < callers; i++ {
		select {
		case r := <-results:
			require.NoError(t, r.err)
			// Not merely "they all agree" — they must all agree on a REAL engine. Handing
			// every caller the same nil would satisfy an identity check while delivering
			// nothing, and a placeholder whose load never populated it does exactly that.
			require.NotNil(t, r.eng, "a caller was handed a nil engine")
			engines = append(engines, r.eng)
		case <-time.After(5 * time.Second):
			t.Fatal("a caller never returned from Lease")
		}
	}

	assert.Equal(t, 1, gs.loads(sid),
		"%d concurrent cold requests for one session must trigger exactly ONE load — "+
			"every extra load is another engine built from its own snapshot of the history", callers)
	for i, e := range engines {
		assert.Same(t, engines[0], e,
			"caller %d got a different engine; one session must have exactly one engine", i)
	}
}

// GATE 2 — a caller that waits on someone else's load must get THAT load's
// engine, and must never fall back to building its own.
//
// This is the shape of the original data loss: the waiter's own engine was built
// from a snapshot of the history taken BEFORE the winner's turn ran. Whether it
// ever reached the pool depended on whether the winner had been evicted by the
// time the waiter looked — i.e. on the LRU, not on anything the user did.
//
// Mutation: have awaitLoaded fall through to a build of its own and this fails.
func TestPool_AWaiterTakesTheLoadersEngine_NotAStaleCopyOfItsOwn(t *testing.T) {
	const sid = "sess-waiter"

	gs := newGatedStore()
	p := sfPool(t, gs, 200)

	first := make(chan *engine.Engine, 1)
	go func() {
		eng, release, err := p.Lease(context.Background(), sfOwner, sid)
		require.NoError(t, err)
		release()
		first <- eng
	}()
	gs.awaitLoad(t)

	// The second caller arrives while the load is still in flight. It must find the
	// placeholder and wait, not miss and start loading.
	second := make(chan *engine.Engine, 1)
	go func() {
		eng, release, err := p.Lease(context.Background(), sfOwner, sid)
		require.NoError(t, err)
		release()
		second <- eng
	}()
	eventually(t, "the second caller to be queued on the placeholder", func() bool {
		return inUseOf(p, sid) >= 2
	})
	assert.Equal(t, 1, gs.loads(sid),
		"the second caller must NOT have started a load of its own — that engine would carry a "+
			"history snapshot taken before the first caller's turn")

	gs.release()

	e1 := <-first
	e2 := <-second
	assert.Same(t, e1, e2, "the waiter must receive the loader's engine")
	assert.Equal(t, 1, gs.loads(sid), "still exactly one load after both callers finished")
}

// GATE 3 — the WAIT is cancellable. A client that hangs up while queued behind a
// cold load must stop waiting immediately, not hold the request open for the
// load's full timeout.
//
// Mutation: drop the ctx.Done() case from awaitLoaded and this hangs until the
// test's 2s deadline.
func TestPool_WaitingOnAColdLoadIsCancellable(t *testing.T) {
	const sid = "sess-cancel"

	gs := newGatedStore()
	p := sfPool(t, gs, 200)

	go func() {
		_, release, err := p.Lease(context.Background(), sfOwner, sid)
		if err == nil {
			release()
		}
	}()
	gs.awaitLoad(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := p.Lease(ctx, sfOwner, sid)
		done <- err
	}()
	eventually(t, "the second caller to be queued", func() bool { return inUseOf(p, sid) >= 2 })

	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled,
			"a caller whose client hung up must be released from the queue, with the reason")
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled caller stayed stuck waiting on a load it no longer cares about")
	}

	gs.release()
	eventually(t, "the abandoned wait to give its pin back", func() bool { return inUseOf(p, sid) <= 1 })
}

// GATE 4 — the caller who happened to TRIGGER the load may leave; the load
// belongs to the pool, not to them.
//
// Single-flight makes the load shared work. If it stayed tied to the lifetime of
// whichever request triggered it, one user pressing Stop would fail every other
// request queued behind them on that session — a NEW failure introduced by the
// fix, on exactly the axis the fix exists to protect. Aborts are common enough
// in real traffic to hit this.
//
// Mutation: build the load context from the caller's ctx instead of
// context.WithoutCancel, and the surviving waiter fails with context.Canceled.
func TestPool_TheLoaderHangingUpDoesNotFailTheCallersStillWaiting(t *testing.T) {
	const sid = "sess-loader-leaves"

	gs := newGatedStore()
	p := sfPool(t, gs, 200)

	loaderCtx, cancelLoader := context.WithCancel(context.Background())
	loaderDone := make(chan error, 1)
	go func() {
		_, release, err := p.Lease(loaderCtx, sfOwner, sid)
		if err == nil {
			release()
		}
		loaderDone <- err
	}()
	gs.awaitLoad(t)

	waiter := make(chan error, 1)
	go func() {
		eng, release, err := p.Lease(context.Background(), sfOwner, sid)
		if err == nil {
			assert.NotNil(t, eng)
			release()
		}
		waiter <- err
	}()
	eventually(t, "the waiter to be queued", func() bool { return inUseOf(p, sid) >= 3 })

	cancelLoader()
	require.ErrorIs(t, <-loaderDone, context.Canceled,
		"the caller who hung up must be released, like any other cancelled caller")

	gs.release()

	select {
	case err := <-waiter:
		assert.NoError(t, err,
			"the waiter is still here and still wants an answer — another user pressing Stop "+
				"must not fail their turn")
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never completed after the loader hung up")
	}
	assert.Equal(t, 1, gs.loads(sid), "and it still cost exactly one load")
}

// GATE 5 — a failed load must not be cached. A transient store error must not
// brick the session; the next request re-loads from scratch.
//
// Mutation: leave the placeholder in the map on error and the retry hangs (it
// waits forever on a `ready` that is already closed with an error, or inherits
// the poisoned entry) instead of succeeding.
func TestPool_AFailedLoadIsNotCached_TheSessionRetriesCleanly(t *testing.T) {
	const sid = "sess-load-fail"
	boom := errors.New("store is down")

	gs := newGatedStore()
	gs.setErr(boom)
	gs.release() // no gating for this one; fail immediately
	p := sfPool(t, gs, 200)

	_, _, err := p.Lease(context.Background(), sfOwner, sid)
	require.ErrorIs(t, err, boom, "a load failure must be reported, not swallowed")

	assert.Equal(t, -1, inUseOf(p, sid),
		"a failed load must leave NO entry behind — a poisoned placeholder would make every "+
			"later request for this session fail forever")
	assert.Equal(t, 0, p.Len())

	// The store recovers. The very next request must succeed.
	gs.setErr(nil)
	eng, release, err := p.Lease(context.Background(), sfOwner, sid)
	require.NoError(t, err, "the session must recover as soon as the store does")
	require.NotNil(t, eng)
	release()
	assert.Equal(t, 2, gs.loads(sid), "the retry really did re-load")
}

// GATE 6 — a QUEUED lease pins its entry.
//
// ⚠️ This gate is white-box, and it is white-box on purpose. My previous attempt
// at it was a BLACK-BOX test with the same name, and it was an EMPTY GATE:
// removing the queued caller's pin left it green, because whenever a caller is
// queued, the lease-HOLDER's own pin is already keeping the entry alive, so the
// test could not tell which pin had saved it. The window the queued pin actually
// protects — between the holder's e.mu.Unlock() and the waiter's e.mu.Lock() —
// is a few instructions wide and cannot be forced open from outside the package.
//
// So this asserts the invariant directly on the pin count. Under the mutation
// (pin only after acquiring e.mu) inUse stays at 1 while a second caller is
// queued, and this fails.
//
// What the pin buys: without it, the moment the holder releases, inUse drops to
// 0 while the waiter is still blocked on e.mu. An evictor may then drop the
// entry, a third request misses, builds a SECOND engine — and the waiter wakes
// up holding the lock of an engine that is no longer the session's. Two engines,
// one session.
func TestPool_AQueuedLeasePinsItsEntry(t *testing.T) {
	const sid = "sess-queued"

	gs := newGatedStore()
	gs.release()
	p := sfPool(t, gs, 200)

	_, releaseHolder, err := p.Lease(context.Background(), sfOwner, sid)
	require.NoError(t, err)
	require.Equal(t, 1, inUseOf(p, sid), "precondition: the holder is the only pin")

	queued := make(chan func(), 1)
	go func() {
		_, release, err := p.Lease(context.Background(), sfOwner, sid)
		require.NoError(t, err)
		queued <- release
	}()

	eventually(t, "the queued caller to pin the entry it is waiting for", func() bool {
		return inUseOf(p, sid) == 2
	})

	releaseHolder()
	release := <-queued
	release()

	eventually(t, "both pins to be given back", func() bool { return inUseOf(p, sid) == 0 })
}
