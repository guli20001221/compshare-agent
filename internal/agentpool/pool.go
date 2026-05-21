// Package agentpool manages a per-session LRU cache of *engine.Engine instances.
// Each session maps to exactly one Engine; cache misses trigger a rehydration
// from the MessageStore. The cache evicts entries on overflow (LRU) and on
// idle TTL expiry (background gc goroutine).
package agentpool

import (
	"container/list"
	"context"
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
}

// Pool is a concurrency-safe LRU cache of *engine.Engine keyed by (Owner, SessionID).
// Entries are created lazily on Lease/Get misses by calling buildEngine, which
// rehydrates history from the MessageStore. Call Close when done to stop the
// background gc goroutine.
type Pool struct {
	cfg          *config.Config
	messageStore store.MessageStore
	capacity     int
	idleTTL      time.Duration

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
		cfg:          cfg,
		messageStore: ms,
		capacity:     cap,
		idleTTL:      ttl,
		lruList:      list.New(),
		items:        make(map[entryKey]*list.Element),
		stopCh:       make(chan struct{}),
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
	e, err := p.getOrCreate(ctx, owner, sessionID)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	return e.eng, func() { e.mu.Unlock() }, nil
}

// Get returns the cached *engine.Engine for (owner, sessionID), building a fresh
// one via rehydration on a cache miss. It is safe for concurrent use.
//
// Deprecated: HTTP-path callers should use Lease to serialize per-session engine
// access. Get is retained for callers that do not require serialization (e.g.
// read-only inspection, tests).
func (p *Pool) Get(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error) {
	e, err := p.getOrCreate(ctx, owner, sessionID)
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
func (p *Pool) getOrCreate(ctx context.Context, owner store.Owner, sessionID string) (*entry, error) {
	k := entryKey{Owner: owner, SessionID: sessionID}

	// Fast path: cache hit.
	p.mu.Lock()
	if el, ok := p.items[k]; ok {
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
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
		return e, nil
	}

	// Evict LRU if at capacity.
	if len(p.items) >= p.capacity {
		p.evictLRULocked()
	}

	e := &entry{
		key:         k,
		eng:         eng,
		lastTouched: time.Now(),
	}
	el := p.lruList.PushFront(e)
	p.items[k] = el
	return e, nil
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
