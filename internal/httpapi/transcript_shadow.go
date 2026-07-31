package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"

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

// transcriptShadowStats is process-global by design: the shadow write has no
// per-session meaning and the rollout question is aggregate.
var transcriptShadowStats TranscriptShadowStats

// TranscriptShadowSnapshot returns a copy of the counters for diagnostics.
func TranscriptShadowSnapshot() TranscriptShadowStats { return transcriptShadowStats }

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
		transcriptShadowStats.Attempted++
		transcriptShadowStats.Oversized++
		return
	case stats.Invalid:
		transcriptShadowStats.Attempted++
		transcriptShadowStats.Invalid++
		return
	case len(payload) == 0:
		// Not an attempt: a turn with no tool traffic has nothing to record.
		return
	}

	transcriptShadowStats.Attempted++
	metaStore, ok := h.messages.(store.AssistantMetadataStore)
	if !ok {
		transcriptShadowStats.NoStore++
		return
	}
	switch err := metaStore.UpdateAssistantMetadata(context.Background(), owner, assistantMsgID, payload); {
	case err == nil:
		transcriptShadowStats.Succeeded++
	case errors.Is(err, sql.ErrNoRows):
		transcriptShadowStats.NoRowMatch++
	default:
		transcriptShadowStats.WriteError++
		log.Printf("transcript shadow write failed (turn unaffected): %v", err)
	}
}
