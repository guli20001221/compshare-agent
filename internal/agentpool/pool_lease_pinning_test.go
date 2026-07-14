package agentpool_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingMessageStore records how many times each session was rebuilt from the DB. A rebuild
// is the observable fingerprint of an eviction: it is the only reason buildEngine runs twice for
// one session. Counting per SESSION (not in total) is what lets a test say "the busy one was
// never dropped" while other sessions churn through the cache around it.
type countingMessageStore struct {
	mu    sync.Mutex
	calls map[string]int
}

func newCountingStore() *countingMessageStore {
	return &countingMessageStore{calls: map[string]int{}}
}

func (m *countingMessageStore) Append(_ context.Context, _ store.Message) error { return nil }
func (m *countingMessageStore) UpdateAssistant(_ context.Context, _ store.Owner, _ string, _ store.AssistantPatch) error {
	return nil
}
func (m *countingMessageStore) MarkAssistantOutcome(_ context.Context, _ store.Owner, _ string, _ string, _ *string, _, _ *int) error {
	return nil
}
func (m *countingMessageStore) GetWithOwnerCheck(_ context.Context, _ store.Owner, _ string) (store.Message, error) {
	return store.Message{}, nil
}
func (m *countingMessageStore) ListBySession(_ context.Context, sessionID string, _ int, _ string) ([]store.Message, string, error) {
	m.mu.Lock()
	m.calls[sessionID]++
	m.mu.Unlock()
	return nil, "", nil
}
func (m *countingMessageStore) builds(sessionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[sessionID]
}

// leaseAsync takes a lease on a background goroutine and hands back the engine plus a release
// func, so the test goroutine can hold a lease and keep working. Leasing the same session twice
// from one goroutine would deadlock on the entry mutex — which is the point of the mutex.
func leaseAsync(t *testing.T, pool *agentpool.Pool, sessionID string) (*engine.Engine, func()) {
	t.Helper()
	type leased struct {
		eng     *engine.Engine
		release func()
		err     error
	}
	ch := make(chan leased, 1)
	go func() {
		eng, release, err := pool.Lease(context.Background(), owner1, sessionID)
		ch <- leased{eng, release, err}
	}()
	select {
	case got := <-ch:
		require.NoError(t, got.err)
		return got.eng, got.release
	case <-time.After(5 * time.Second):
		t.Fatalf("lease on %s never returned", sessionID)
		return nil, nil
	}
}

// A session with a turn RUNNING on it must not be evicted for capacity.
//
// The pool's eviction used to delete the entry from the map without looking at whether anyone
// was using it. Evicting an entry does not stop the goroutine holding its lock: the lease-holder
// keeps running on the engine we dropped, the next request for that session misses the cache and
// builds a SECOND engine with a SECOND mutex, and the two turns of one session then run
// CONCURRENTLY — on a single replica, each hydrating from its own snapshot of the session row
// and writing over the other.
//
// That is not a cache-efficiency bug. It is what makes "the lease serializes this session" false
// exactly when the pool is under pressure, and every fix built on that promise inherits the lie.
func TestPool_ASessionWithATurnRunningIsNotEvictedForCapacity(t *testing.T) {
	ms := newCountingStore()
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 2,
		IdleTTL:  time.Hour,
	})
	defer pool.Close()

	// A turn is in flight on sess-busy.
	busyEng, releaseBusy := leaseAsync(t, pool, "sess-busy")
	require.Equal(t, 1, ms.builds("sess-busy"))

	// Meanwhile other sessions churn through the pool — far more than it can hold.
	for _, id := range []string{"sess-1", "sess-2", "sess-3", "sess-4", "sess-5"} {
		_, rel := leaseAsync(t, pool, id)
		rel()
	}

	// THE ASSERTION. The busy session is still the same engine behind the same lock. If it had
	// been evicted, this lease would rebuild it — and the turn still running above would be
	// writing to an engine nobody else can see.
	releaseBusy()
	again, rel := leaseAsync(t, pool, "sess-busy")
	defer rel()

	assert.Same(t, busyEng, again,
		"the session that had a turn running must still be the SAME engine — a rebuild here means the pool evicted it mid-turn and the next request would have raced the running one")
	assert.Equal(t, 1, ms.builds("sess-busy"),
		"sess-busy must have been built exactly ONCE: a second build is the fingerprint of the eviction that must not have happened")
}

// A session with a turn QUEUED behind another turn must not be evicted either.
//
// The queue is where a busy session spends its time. Dropping the entry out from under a waiting
// lease hands it the same split-brain a moment later: it wakes up holding a lock on an engine the
// pool has already forgotten, while the next request builds a fresh one.
func TestPool_AQueuedSessionIsNotEvictedForCapacity(t *testing.T) {
	ms := newCountingStore()
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 2,
		IdleTTL:  time.Hour,
	})
	defer pool.Close()

	first, releaseFirst := leaseAsync(t, pool, "sess-queued")

	// A second turn on the same session: it blocks on the entry mutex. It is not "using" the
	// engine yet — but it is committed to using THIS one.
	type leased struct {
		eng     *engine.Engine
		release func()
	}
	waiter := make(chan leased, 1)
	go func() {
		eng, release, err := pool.Lease(context.Background(), owner1, "sess-queued")
		if err != nil {
			close(waiter)
			return
		}
		waiter <- leased{eng, release}
	}()

	// Give the waiter time to actually reach the mutex, then evict-storm the pool.
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{"sess-1", "sess-2", "sess-3", "sess-4", "sess-5"} {
		_, rel := leaseAsync(t, pool, id)
		rel()
	}

	// THE OBSERVABLE. A THIRD request for the same session arrives while the first lease is
	// still held. It must QUEUE — that is what "one session, one lock" means.
	//
	// Asserting only that the waiter wakes on the same engine would be an empty gate, and it is
	// worth being precise about why: the waiter is blocked on the entry's OWN mutex and holds a
	// pointer to it, so it wakes on that engine whether or not the pool still knows the entry
	// exists. It cannot see its own eviction. A newcomer can: if the entry was dropped, this
	// lease MISSES the cache, builds a second engine with a second mutex, and sails straight
	// through while the first turn is still running — two turns of one session, live at once.
	third := make(chan *engine.Engine, 1)
	go func() {
		eng, release, err := pool.Lease(context.Background(), owner1, "sess-queued")
		if err != nil {
			close(third)
			return
		}
		defer release()
		third <- eng
	}()

	select {
	case eng := <-third:
		t.Fatalf("a third lease on a session whose turn is still running acquired IMMEDIATELY (engine %p vs %p) — the pool evicted the entry, so this request built a SECOND engine with a SECOND lock and two turns of one session are now running concurrently", eng, first)
	case <-time.After(300 * time.Millisecond):
		// Correct: it is queued behind the running turn, exactly like the waiter.
	}

	releaseFirst()

	select {
	case got, ok := <-waiter:
		require.True(t, ok, "the queued lease failed")
		got.release()
		assert.Same(t, first, got.eng,
			"the queued lease must wake up on the SAME engine it queued for")
	case <-time.After(5 * time.Second):
		t.Fatal("the queued lease never acquired — releasing the first lease must hand the session to the waiter")
	}

	select {
	case eng, ok := <-third:
		require.True(t, ok, "the third lease failed")
		assert.Same(t, first, eng, "the third lease must land on the same engine too — one session, one engine")
	case <-time.After(5 * time.Second):
		t.Fatal("the third lease never acquired")
	}

	assert.Equal(t, 1, ms.builds("sess-queued"),
		"one build only: three leases, one engine. A second build is the fingerprint of the eviction that must not have happened")
}

// The idle-TTL sweeper must not take a running turn either. A turn can legitimately outlive the
// TTL — a long ReAct loop, a slow upstream — and lastTouched is only stamped when a lease is
// taken and released, so a long-running turn looks STALEST precisely while it is busiest.
func TestPool_ARunningTurnSurvivesTheIdleSweeper(t *testing.T) {
	ms := newCountingStore()
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  30 * time.Millisecond, // gc ticks every 10ms at this TTL
	})
	defer pool.Close()

	eng, release := leaseAsync(t, pool, "sess-long")

	// Outlive the TTL several times over, with the turn still in flight.
	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, 1, pool.Len(),
		"the idle sweeper must not evict a session with a turn RUNNING on it, however long the turn takes")

	release()

	// Now it is genuinely idle, and the sweeper must do its job — the pin must not have turned
	// into a leak.
	require.Eventually(t, func() bool { return pool.Len() == 0 }, 2*time.Second, 10*time.Millisecond,
		"once the turn releases, the entry is idle and MUST become evictable again — a pin that outlives its lease is a memory leak")

	again, rel := leaseAsync(t, pool, "sess-long")
	defer rel()
	assert.NotSame(t, eng, again, "after a genuine idle eviction the session is rebuilt cold")
	assert.Equal(t, 2, ms.builds("sess-long"))
}

// Capacity bounds IDLE engines. When every entry is in use there is nothing the pool may legally
// evict, and both alternatives are worse than a temporary overshoot:
//
//   - evict an active session → the split-brain above;
//   - block until a slot frees → one busy session stalls every OTHER session, turning a soft
//     capacity target into a pool-wide stall.
//
// So the pool goes over capacity on purpose, and takes the excess back as soon as turns finish.
func TestPool_AllEntriesBusy_OvershootsCapacityInsteadOfEvictingOrBlocking(t *testing.T) {
	ms := newCountingStore()
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 2,
		IdleTTL:  time.Hour,
	})
	defer pool.Close()

	ids := []string{"sess-a", "sess-b", "sess-c", "sess-d"}
	engs := make([]*engine.Engine, len(ids))
	rels := make([]func(), len(ids))
	for i, id := range ids {
		// leaseAsync fails the test on a 5s timeout, so this also pins "must not block".
		engs[i], rels[i] = leaseAsync(t, pool, id)
	}

	assert.Equal(t, len(ids), pool.Len(),
		"with every entry in use the pool must admit the new sessions rather than evict a running one or stall the caller")
	for i, id := range ids {
		assert.Equal(t, 1, ms.builds(id), "%s must not have been evicted and rebuilt while in use", id)
		require.NotNil(t, engs[i])
	}

	// Turns finish. The overshoot the pins were holding open must be given back.
	for _, rel := range rels {
		rel()
	}
	assert.LessOrEqual(t, pool.Len(), 2,
		"once the turns release, the pool must reclaim down to capacity — an overshoot that is never reclaimed is an unbounded cache")
}

// The sibling that keeps the pin from becoming "never evict anything": capacity eviction must
// still work normally for idle sessions. Without this, "return false from every eviction" would
// satisfy every test above.
func TestPool_IdleSessionsAreStillEvictedForCapacity(t *testing.T) {
	ms := newCountingStore()
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 2,
		IdleTTL:  time.Hour,
	})
	defer pool.Close()

	for _, id := range []string{"sess-1", "sess-2", "sess-3"} {
		_, rel := leaseAsync(t, pool, id)
		rel() // idle immediately
	}

	assert.Equal(t, 2, pool.Len(), "capacity must still bound idle engines")

	// sess-1 was the LRU idle entry, so it is the one that went.
	_, rel := leaseAsync(t, pool, "sess-1")
	defer rel()
	assert.Equal(t, 2, ms.builds("sess-1"),
		"an IDLE session must still be evicted at capacity and rebuilt on next use — the pin protects running turns, not everything")
}
