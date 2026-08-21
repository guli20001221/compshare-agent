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

// TranscriptPersistenceStats counts bounded metadata-write outcomes.
type TranscriptPersistenceStats struct {
	Attempted  int64
	Succeeded  int64
	NoStore    int64
	NoRowMatch int64
	Oversized  int64
	Invalid    int64
	WriteError int64
}

// Counters are process-wide and atomic because HTTP turns complete concurrently.
var transcriptPersistenceCounters struct {
	Attempted  atomic.Int64
	Succeeded  atomic.Int64
	NoStore    atomic.Int64
	NoRowMatch atomic.Int64
	Oversized  atomic.Int64
	Invalid    atomic.Int64
	WriteError atomic.Int64
}

// TranscriptPersistenceSnapshot returns a copy of the counters for diagnostics.
func TranscriptPersistenceSnapshot() TranscriptPersistenceStats {
	return TranscriptPersistenceStats{
		Attempted:  transcriptPersistenceCounters.Attempted.Load(),
		Succeeded:  transcriptPersistenceCounters.Succeeded.Load(),
		NoStore:    transcriptPersistenceCounters.NoStore.Load(),
		NoRowMatch: transcriptPersistenceCounters.NoRowMatch.Load(),
		Oversized:  transcriptPersistenceCounters.Oversized.Load(),
		Invalid:    transcriptPersistenceCounters.Invalid.Load(),
		WriteError: transcriptPersistenceCounters.WriteError.Load(),
	}
}

// Transcript metadata is useful for later replay but must not hold a completed
// reply open on a slow database.
const transcriptPersistTimeout = 3 * time.Second

// persistTurnTranscript writes the completed turn's canonical transcript to the
// assistant row. Failure is observable but never changes an already delivered
// turn's outcome.
func (h *Handlers) persistTurnTranscript(
	owner store.Owner,
	assistantMsgID string,
	agent turnTranscriptSource,
	replyPersistErr error,
) {
	if agent == nil || assistantMsgID == "" || replyPersistErr != nil {
		return
	}
	payload, stats := agent.LastTurnTranscript()
	if !stats.Oversized && !stats.Invalid && len(payload) == 0 {
		// Not an attempt: a turn with no tool traffic has nothing to record.
		return
	}

	// The increment's ticket makes concurrent milestone logging exact.
	ticket := transcriptPersistenceCounters.Attempted.Add(1)
	defer func() { reportTranscriptPersistence(ticket) }()

	switch {
	case stats.Oversized:
		transcriptPersistenceCounters.Oversized.Add(1)
		return
	case stats.Invalid:
		transcriptPersistenceCounters.Invalid.Add(1)
		return
	}

	metaStore, ok := h.messages.(store.AssistantMetadataStore)
	if !ok {
		transcriptPersistenceCounters.NoStore.Add(1)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), transcriptPersistTimeout)
	defer cancel()

	switch err := metaStore.UpdateAssistantMetadata(writeCtx, owner, assistantMsgID, payload); {
	case err == nil:
		transcriptPersistenceCounters.Succeeded.Add(1)
	case errors.Is(err, sql.ErrNoRows):
		transcriptPersistenceCounters.NoRowMatch.Add(1)
	default:
		transcriptPersistenceCounters.WriteError.Add(1)
		log.Printf("transcript persistence failed (turn unaffected): %v", err)
	}
}

const transcriptReportEvery = 100

func reportTranscriptPersistence(ticket int64) {
	if ticket%transcriptReportEvery != 0 {
		return
	}
	s := TranscriptPersistenceSnapshot()
	log.Printf(
		"transcript persistence: attempted=%d succeeded=%d no_store=%d no_row=%d oversized=%d invalid=%d write_error=%d",
		s.Attempted, s.Succeeded, s.NoStore, s.NoRowMatch, s.Oversized, s.Invalid, s.WriteError)
}
