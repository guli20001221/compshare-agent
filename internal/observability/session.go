package observability

// SessionTrace records whether the agent could have had the context at all.
//
// Every other block in this package describes what the engine DID with the
// context it was handed — which instance it bound (StateTrace), how many
// messages the router and the loop saw (ContextTrace), what the retriever
// returned (RetrievalTrace). None of them can distinguish the two questions a
// production "the agent forgot" report actually poses:
//
//	(a) the agent was given the conversation and failed to use it, or
//	(b) the agent was never given the conversation.
//
// Today (b) is unattributable. The HTTP layer silently mints a fresh, EMPTY
// session when the client's SessionId is unknown / expired / deleted / owned by
// another tenant (handlers_session.go) while the front end keeps rendering the
// old transcript; the pool evicts at 200 sessions or 30 minutes idle and the
// cold rebuild restores only persisted user/assistant text — tool results,
// retrieved evidence and workflow steps were never persisted; and the service
// sends `done` BEFORE it saves the turn's state, so a fast follow-up can read a
// stale copy and a CAS conflict overwrites rather than merges. Each of those
// produces a turn where the agent is behaving correctly on the context it has,
// and the context is simply not the user's.
//
// Two independent audits of this repo stopped at the same wall: the production
// export carries no session continuity, no turn identity and no state version,
// so the share of production amnesia attributable to each layer cannot be
// measured — only argued. This block is what ends the argument. It changes no
// behavior; it only makes the failure visible.
//
// Redaction: a session id is a customer identifier and never enters a trace in
// the clear (same rule as UserMsgHash). Compare the HASHES, and read Swapped —
// never infer a swap from two hashes differing, because an absent field means
// "not recorded", never "no swap" (attribution-observable-only).
type SessionTrace struct {
	// RequestedSessionIDHash is the SessionId the CLIENT sent; SessionIDHash is
	// the one the turn actually ran on. They differ exactly when the backend
	// substituted a session the user cannot see.
	RequestedSessionIDHash string `json:"requested_session_id_hash,omitempty"`
	SessionIDHash          string `json:"session_id_hash,omitempty"`
	// Swapped is the single field that separates "the agent forgot" from "the
	// agent was handed a blank session". NOT omitempty: when the block is present
	// it must carry the flag even when false, or false and "not written" are
	// indistinguishable and the denominator is lost.
	Swapped bool `json:"swapped"`
	// SwapReason is why (SessionSwap* consts). Empty when Swapped is false.
	SwapReason string `json:"swap_reason,omitempty"`

	// TurnIndexInSession and MaxSessionTurns bound the session's remaining life.
	// Production caps a session at MaxSessionTurns QA pairs and then refuses,
	// with no handoff of goal / constraints / selected instance / pending task —
	// so the turns approaching the cap are where a user is most likely to be cut
	// off mid-task, and a turn AT the cap is amnesia by design, not by defect.
	TurnIndexInSession int `json:"turn_index_in_session,omitempty"`
	MaxSessionTurns    int `json:"max_session_turns,omitempty"`

	// RehydrateSource is where this turn's engine came from (RehydrateSource*
	// consts). A cold rebuild is not equivalent to a hot pool hit: only persisted
	// user/assistant text is restored, so the tool results and retrieved evidence
	// a follow-up refers to are gone even though the transcript looks complete.
	// RehydratedMessageCount is what the rebuild actually restored.
	RehydrateSource        string `json:"rehydrate_source,omitempty"`
	RehydratedMessageCount int    `json:"rehydrated_message_count,omitempty"`
	// BuildRaced records that this turn LOST a concurrent build race: it missed the pool,
	// built an engine, and then discarded it because another request for the same session had
	// inserted one first — so the turn ran on the WINNER's freshly-built engine.
	//
	// RehydrateSource still says "cold" for that turn, and correctly: the engine it ran on was
	// rebuilt from the DB and carries no tool results. But "cold because the pool evicted us"
	// and "cold because two requests hit the same session at once" are DIFFERENT causes with
	// different fixes, and without this flag they are one number. A burst of concurrent traffic
	// would read as a capacity problem. Recording the race is what keeps the cold count from
	// silently absorbing it.
	BuildRaced bool `json:"build_raced,omitempty"`

	// ContextLoadFailure records that the session's persisted context could NOT be loaded, and
	// why (ContextLoad* consts). Empty when it loaded.
	//
	// This is a context-loss event in its own right and it was invisible: on a parse failure
	// the chat path logs a warning, never calls SetSessionState, and runs the turn anyway — so
	// the agent silently loses its selected instance, its last intent and any pending workflow
	// frame, and every downstream trace field looks like a session that simply had no state.
	// A turn that lost its state and a turn that never had any are not the same event, and the
	// whole point of this block is that they stop being one.
	ContextLoadFailure string `json:"context_load_failure,omitempty"`

	// ContextVersionIn is the SessionState version this turn READ, ContextVersionOut
	// the version it WROTE. A turn whose In is older than the previous turn's Out
	// read a stale copy — the read-before-lock / done-before-save race. CASConflict
	// records that the write collided; today the retry overwrites the winner's state
	// wholesale instead of merging, so a conflict is a silent context loss, not a
	// retry.
	//
	// NOT omitempty, unlike every other numeric field here: 0 is a LEGAL value on
	// both. A brand-new session reads version 0, and a turn whose save failed wrote
	// version 0. Under omitempty those marshal as absent — indistinguishable from
	// "not observed", which is exactly the ambiguity this block exists to remove.
	// The whole block is gated by traceSessionObserved, so when these appear at all
	// they were observed (same reasoning as Swapped).
	ContextVersionIn  int  `json:"context_version_in"`
	ContextVersionOut int  `json:"context_version_out"`
	CASConflict       bool `json:"cas_conflict,omitempty"`
	// StateSaveFailed records that the turn's assistant reply or SessionState did
	// NOT persist. Today that path logs a warning and still streams `done`, so the
	// user is told the turn succeeded while the next turn will not see it.
	StateSaveFailed bool `json:"state_save_failed,omitempty"`
}

// SessionSwap* are the SessionTrace.SwapReason values — why the backend served a
// different session than the client asked for. Each is a distinct product bug
// with a distinct fix, so they are not collapsed into a single "invalid".
const (
	SessionSwapNotFound  = "not_found"  // id unknown to the store
	SessionSwapForeign   = "foreign"    // id exists but belongs to another tenant
	SessionSwapExpired   = "expired"    // id existed and was reaped
	SessionSwapMalformed = "malformed"  // id failed validation
	SessionSwapAbsent    = "absent"     // client sent none; a fresh session is correct, not a defect
)

// RehydrateSource* are the SessionTrace.RehydrateSource values.
const (
	RehydrateSourceHot  = "hot"  // engine served from the live pool; full in-memory history intact
	RehydrateSourceCold = "cold" // engine rebuilt from the DB; tool results / evidence NOT restored
	RehydrateSourceNew  = "new"  // first turn of a brand-new session; nothing to restore
)

// ContextLoad* are the SessionTrace.ContextLoadFailure values — why the session's persisted
// state could not be loaded for this turn. They are distinct because they need opposite fixes:
// malformed is data we corrupted, unknown_schema is a forward-rollout condition where an older
// binary must leave a newer binary's row alone.
const (
	ContextLoadMalformed     = "malformed"      // sessions.context did not parse
	ContextLoadUnknownSchema = "unknown_schema" // parsed, but schema_version is from a newer binary
)

func traceSessionObserved(t SessionTrace) bool {
	return t.RequestedSessionIDHash != "" ||
		t.SessionIDHash != "" ||
		t.Swapped ||
		t.SwapReason != "" ||
		t.TurnIndexInSession > 0 ||
		t.MaxSessionTurns > 0 ||
		t.RehydrateSource != "" ||
		t.RehydratedMessageCount > 0 ||
		t.ContextVersionIn > 0 ||
		t.ContextVersionOut > 0 ||
		t.CASConflict ||
		t.StateSaveFailed ||
		t.BuildRaced ||
		t.ContextLoadFailure != ""
}
