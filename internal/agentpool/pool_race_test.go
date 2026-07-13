package agentpool_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingMessageStore holds every ListBySession caller inside buildEngine until the test lets
// them all go. That is what forces the pool's lost-race branch to run: both callers must be
// PAST the fast-path cache check and INSIDE the slow build at the same time, which is exactly
// the window the branch exists for and which a sequential test can never open.
type blockingMessageStore struct {
	entered chan struct{} // one send per caller that has reached the build
	release chan struct{} // closed by the test once every caller is inside
}

func (m *blockingMessageStore) Append(_ context.Context, _ store.Message) error { return nil }
func (m *blockingMessageStore) UpdateAssistant(_ context.Context, _ store.Owner, _ string, _ store.AssistantPatch) error {
	return nil
}
func (m *blockingMessageStore) GetWithOwnerCheck(_ context.Context, _ store.Owner, _ string) (store.Message, error) {
	return store.Message{}, nil
}
func (m *blockingMessageStore) ListBySession(_ context.Context, _ string, _ int, _ string) ([]store.Message, string, error) {
	m.entered <- struct{}{}
	<-m.release
	return []store.Message{
		{Role: "user", Status: "ok", Content: "先前的问题"},
		{Role: "assistant", Status: "ok", Content: "先前的回答"},
	}, "", nil
}

func raceTestConfig() *config.Config {
	return &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "test-model"},
	}}
}

// BuildRaced must be set by the POOL, on a real lost race.
//
// The first version of this gate asserted only that a bool travelled from a fake pool into the
// trace. It never ran internal/agentpool at all — hardcoding the pool's lost-race branch to
// report raced=false left it green. That is an empty gate: it pins the plumbing and lets the
// thing being plumbed break silently, which is precisely the failure mode BuildRaced exists to
// make visible.
//
// This drives the real Pool. Two goroutines lease the same session; both miss the cache, both
// enter buildEngine, and the test holds them there until both are inside. Then it releases
// them and the pool's own lock decides: one inserts and wins, the other finds the winner
// already there, throws away the engine it just built, and takes the lost-race branch.
//
// WHY the flag has to be right: a lost race and an eviction are both "cold", and they need
// OPPOSITE fixes — raise the pool capacity, or serialize the duplicate request. Reported as one
// number, a burst of concurrent traffic reads as a capacity problem and the real cause is
// invisible.
func TestPool_LostRaceIsReportedAsRaced_NotAsAnEviction(t *testing.T) {
	const racers = 2
	ms := &blockingMessageStore{
		entered: make(chan struct{}, racers),
		release: make(chan struct{}),
	}
	pool := agentpool.New(raceTestConfig(), ms, agentpool.Options{
		Capacity: 10, // roomy: nothing here can be evicted, so "cold" can ONLY mean the race
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	type result struct {
		eng        *engine.Engine
		cacheHit   bool
		raced      bool
		rehydrated int
	}
	results := make([]result, racers)
	errs := make([]error, racers)

	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			eng, release, cacheHit, raced, rehydrated, err := pool.LeaseWithTrace(
				context.Background(), owner1, "sess-raced")
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = result{eng, cacheHit, raced, rehydrated}
			release()
		}(i)
	}

	// Both callers are now inside buildEngine, past the cache check. Let them out together —
	// from here the pool's own mutex picks the winner, not the test.
	for i := 0; i < racers; i++ {
		select {
		case <-ms.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d callers reached buildEngine — the race window never opened, so this test would be proving nothing", i, racers)
		}
	}
	close(ms.release)
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i], "racer %d", i)
	}

	// THE ASSERTIONS. Exactly one caller lost the race, and it says so.
	racedCount := 0
	for _, r := range results {
		if r.raced {
			racedCount++
		}
		assert.False(t, r.cacheHit,
			"neither caller may report a pool HIT: both missed the cache and both built — reporting a hit would claim the turn had its in-memory tool results when it did not")
	}
	assert.Equal(t, 1, racedCount,
		"exactly ONE of two concurrent leases must report raced: the winner inserted, the loser threw its engine away and ran on the winner's. Zero means the pool is not reporting the lost race at all and every concurrent burst is misfiled as an eviction; two would mean nobody won")

	// Both turns ran on the SAME engine — the winner's. If they did not, two engines for one
	// session are live at once and the per-session lease serializes nothing.
	require.NotNil(t, results[0].eng)
	assert.Same(t, results[0].eng, results[1].eng,
		"both callers must end up on the winner's engine; two live engines for one session would make the per-session lease meaningless")
}

// The sibling that keeps the flag honest in the other direction: a cold rebuild caused by
// EVICTION must NOT be reported as a race. Without this, "always return raced=true" would pass
// the test above, and the two causes would be conflated in the opposite direction.
func TestPool_EvictionRebuildIsNotReportedAsRaced(t *testing.T) {
	ms := &blockingMessageStore{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
	close(ms.release) // nothing to hold: these leases are strictly sequential
	go func() {
		for range ms.entered { // drain, so ListBySession never blocks on the send
		}
	}()

	pool := agentpool.New(raceTestConfig(), ms, agentpool.Options{
		Capacity: 1, // one slot: the second session evicts the first
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	ctx := context.Background()

	_, rel1, _, _, _, err := pool.LeaseWithTrace(ctx, owner1, "sess-A")
	require.NoError(t, err)
	rel1()

	// sess-B evicts sess-A (capacity 1).
	_, rel2, _, _, _, err := pool.LeaseWithTrace(ctx, owner1, "sess-B")
	require.NoError(t, err)
	rel2()

	// sess-A is cold again — but because it was EVICTED, not because anyone raced it.
	_, rel3, cacheHit, raced, rehydrated, err := pool.LeaseWithTrace(ctx, owner1, "sess-A")
	require.NoError(t, err)
	rel3()

	assert.False(t, cacheHit, "precondition: sess-A must have been evicted, or this proves nothing")
	assert.False(t, raced,
		"an eviction rebuild must NOT be reported as a race — conflating them sends the fix in the wrong direction (serialize duplicates vs. raise capacity)")
	assert.Equal(t, 2, rehydrated, "the eviction rebuild restored the persisted turn from the DB")
}
