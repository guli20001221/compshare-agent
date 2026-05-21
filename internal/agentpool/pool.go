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

// entry is one node in the LRU linked list.
type entry struct {
	sessionID   string
	eng         *engine.Engine
	lastTouched time.Time
}

// Pool is a concurrency-safe LRU cache of *engine.Engine keyed by session ID.
// Entries are created lazily on Get misses by calling buildEngine, which
// rehydrates history from the MessageStore. Call Close when done to stop the
// background gc goroutine.
type Pool struct {
	cfg          *config.Config
	messageStore store.MessageStore
	capacity     int
	idleTTL      time.Duration

	mu      sync.Mutex
	lruList *list.List               // front = most recently used
	items   map[string]*list.Element // sessionID → list.Element(*entry)

	stopCh chan struct{}
	wg     sync.WaitGroup
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
		items:        make(map[string]*list.Element),
		stopCh:       make(chan struct{}),
	}

	p.wg.Add(1)
	go p.gcLoop(gcTick)
	return p
}

// Get returns the cached *engine.Engine for sessionID, building a fresh one via
// rehydration on a cache miss. It is safe for concurrent use.
//
// Concurrency design: the lock is released during the potentially-slow
// buildEngine call. After buildEngine returns we re-acquire the lock and
// re-check whether another goroutine raced us to insert the same session; if
// so, we discard the duplicate and return the winner already in the cache.
func (p *Pool) Get(ctx context.Context, sessionID string) (*engine.Engine, error) {
	// Fast path: cache hit.
	p.mu.Lock()
	if el, ok := p.items[sessionID]; ok {
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
		eng := e.eng
		p.mu.Unlock()
		return eng, nil
	}
	p.mu.Unlock()

	// Slow path: build a new engine outside the lock.
	eng, err := p.buildEngine(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Re-acquire lock and insert (checking for a concurrent insert).
	p.mu.Lock()
	defer p.mu.Unlock()

	if el, ok := p.items[sessionID]; ok {
		// Another goroutine already inserted while we were building; use theirs.
		e := el.Value.(*entry)
		e.lastTouched = time.Now()
		p.lruList.MoveToFront(el)
		return e.eng, nil
	}

	// Evict LRU if at capacity.
	if len(p.items) >= p.capacity {
		p.evictLRULocked()
	}

	e := &entry{
		sessionID:   sessionID,
		eng:         eng,
		lastTouched: time.Now(),
	}
	el := p.lruList.PushFront(e)
	p.items[sessionID] = el
	return eng, nil
}

// Close stops the background gc goroutine and waits for it to exit.
func (p *Pool) Close() {
	close(p.stopCh)
	p.wg.Wait()
}

// gcLoop periodically evicts entries that have been idle longer than p.idleTTL.
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
		delete(p.items, e.sessionID)
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
	delete(p.items, e.sessionID)
}
