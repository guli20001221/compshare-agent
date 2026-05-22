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
// live LLM (model is blank; we never call Chat in pool tests).
func minimalConfig() *config.Config {
	return &config.Config{
		Agent: config.AgentConfig{
			LLM: config.LLMConfig{
				BaseURL: "http://localhost:1",
				Model:   "",
			},
		},
	}
}

var owner1 = store.Owner{TopOrganizationID: 1, OrganizationID: 1}

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

	eng1, err := pool.Get(ctx, owner1, "sess-1")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	eng2, err := pool.Get(ctx, owner1, "sess-1")
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

	eng1, err := pool.Get(ctx, owner1, "sess-1")
	if err != nil {
		t.Fatalf("Get sess-1: %v", err)
	}

	_, err = pool.Get(ctx, owner1, "sess-2")
	if err != nil {
		t.Fatalf("Get sess-2: %v", err)
	}

	// sess-1 should have been evicted; re-Get rebuilds it as a new engine.
	eng1b, err := pool.Get(ctx, owner1, "sess-1")
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

	eng1, err := pool.Get(ctx, owner1, "sess-ttl")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// Wait until the gc loop evicts the idle entry (pool size drops to 0).
	// Total budget: 1 s; step: 10 ms.
	require.Eventually(t, func() bool {
		return pool.SizeForTest() == 0
	}, 1*time.Second, 10*time.Millisecond, "idle engine was not evicted within 1s")

	// A fresh Get must rebuild the engine (new pointer, new ListBySession call).
	eng2, err := pool.Get(ctx, owner1, "sess-ttl")
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
// messages whose status is "ok". Messages with status pending / error / aborted
// or any other value must be excluded regardless of role.
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
