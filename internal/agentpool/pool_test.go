package agentpool_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/require"
)

// mockMessageStore is a minimal store.MessageStore for tests.
// It only implements ListBySession; other methods are no-ops.
type mockMessageStore struct {
	listCalls int
	messages  []store.Message
}

func (m *mockMessageStore) Append(_ context.Context, _ store.Message) error { return nil }
func (m *mockMessageStore) UpdateAssistant(_ context.Context, _ store.Owner, _ string, _ store.AssistantPatch) error {
	return nil
}
func (m *mockMessageStore) ListBySession(_ context.Context, _ string, _ int, _ string) ([]store.Message, string, error) {
	m.listCalls++
	return m.messages, "", nil
}
func (m *mockMessageStore) GetWithOwnerCheck(_ context.Context, _ store.Owner, _ string) (store.Message, error) {
	return store.Message{}, nil
}

// minimalConfig returns a Config that satisfies engine.New without requiring a
// live LLM. Model is a non-empty placeholder: pool tests never call Chat, but
// SharedDeps rejects an empty model at construction time. The endpoint is never
// dialed by these tests.
func minimalConfig() *config.Config {
	return &config.Config{
		Agent: config.AgentConfig{
			LLM: config.LLMConfig{
				BaseURL: "http://localhost:1",
				Model:   "test-model",
			},
		},
	}
}

var owner1 = store.Owner{TopOrganizationID: 1, OrganizationID: 1}

func leaseAndRelease(ctx context.Context, pool *agentpool.Pool, owner store.Owner, sessionID string) (*engine.Engine, error) {
	eng, release, err := pool.Lease(ctx, owner, sessionID)
	if release != nil {
		release()
	}
	return eng, err
}

// TestPoolHitReusesEngine verifies that two consecutive Get calls for the same
// (owner, sessionID) return the same *engine.Engine pointer and only call ListBySession once.
func TestPoolHitReusesEngine(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()

	eng1, err := leaseAndRelease(ctx, pool, owner1, "sess-1")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	eng2, err := leaseAndRelease(ctx, pool, owner1, "sess-1")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}

	if eng1 != eng2 {
		t.Error("expected same engine pointer on cache hit, got different pointers")
	}
	if ms.listCalls != 1 {
		t.Errorf("expected ListBySession called once, got %d", ms.listCalls)
	}
}

// TestPoolLRUEviction verifies that a pool with capacity=1 evicts sess-1 when
// sess-2 is inserted, so a subsequent Get("sess-1") returns a fresh engine.
func TestPoolLRUEviction(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 1,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()

	eng1, err := leaseAndRelease(ctx, pool, owner1, "sess-1")
	if err != nil {
		t.Fatalf("Get sess-1: %v", err)
	}

	_, err = leaseAndRelease(ctx, pool, owner1, "sess-2")
	if err != nil {
		t.Fatalf("Get sess-2: %v", err)
	}

	// sess-1 should have been evicted; re-Get rebuilds it as a new engine.
	eng1b, err := leaseAndRelease(ctx, pool, owner1, "sess-1")
	if err != nil {
		t.Fatalf("second Get sess-1: %v", err)
	}

	if eng1b == eng1 {
		t.Error("expected a new engine after LRU eviction, got same pointer")
	}
	// ListBySession should have been called for sess-1 twice (initial + after eviction).
	if ms.listCalls != 3 {
		t.Errorf("expected ListBySession called 3 times (sess-1, sess-2, sess-1 again), got %d", ms.listCalls)
	}
}

// TestPoolIdleTTLEviction verifies that an engine idle beyond IdleTTL is
// removed by the gc loop and rebuilt on next Get. Uses require.Eventually
// instead of a fixed sleep so the test tolerates loaded CI environments.
func TestPoolIdleTTLEviction(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  50 * time.Millisecond, // short TTL triggers 10ms gc tick
	})
	defer pool.Close()

	ctx := context.Background()

	eng1, err := leaseAndRelease(ctx, pool, owner1, "sess-ttl")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// Wait until the gc loop evicts the idle entry (pool size drops to 0).
	// Total budget: 1 s; step: 10 ms.
	require.Eventually(t, func() bool {
		return pool.SizeForTest() == 0
	}, 1*time.Second, 10*time.Millisecond, "idle engine was not evicted within 1s")

	// A fresh Get must rebuild the engine (new pointer, new ListBySession call).
	eng2, err := leaseAndRelease(ctx, pool, owner1, "sess-ttl")
	if err != nil {
		t.Fatalf("Get after eviction: %v", err)
	}

	if eng2 == eng1 {
		t.Error("expected a new engine after idle TTL eviction, got same pointer")
	}
	if ms.listCalls != 2 {
		t.Errorf("expected ListBySession called twice, got %d", ms.listCalls)
	}
}

// TestFilterHistoryStatusGating verifies that filterHistory only passes through
// successful messages plus unanswered assistant boundaries. Failed display
// content and pending rows must not become completed answers.
func TestFilterHistoryStatusGating(t *testing.T) {
	msgs := []store.Message{
		{Role: "user", Content: "hello", Status: "ok"},
		{Role: "assistant", Content: "hi there", Status: "ok"},
		{Role: "user", Content: "pending msg", Status: "pending"},
		{Role: "assistant", Content: "error reply", Status: "error"},
		{Role: "user", Content: "aborted", Status: "aborted"},
		{Role: "system", Content: "system ok", Status: "ok"}, // role filtered
		{Role: "tool", Content: "tool ok", Status: "ok"},     // role filtered
	}

	got := agentpool.FilterHistoryForTest(msgs)

	want := []engine.HistoryMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "assistant"},
	}

	require.Equal(t, want, got)
}

// TestPoolCloseIdempotent verifies that calling Close twice does not panic.
func TestPoolCloseIdempotent(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  5 * time.Minute,
	})

	require.NotPanics(t, func() {
		pool.Close()
		pool.Close()
	})
}

// TestLeaseSerialization verifies that concurrent Lease calls for the same session
// are serialized: the second caller blocks until the first releases.
func TestLeaseSerialization(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()

	// First lease — hold it.
	eng1, release1, err := pool.Lease(ctx, owner1, "sess-serial")
	require.NoError(t, err)

	var (
		secondStarted  int32
		secondFinished int32
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		atomic.StoreInt32(&secondStarted, 1)
		eng2, release2, err2 := pool.Lease(ctx, owner1, "sess-serial")
		require.NoError(t, err2)
		defer release2()
		// eng2 must be the same instance as eng1 (cache hit).
		require.True(t, eng1 == eng2, "expected same engine pointer from Lease cache hit")
		atomic.StoreInt32(&secondFinished, 1)
	}()

	// Give the goroutine time to start and block on the entry mutex.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&secondStarted) == 1
	}, time.Second, time.Millisecond, "second goroutine never started")

	// Allow a brief moment for the goroutine to hit the Lease call.
	time.Sleep(5 * time.Millisecond)

	// The second caller should still be blocked.
	require.Equal(t, int32(0), atomic.LoadInt32(&secondFinished), "second Lease should be blocked while first holds lock")

	// Release first lease — now second can proceed.
	release1()

	wg.Wait()
	require.Equal(t, int32(1), atomic.LoadInt32(&secondFinished), "second Lease should complete after first releases")
}

func TestCapacityEvictionCannotDuplicateAnActivelyLeasedSession(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 1,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()
	engA, releaseA, err := pool.Lease(ctx, owner1, "sess-a")
	require.NoError(t, err)

	// Capacity pressure may temporarily overflow while A is in use, but it must not evict A and
	// create a second mutex/Engine identity for the same session.
	_, releaseB, err := pool.Lease(ctx, owner1, "sess-b")
	require.NoError(t, err)
	require.Equal(t, 2, pool.SizeForTest())
	releaseB()
	require.Equal(t, 1, pool.SizeForTest(), "release should converge the soft capacity")

	started := make(chan struct{})
	finished := make(chan *engine.Engine, 1)
	go func() {
		close(started)
		eng, release, leaseErr := pool.Lease(ctx, owner1, "sess-a")
		if leaseErr != nil {
			finished <- nil
			return
		}
		defer release()
		finished <- eng
	}()
	<-started
	select {
	case <-finished:
		t.Fatal("second lease of active session did not wait for the existing per-session mutex")
	case <-time.After(20 * time.Millisecond):
	}
	require.Equal(t, 2, ms.listCalls, "active sess-a must not be rebuilt under capacity pressure")

	releaseA()
	select {
	case engA2 := <-finished:
		require.Same(t, engA, engA2)
	case <-time.After(time.Second):
		t.Fatal("waiting same-session lease did not resume")
	}
}

// TestLeaseOwnerScoping verifies that different owners with the same sessionID
// get independent engine instances.
func TestLeaseOwnerScoping(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()

	ownerA := store.Owner{TopOrganizationID: 1, OrganizationID: 10}
	ownerB := store.Owner{TopOrganizationID: 2, OrganizationID: 20}
	const sessID = "same-session-id"

	engA, releaseA, err := pool.Lease(ctx, ownerA, sessID)
	require.NoError(t, err)
	releaseA()

	engB, releaseB, err := pool.Lease(ctx, ownerB, sessID)
	require.NoError(t, err)
	releaseB()

	require.True(t, engA != engB, "different owners must get different engine instances for the same sessionID")
	require.Equal(t, 2, ms.listCalls, "expected two ListBySession calls (one per owner)")
	require.Equal(t, 2, pool.SizeForTest(), "pool should hold two entries (one per owner)")
}

// TestPoolEnginesShareProcessWideDependencies verifies the HTTP pool does not
// rebuild process-wide engine dependencies on every session miss. The LLM
// client and rate limiter are shared across sessions; per-session state such as
// the entity registry remains isolated.
func TestPoolEnginesShareProcessWideDependencies(t *testing.T) {
	ms := &mockMessageStore{}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 10,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()
	engA, err := leaseAndRelease(ctx, pool, owner1, "sess-a")
	require.NoError(t, err)
	engB, err := leaseAndRelease(ctx, pool, owner1, "sess-b")
	require.NoError(t, err)

	require.Same(t, engA.LLMClientPointer(), engB.LLMClientPointer(), "LLM client should be process-wide")
	require.Same(t, engA.RateLimiterPointer(), engB.RateLimiterPointer(), "rate limiter should be process-wide")
	require.NotSame(t, engA.RegistryPointer(), engB.RegistryPointer(), "registries must stay per session")
}
