package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// turnTranscriptSource is the engine's producer side, taken as an interface so
// this path does not depend on the engine's internals and can be exercised
// without constructing one. *engine.Engine satisfies it.
type turnTranscriptSource interface {
	LastTurnTranscript() (json.RawMessage, engine.TranscriptStats)
}

// TranscriptShadowStats counts shadow-write outcomes across the process. It
// exists so a rollout can answer "is this actually writing?" without reading
// rows, and can tell the four failure shapes apart — a single success counter
// would report a store that silently never writes as healthy.
type TranscriptShadowStats struct {
	Attempted  int64
	Succeeded  int64
	NoStore    int64
	NoRowMatch int64
	Oversized  int64
	Invalid    int64
	WriteError int64
}

// transcriptShadowCounters is process-global by design: the shadow write has no
// per-session meaning and the rollout question is aggregate. The fields are
// atomic because the HTTP handler is concurrent — plain ++ here is a data race
// on every simultaneous turn, and a racing counter would misreport exactly the
// rollout number it exists to provide.
var transcriptShadowCounters struct {
	Attempted  atomic.Int64
	Succeeded  atomic.Int64
	NoStore    atomic.Int64
	NoRowMatch atomic.Int64
	Oversized  atomic.Int64
	Invalid    atomic.Int64
	WriteError atomic.Int64
}

// TranscriptShadowSnapshot returns a copy of the counters for diagnostics.
func TranscriptShadowSnapshot() TranscriptShadowStats {
	return TranscriptShadowStats{
		Attempted:  transcriptShadowCounters.Attempted.Load(),
		Succeeded:  transcriptShadowCounters.Succeeded.Load(),
		NoStore:    transcriptShadowCounters.NoStore.Load(),
		NoRowMatch: transcriptShadowCounters.NoRowMatch.Load(),
		Oversized:  transcriptShadowCounters.Oversized.Load(),
		Invalid:    transcriptShadowCounters.Invalid.Load(),
		WriteError: transcriptShadowCounters.WriteError.Load(),
	}
}

// shadowPersistTimeout bounds the extra statement. Errors here were always
// swallowed, but latency was not: an unbounded context on a blocked database
// held the turn open indefinitely. The turn owes this write nothing, so it
// waits a bounded moment and gives up.
const shadowPersistTimeout = 3 * time.Second

// shadowPersistTranscript writes the just-finished turn's canonical transcript
// to messages.metadata on the assistant row.
//
// Every failure here is swallowed. That is the point: the transcript is a
// migration carrier being proven out, so its write must be incapable of
// changing a turn's outcome. It runs only after the reply row is durable, uses
// its own statement (see store.AssistantMetadataStore), and reports through
// counters rather than errors.
//
// It also does not touch the durable turn path. CommitTurn writes the assistant
// row atomically and hashCommit fingerprints that write; adding metadata to the
// hash without adding it to the commit would make the fingerprint attest to a
// column the row does not carry, turning a consistency check into a false
// proof. Durable parity is a deliberate, separate change.
func (h *Handlers) shadowPersistTranscript(
	owner store.Owner,
	assistantMsgID string,
	agent turnTranscriptSource,
	replyPersistErr error,
) {
	if agent == nil || assistantMsgID == "" || replyPersistErr != nil {
		return
	}
	payload, stats := agent.LastTurnTranscript()
	switch {
	case stats.Oversized:
		transcriptShadowCounters.Attempted.Add(1)
		transcriptShadowCounters.Oversized.Add(1)
		return
	case stats.Invalid:
		transcriptShadowCounters.Attempted.Add(1)
		transcriptShadowCounters.Invalid.Add(1)
		return
	case len(payload) == 0:
		// Not an attempt: a turn with no tool traffic has nothing to record.
		return
	}

	transcriptShadowCounters.Attempted.Add(1)
	metaStore, ok := h.messages.(store.AssistantMetadataStore)
	if !ok {
		transcriptShadowCounters.NoStore.Add(1)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), shadowPersistTimeout)
	defer cancel()

	switch err := metaStore.UpdateAssistantMetadata(writeCtx, owner, assistantMsgID, payload); {
	case err == nil:
		transcriptShadowCounters.Succeeded.Add(1)
	case errors.Is(err, sql.ErrNoRows):
		transcriptShadowCounters.NoRowMatch.Add(1)
	default:
		transcriptShadowCounters.WriteError.Add(1)
		log.Printf("transcript shadow write failed (turn unaffected): %v", err)
	}
}
