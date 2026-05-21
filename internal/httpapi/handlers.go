package httpapi

import (
	"context"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// EnginePool abstracts per-session engine lifecycle so httpapi does not depend
// directly on the agentpool package. Task 7 wires the concrete *agentpool.Pool.
type EnginePool interface {
	// Lease returns the engine for (owner, sessionID), building one on a cache miss,
	// and holds the per-entry mutex until the caller invokes the returned release func.
	// HTTP-path callers MUST use Lease to serialize concurrent Chat calls on the same session.
	Lease(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, func(), error)

	// Get returns the engine without acquiring the per-entry serialization lock.
	// Retained for backward compatibility; prefer Lease in the HTTP path.
	Get(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error)
}

// Handlers holds the dependencies shared by all gateway Action handlers.
type Handlers struct {
	cfg      *config.Config
	sessions store.SessionStore
	messages store.MessageStore
	feedback store.FeedbackStore
	// pool may be nil for Task 6; Task 7 wires a concrete EnginePool.
	pool EnginePool
}

// NewHandlers constructs a Handlers with all dependencies injected.
// pool may be nil if Chat is not yet wired.
func NewHandlers(
	cfg *config.Config,
	sessions store.SessionStore,
	messages store.MessageStore,
	feedback store.FeedbackStore,
	pool EnginePool,
) *Handlers {
	return &Handlers{
		cfg:      cfg,
		sessions: sessions,
		messages: messages,
		feedback: feedback,
		pool:     pool,
	}
}
