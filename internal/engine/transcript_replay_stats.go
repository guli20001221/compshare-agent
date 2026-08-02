package engine

import (
	"log"
	"sync/atomic"
)

// Read-side observability for the canonical transcript.
//
// The write side has counted its outcomes since it was written
// (internal/httpapi/transcript_shadow.go). The read side had nothing: a pilot
// could write rows and replay them while nobody could tell whether a transcript
// ever reached the model, whether a persisted row was readable, or whether the
// projector's last-line orphan drop was firing. "Is the rollout working?" was
// answerable for the write and unanswerable for the read, which is the half that
// changes what the model sees.
//
// These counters are process-global for the same reason the write-side ones are:
// the question is aggregate, not per-session. They are atomic because the HTTP
// handler is concurrent — a plain ++ here is a data race on every simultaneous
// turn, and a racing counter misreports exactly the number it exists to provide.
//
// Every increment sits behind canonicalTranscriptEnabled, so with the flag off
// this file costs one already-evaluated boolean and touches nothing. That is not
// incidental: PR #496 removed a half-enabled state in which the transcript ran
// unconditionally and only projection was gated, and instrumentation that ran
// regardless would put a smaller version of it straight back.

// TranscriptReplayStats is a snapshot of the read side.
//
// The two groups have different denominators and must not be divided into each
// other. The replay group counts PROJECTIONS: a context is rebuilt on every
// turn, so one persisted row is re-projected many times over a session. The row
// group counts REHYDRATIONS: once per cold load per row.
type TranscriptReplayStats struct {
	// ContextBuilds is how many times a context rebuild reached the attach step
	// with the transcript enabled. It is the milestone ticket source and the
	// denominator for the replay group.
	ContextBuilds int64
	// PairsReplayed is the exchanges offered to the attach step.
	PairsReplayed int64
	// TranscriptsAttached is the exchanges that came away carrying tool work.
	// Zero here while rows are being written is the signal that the read side is
	// not consuming what the write side produced.
	TranscriptsAttached int64
	// MatchMissed is exchanges the attach step could not attribute to any
	// recorded turn. Expected to be 0: the pair list and the record list are both
	// derived from e.messages, so a miss means the two rebuild paths diverged —
	// which is precisely the failure hot/cold parity tests cannot see, because
	// both sides of a parity test make the same substitution and agree.
	MatchMissed int64
	// MessagesProjected is chat messages emitted by ProjectTranscript.
	MessagesProjected int64
	// ToolCallsDropped counts tool_calls the projector refused to replay because
	// nothing answered them, or because their arguments would not parse. The
	// projector's own comment calls this "the last line, not the first" — bounding
	// sheds whole rounds so it should never fire. Nothing counted whether it did.
	ToolCallsDropped int64
	// BudgetDropped is exchanges CARRYING a transcript that budgetReplayedPairs
	// dropped for size. Distinct from ordinary budget pressure: it is the case
	// where the tool evidence is what pushed the exchange out.
	BudgetDropped int64

	// Row group — one count per persisted row per rehydration.
	RowsParsed int64
	// RowsWithoutTranscript is the ordinary case during a pilot: a row written
	// before the flag, or by a writer that owns other metadata keys. It is
	// counted because an operator seeing TranscriptsAttached=0 must be able to
	// tell "no row carries one yet" from "rows carry one and we cannot read it".
	RowsWithoutTranscript int64
	// RowsForeignVersion is a row carrying agent_transcript_vN this binary does
	// not read. ParseTranscriptMetadata degrades to nil here deliberately and
	// silently; silence is right for the turn and wrong for the rollout.
	RowsForeignVersion int64
	// RowsUnreadable is metadata that is not decodable JSON at all.
	RowsUnreadable int64
	// RowsEmptyTranscript is a well-formed envelope carrying zero messages.
	RowsEmptyTranscript int64
	// RowsIllegalStructure is a row this binary refuses to replay: a role outside
	// user/assistant/tool, tool_calls on a non-assistant message, or a call
	// missing its id or name. The producer cannot emit any of these, so a
	// non-zero value means rows are arriving from somewhere else — which is the
	// only counter here that would justify turning the flag back off.
	RowsIllegalStructure int64
}

var transcriptReplayCounters struct {
	ContextBuilds       atomic.Int64
	PairsReplayed       atomic.Int64
	TranscriptsAttached atomic.Int64
	MatchMissed         atomic.Int64
	MessagesProjected   atomic.Int64
	ToolCallsDropped    atomic.Int64
	BudgetDropped       atomic.Int64

	RowsParsed            atomic.Int64
	RowsWithoutTranscript atomic.Int64
	RowsForeignVersion    atomic.Int64
	RowsUnreadable        atomic.Int64
	RowsEmptyTranscript   atomic.Int64
	RowsIllegalStructure  atomic.Int64
}

// TranscriptReplaySnapshot returns a copy of the read-side counters.
func TranscriptReplaySnapshot() TranscriptReplayStats {
	return TranscriptReplayStats{
		ContextBuilds:       transcriptReplayCounters.ContextBuilds.Load(),
		PairsReplayed:       transcriptReplayCounters.PairsReplayed.Load(),
		TranscriptsAttached: transcriptReplayCounters.TranscriptsAttached.Load(),
		MatchMissed:         transcriptReplayCounters.MatchMissed.Load(),
		MessagesProjected:   transcriptReplayCounters.MessagesProjected.Load(),
		ToolCallsDropped:    transcriptReplayCounters.ToolCallsDropped.Load(),
		BudgetDropped:       transcriptReplayCounters.BudgetDropped.Load(),

		RowsParsed:            transcriptReplayCounters.RowsParsed.Load(),
		RowsWithoutTranscript: transcriptReplayCounters.RowsWithoutTranscript.Load(),
		RowsForeignVersion:    transcriptReplayCounters.RowsForeignVersion.Load(),
		RowsUnreadable:        transcriptReplayCounters.RowsUnreadable.Load(),
		RowsEmptyTranscript:   transcriptReplayCounters.RowsEmptyTranscript.Load(),
		RowsIllegalStructure:  transcriptReplayCounters.RowsIllegalStructure.Load(),
	}
}

// recordTranscriptRowOutcome counts one rehydrated row's parse result. The
// caller has already checked the flag — this runs only from transcriptFromRow,
// which returns before reaching here when the transcript is off.
func recordTranscriptRowOutcome(outcome transcriptParseOutcome) {
	switch outcome {
	case transcriptParseOK:
		transcriptReplayCounters.RowsParsed.Add(1)
	case transcriptParseNoTranscript:
		transcriptReplayCounters.RowsWithoutTranscript.Add(1)
	case transcriptParseForeignVersion:
		transcriptReplayCounters.RowsForeignVersion.Add(1)
	case transcriptParseUnreadable:
		transcriptReplayCounters.RowsUnreadable.Add(1)
	case transcriptParseEmpty:
		transcriptReplayCounters.RowsEmptyTranscript.Add(1)
	case transcriptParseIllegalStructure:
		transcriptReplayCounters.RowsIllegalStructure.Add(1)
	}
	// transcriptParseAbsent is deliberately uncounted: a row with no metadata at
	// all is every row the service has ever written before this flag, and
	// counting it would bury the four outcomes above under it.
}

// replayReportEvery is how often the aggregate is logged. Same reasoning as the
// write side: counters whose only reader is a test cannot answer "is the pilot
// working?", and the alternative is querying the database by hand.
const replayReportEvery = 100

// reportReplayProgress emits the aggregate on the context build that owns the
// milestone. It takes the ticket rather than re-reading the counter for the same
// reason the write side does: concurrent turns would otherwise both observe the
// same total and log twice, or step past a multiple and never log.
func reportReplayProgress(ticket int64) {
	if ticket%replayReportEvery != 0 {
		return
	}
	s := TranscriptReplaySnapshot()
	log.Printf(
		"transcript replay: contexts=%d pairs=%d attached=%d match_missed=%d messages=%d tool_calls_dropped=%d budget_dropped=%d | rows ok=%d none=%d foreign_version=%d unreadable=%d empty=%d illegal_structure=%d",
		s.ContextBuilds, s.PairsReplayed, s.TranscriptsAttached, s.MatchMissed,
		s.MessagesProjected, s.ToolCallsDropped, s.BudgetDropped,
		s.RowsParsed, s.RowsWithoutTranscript, s.RowsForeignVersion,
		s.RowsUnreadable, s.RowsEmptyTranscript, s.RowsIllegalStructure)
}
