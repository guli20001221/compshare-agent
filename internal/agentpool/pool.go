// Package agentpool manages a per-session LRU cache of *engine.Engine instances.
// Each session maps to exactly one Engine; cache misses trigger a rehydration
// from the MessageStore. The cache evicts entries on overflow (LRU) and on
// idle TTL expiry (background gc goroutine).
package agentpool

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// Options configures the Pool.
type Options struct {
	// Capacity is the maximum number of engines kept in the cache at once.
	// When a new session is added and the cache is full, the least-recently-
	// used entry is evicted. Must be >= 1; defaults to 200 if zero.
	Capacity int
	// IdleTTL is the duration after which an entry that has not been accessed
	// is eligible for eviction by the gc goroutine. Must be > 0; defaults to
	// 30 minutes if zero.
	IdleTTL time.Duration
	// MutatingToolsEnabled controls whether write operations (start/stop/
	// reboot/reset-password) are enabled for engines built by this pool.
	// Default false (read-only). Set from COMPSHARE_ENABLE_MUTATING_TOOLS
	// at server startup.
	MutatingToolsEnabled bool
}

const (
	defaultCapacity = 200
	defaultIdleTTL  = 30 * time.Minute
	gcTickInterval  = 30 * time.Second
)

// entryKey is the composite map key used to scope engines by both owner and
// session. Different owners with the same SessionID get independent engines.
type entryKey struct {
	Owner     store.Owner
	SessionID string
}

// entry is one node in the LRU linked list.
// mu serializes concurrent Chat calls on the same session; callers must hold
// mu for the duration of the LLM/engine call.
type entry struct {
	key         entryKey
	eng         *engine.Engine
	mu          sync.Mutex // serializes per-session engine access
	lastTouched time.Time

	// inUse counts the leases currently HOLDING or WAITING FOR mu. It is guarded by the
	// POOL's mutex (p.mu), not by e.mu — eviction runs under p.mu and must be able to read it
	// without touching the entry's own lock.
	//
	// It exists because an entry IS a lock, and evicting a lock does not stop the goroutine
	// holding it. Eviction used to delete the entry from the map with no regard for whether a
	// turn was running on it: the lease-holder kept its *entry, the next request for the same
	// session missed the cache and built a SECOND engine with a SECOND mutex, and two turns of
	// ONE session then ran concurrently on a single replica — each hydrating from its own
	// snapshot of the session row and writing over the other. Every claim of the form "the
	// lease serializes this session" was false whenever the pool was under capacity pressure,
	// which is exactly when it matters.
	//
	// A WAITING lease counts too. Dropping the entry out from under a queued caller produces
	// the same split-brain a moment later, and the queue is precisely where a busy session
	// spends its time.
	inUse int
}

// Pool is a concurrency-safe LRU cache of *engine.Engine keyed by (Owner, SessionID).
// Entries are created lazily on Lease/Get misses by calling buildEngine, which
// rehydrates history from the MessageStore. Call Close when done to stop the
// background gc goroutine.
type Pool struct {
	deps                 *engine.SharedDeps
	messageStore         store.MessageStore
	capacity             int
	idleTTL              time.Duration
	mutatingToolsEnabled bool

	mu      sync.Mutex
	lruList *list.List                 // front = most recently used
	items   map[entryKey]*list.Element // (Owner,SessionID) → list.Element(*entry)

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New creates a Pool with the given config, MessageStore, and options.
// It starts the background gc goroutine; call Close() to stop it.
func New(cfg *config.Config, ms store.MessageStore, opts Options) *Pool {
	deps, err := engine.NewSharedDeps(cfg)
	if err != nil {
		panic(fmt.Sprintf("agentpool.New: %v", err))
	}
	return NewWithDeps(deps, ms, opts)
}

// NewWithDeps creates a Pool from process-wide shared engine dependencies.
// HTTP server mode uses this so every session shares one LLM client, limiter,
// configured planner/retriever/renderer, and credential provider.
func NewWithDeps(deps *engine.SharedDeps, ms store.MessageStore, opts Options) *Pool {
	if deps == nil {
		panic("agentpool.NewWithDeps: deps is nil")
	}
	cap := opts.Capacity
	if cap <= 0 {
		cap = defaultCapacity
	}
	ttl := opts.IdleTTL
	if ttl <= 0 {
		ttl = defaultIdleTTL
	}

	gcTick := gcTickInterval
	// Use a faster tick in tests (TTL < 1s implies test mode).
	if ttl < time.Second {
		gcTick = 10 * time.Millisecond
	}

	p := &Pool{
		deps:                 deps,
		messageStore:         ms,
		capacity:             cap,
		idleTTL:              ttl,
		mutatingToolsEnabled: opts.MutatingToolsEnabled,
		lruList:              list.New(),
		items:                make(map[entryKey]*list.Element),
		stopCh:               make(chan struct{}),
	}

	p.wg.Add(1)
	go p.gcLoop(gcTick)
	return p
}

// Lease returns the cached *engine.Engine for (owner, sessionID) plus an unlock
// closure. The per-entry mutex is held until the caller invokes the returned
// release func, serializing concurrent Chat calls on the same session.
//
//	eng, release, err := pool.Lease(ctx, owner, sessionID)
//	if err != nil { ... }
//	defer release()
//	// safe to call eng.ChatWithOptions here
//
// Callers in the HTTP path MUST use Lease instead of Get to prevent concurrent
// requests from interleaving ReAct history in the same engine.
// The entry is PINNED (inUse++) before mu is taken, and unpinned by the release closure — so it
// cannot be evicted while this turn is running, nor while it is queued behind another turn of the
// same session. That pin is what makes "one session, one engine, one lock" true. See entry.inUse.
func (p *Pool) Lease(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, func(), error) {
	e, err := p.getOrCreate(ctx, owner, sessionID, true)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	var once sync.Once
	return e.eng, func() {
		once.Do(func() {
			e.mu.Unlock()
			p.release(e)
		})
	}, nil
}

// Get returns the cached *engine.Engine for (owner, sessionID), building a fresh
// one via rehydration on a cache miss. It is safe for concurrent use.
//
// Deprecated: HTTP-path callers should use Lease to serialize per-session engine
// access. Get is retained for callers that do not require serialization (e.g.
// read-only inspection, tests).
// Get does NOT pin: it takes no lease, so there is no release point at which to unpin, and the
// engine it hands back may be evicted at any moment. One more reason not to use it on the HTTP
// path.
func (p *Pool) Get(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error) {
	e, err := p.getOrCreate(ctx, owner, sessionID, false)
	if err != nil {
		return nil, err
	}
	return e.eng, nil
}

// getOrCreate finds or builds the entry for (owner, sessionID), updating LRU state.
// Concurrency design: the pool lock is released during the potentially-slow
// buildEngine call. After buildEngine returns we re-acquire the lock and
// re-check whether another goroutine raced us to insert the same key; if
// so, we discard the duplicate and return the winner already in the cache.
// pin marks the entry in-use for a caller that will take a lease and later release it, so
// neither capacity nor idle eviction can take it away mid-turn.
func (p *Pool) getOrCreate(ctx context.Context, owner store.Owner, sessionID string, pin bool) (*entry, error) {
	k := entryKey{Owner: owner, SessionID: sessionID}

	// Fast path: cache hit.
	p.mu.Lock()
	if el, ok := p.items[k]; ok {
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
		if pin {
			e.inUse++
		}
		p.mu.Unlock()
		return e, nil
	}
	p.mu.Unlock()

	// Slow path: build a new engine outside the lock.
	eng, err := p.buildEngine(ctx, owner, sessionID)
	if err != nil {
		return nil, err
	}

	// Re-acquire lock and insert (checking for a concurrent insert).
	p.mu.Lock()
	defer p.mu.Unlock()

	if el, ok := p.items[k]; ok {
		// Another goroutine already inserted while we were building; use theirs.
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
		if pin {
			e.inUse++
		}
		return e, nil
	}

	// Make room — but only ever by taking an IDLE entry.
	//
	// When every entry is in use there is nothing we may legally evict, and both alternatives
	// are worse than a temporary overshoot:
	//
	//   - evict an active session: that is the split-brain this change exists to remove — the
	//     turn running on it keeps its lock, the next request for it builds a second engine,
	//     and one session ends up with two engines racing each other;
	//   - block until a slot frees: one busy session would stall every OTHER session, turning a
	//     soft capacity target into a pool-wide stall.
	//
	// So capacity bounds IDLE engines, and a burst of concurrent sessions may exceed it
	// temporarily. release() reclaims the excess the moment an entry goes idle, so the
	// overshoot is bounded by the number of turns actually in flight and drains on its own.
	if len(p.items) >= p.capacity {
		p.evictOneIdleLocked()
	}

	e := &entry{
		key:         k,
		eng:         eng,
		lastTouched: time.Now(),
	}
	if pin {
		e.inUse = 1
	}
	el := p.lruList.PushFront(e)
	p.items[k] = el
	return e, nil
}

// release drops one lease's pin and reclaims any capacity overshoot that pin was holding open.
// Called by the closure Lease returns, AFTER e.mu is unlocked.
func (p *Pool) release(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e.inUse > 0 {
		e.inUse--
	}
	if e.inUse == 0 {
		// The turn is over, so the entry is idle as of NOW — most recently used, not least.
		// Without this a long turn would surface from the pool looking stale and be first in
		// line for eviction.
		e.lastTouched = time.Now()
		if el, ok := p.items[e.key]; ok {
			p.lruList.MoveToFront(el)
		}
	}

	// An entry just went idle, so an overshoot admitted while everything was busy may now be
	// reclaimable. Only idle entries are candidates, so this stops on its own when the
	// remaining excess is all still in flight.
	for len(p.items) > p.capacity {
		if !p.evictOneIdleLocked() {
			break
		}
	}
}

// Len reports how many engines the pool holds. It may briefly exceed Capacity when a burst of
// concurrent sessions arrives with every entry in use — see getOrCreate.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// Close stops the background gc goroutine and waits for it to exit.
// It is safe to call Close more than once; subsequent calls are no-ops.
func (p *Pool) Close() {
	p.closeOnce.Do(func() { close(p.stopCh) })
	p.wg.Wait()
}

// gcLoop periodically evicts entries that have been idle longer than p.idleTTL.
// When IdleTTL < 1 s (e.g. in tests) the tick is shortened to 10 ms so that
// eviction completes within a tight polling budget without requiring large
// fixed sleeps in tests.
func (p *Pool) gcLoop(tick time.Duration) {
	defer p.wg.Done()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.evictIdle()
		}
	}
}

// evictIdle scans all entries and removes those idle beyond idleTTL.
//
// "Idle" now means what the name always claimed: no lease holds or waits for the entry. A turn
// can legitimately outlive IdleTTL (a long ReAct loop, a slow upstream), and evicting it
// mid-flight splits the session across two engines. lastTouched is only stamped when a lease is
// taken and released, so a long-running turn looks STALEST precisely while it is busiest.
func (p *Pool) evictIdle() {
	deadline := time.Now().Add(-p.idleTTL)
	p.mu.Lock()
	defer p.mu.Unlock()
	// Traverse from back (LRU) to front (MRU). An in-use entry is SKIPPED rather than treated
	// as a stopping point: it is not evictable, but entries ahead of it may still be.
	for el := p.lruList.Back(); el != nil; {
		e := el.Value.(*entry)
		prev := el.Prev()
		if e.inUse > 0 {
			el = prev
			continue
		}
		if e.lastTouched.After(deadline) {
			break
		}
		p.lruList.Remove(el)
		delete(p.items, e.key)
		el = prev
	}
}

// evictOneIdleLocked removes the least-recently-used IDLE entry and reports whether it found one.
// Must be called with p.mu held.
//
// It returns false — rather than evicting something — when every entry is in use. The caller then
// goes over capacity on purpose; see getOrCreate. Evicting a busy entry is never an option: the
// lease-holder keeps running on the engine we just dropped, and the next request for that session
// builds a second one.
func (p *Pool) evictOneIdleLocked() bool {
	for el := p.lruList.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*entry)
		if e.inUse > 0 {
			continue
		}
		p.lruList.Remove(el)
		delete(p.items, e.key)
		return true
	}
	return false
}
