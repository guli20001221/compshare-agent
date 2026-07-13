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
	// rehydratedMessages is how many persisted messages buildEngine restored into
	// eng when THIS entry was built. Written once, before the entry is published
	// under p.mu, and never mutated afterwards. Pure observability — see
	// LeaseWithTrace.
	rehydratedMessages int
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
func (p *Pool) Lease(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, func(), error) {
	eng, release, _, _, _, err := p.LeaseWithTrace(ctx, owner, sessionID)
	return eng, release, err
}

// LeaseWithTrace is Lease plus the two facts the HTTP layer cannot otherwise
// see about the engine it was handed:
//
//   - cacheHit — the engine came from the LIVE pool with its full in-memory
//     history (hot). false means the pool had evicted the session (LRU at
//     capacity, or idle past IdleTTL) and the engine was REBUILT from the DB
//     (cold), or the session is brand new.
//   - rehydrated — how many persisted messages that rebuild restored. Zero on a
//     hot lease: nothing was rehydrated this turn.
//
// Hot and cold are NOT equivalent: a cold rebuild restores only persisted
// user/assistant text, so the tool results and retrieved evidence a follow-up
// refers to are gone even though the transcript looks complete. A turn where "the
// agent forgot" cannot be attributed without knowing which one it got — that is
// the whole reason this returns anything (see internal/observability/session.go).
//
// Observability only: it takes the same lock and does the same work as Lease.
// raced reports that this caller MISSED the pool, built an engine, and then threw it away
// because a concurrent request for the same session had already inserted one. The turn ran on
// the winner's engine — itself just built — so cacheHit is correctly false. But "cold because
// the pool evicted us" and "cold because two requests for one session arrived at once" are
// different causes needing opposite fixes, and without this they are a single number: a burst
// of concurrent traffic would read as a capacity problem.
func (p *Pool) LeaseWithTrace(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, func(), bool, bool, int, error) {
	e, cacheHit, raced, err := p.getOrCreate(ctx, owner, sessionID)
	if err != nil {
		return nil, nil, false, false, 0, err
	}
	rehydrated := 0
	if !cacheHit {
		rehydrated = e.rehydratedMessages
	}
	e.mu.Lock()
	return e.eng, func() { e.mu.Unlock() }, cacheHit, raced, rehydrated, nil
}

// Get returns the cached *engine.Engine for (owner, sessionID), building a fresh
// one via rehydration on a cache miss. It is safe for concurrent use.
//
// Deprecated: HTTP-path callers should use Lease to serialize per-session engine
// access. Get is retained for callers that do not require serialization (e.g.
// read-only inspection, tests).
func (p *Pool) Get(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error) {
	e, _, _, err := p.getOrCreate(ctx, owner, sessionID)
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
//
// cacheHit reports whether the engine came from the live pool (true) or had to be
// rebuilt from the DB (false). The lost-race branch returns false: the caller's
// turn still ran on a freshly rebuilt engine, just the winner's rather than its
// own — attributing that as a pool hit would hide a cold rebuild.
func (p *Pool) getOrCreate(ctx context.Context, owner store.Owner, sessionID string) (*entry, bool, bool, error) {
	k := entryKey{Owner: owner, SessionID: sessionID}

	// Fast path: cache hit.
	p.mu.Lock()
	if el, ok := p.items[k]; ok {
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
		p.mu.Unlock()
		return e, true, false, nil
	}
	p.mu.Unlock()

	// Slow path: build a new engine outside the lock.
	eng, rehydrated, err := p.buildEngine(ctx, owner, sessionID)
	if err != nil {
		return nil, false, false, err
	}

	// Re-acquire lock and insert (checking for a concurrent insert).
	p.mu.Lock()
	defer p.mu.Unlock()

	if el, ok := p.items[k]; ok {
		// Another goroutine inserted while we were building: use theirs, and DISCARD the engine
		// we just built. CacheHit stays FALSE and that is correct — the winner's engine was
		// itself just rebuilt from the DB, so this turn has no tool results either. But Raced
		// records WHY it is cold. "Cold because the pool evicted us" and "cold because two
		// requests for one session arrived at once" need opposite fixes, and without this flag
		// they are a single number: a burst of concurrent traffic would read as a capacity
		// problem, which is exactly the misattribution this whole block exists to prevent.
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
		return e, false, true, nil
	}

	// Evict LRU if at capacity.
	if len(p.items) >= p.capacity {
		p.evictLRULocked()
	}

	e := &entry{
		key:                k,
		eng:                eng,
		lastTouched:        time.Now(),
		rehydratedMessages: rehydrated,
	}
	el := p.lruList.PushFront(e)
	p.items[k] = el
	// We built it and we won the insert: a genuine cold rebuild, not a race.
	return e, false, false, nil
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
func (p *Pool) evictIdle() {
	deadline := time.Now().Add(-p.idleTTL)
	p.mu.Lock()
	defer p.mu.Unlock()
	// Traverse from back (LRU) to front (MRU); stop as soon as we hit a
	// recently-touched entry (list is ordered by recency).
	for el := p.lruList.Back(); el != nil; {
		e := el.Value.(*entry)
		if e.lastTouched.After(deadline) {
			break
		}
		prev := el.Prev()
		p.lruList.Remove(el)
		delete(p.items, e.key)
		el = prev
	}
}

// evictLRULocked removes the least-recently-used entry. Must be called with p.mu held.
func (p *Pool) evictLRULocked() {
	el := p.lruList.Back()
	if el == nil {
		return
	}
	e := el.Value.(*entry)
	p.lruList.Remove(el)
	delete(p.items, e.key)
}
