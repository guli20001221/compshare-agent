package observability

// StateTrace records the per-turn "current instance" binding state (#3,
// instance-binding cluster). It closes the observability residual left after
// PR#300 fixed the binding bug itself: `grep SelectedInstance` over this package
// was 0 — the selected instance was a planner input, never a recorded field, so
// a wrong-instance answer could not be attributed after the fact.
//
// Populated by the recorders at Finish from engine getters (mirrors the 1a
// FinishSignals plumbing). The block is emitted only when traceStateObserved
// is true (any field set); a raw fixture that never ran the engine stays
// byte-identical (SHA-stable).
//
// SCOPE: every engine turn sets ResolutionSource (at minimum "unresolved" via
// refreshSystemPrompt), so the block is present on all real turns and absent on
// fixtures. The #3 fields are inert on the CLI (SessionStateHydrated is false —
// SetSessionState is HTTP-only), so these must be verified through the HTTP
// server (联调), not a CLI smoke.
type StateTrace struct {
	// SessionStateHydrated is the denominator. It is false on the CLI
	// (SetSessionState is HTTP-only) and on an HTTP context parse-failure, so
	// "ResolutionSource=unresolved" can be read correctly: an un-hydrated turn
	// could not have carried a binding, so its "unresolved" is expected, not a
	// bug. Intentionally NOT omitempty — when the State block is present at all
	// it must carry the hydrated flag even when false, or the denominator is
	// ambiguous (absent could mean either false or "field not written").
	SessionStateHydrated bool `json:"session_state_hydrated"`
	// ResolutionSource is how the turn's current-instance binding was determined
	// at turn start. Set on every engine turn (refreshSystemPrompt), so its
	// non-empty value is what makes the State block observed. Values are the
	// ResolutionSource* consts; the turn-start derivation emits session_state /
	// single_host / fact_cache / unresolved. explicit_id / planner are RESERVED
	// (a mid-turn binding-source stamp is a follow-up — see the const block).
	ResolutionSource string `json:"resolution_source,omitempty"`
	// SelectedInstanceID is the bound instance at turn END (final), and
	// SelectedInstanceIDAtTurnStart is the value carried in at turn entry. A
	// divergence between the two means the turn re-bound the instance mid-flow.
	SelectedInstanceID            string `json:"selected_instance_id,omitempty"`
	SelectedInstanceIDAtTurnStart string `json:"selected_instance_id_at_turn_start,omitempty"`
	// FactCacheOldestAgeBucket buckets the age of the oldest still-fresh fact in
	// the near-term cache (TTL 300s) — the only stale-cache observable. Bucketed
	// at the source (not raw seconds) per the redaction closure, so promotion to
	// a metric label never carries unbounded cardinality. Empty when no fresh
	// fact was injected this turn.
	FactCacheOldestAgeBucket string `json:"fact_cache_oldest_age_bucket,omitempty"`
}

// ResolutionSource* are the StateTrace.ResolutionSource values — how the turn's
// "current instance" binding was determined at turn start.
//
// The turn-start derivation (refreshSystemPrompt) emits the first four; the
// priority is session_state > single_host > fact_cache > unresolved (an explicit
// prior selection is the strongest binding, the single-host shortcut next, the
// near-term cache weakest). explicit_id and planner are RESERVED schema values:
// distinguishing "user named an id this turn" / "planner resolved a ref mid-turn"
// from the carried turn-start binding is genuinely ambiguous without a deeper
// signal (the same tool that acts on a session-bound instance also fires the
// mid-turn record), so v1 folds those into the turn-start source rather than
// mis-stamp them. They are defined so the schema is complete and a later
// binding-source stamp can emit them without a schema change.
const (
	ResolutionSourceSessionState = "session_state"
	ResolutionSourceSingleHost   = "single_host"
	ResolutionSourceFactCache    = "fact_cache"
	ResolutionSourceExplicitID   = "explicit_id"
	ResolutionSourcePlanner      = "planner"
	ResolutionSourceUnresolved   = "unresolved"
)

// BucketFactCacheAge maps the oldest-fresh-fact age (seconds, bounded by the
// 300s fact TTL) to a bounded staleness bucket. A negative age (no fresh fact)
// → "" so the field is omitted. Boundaries gradient toward the TTL because the
// stale-answer risk grows as a fact approaches expiry.
func BucketFactCacheAge(seconds int) string {
	switch {
	case seconds < 0:
		return ""
	case seconds <= 60:
		return "le_60s"
	case seconds <= 180:
		return "le_180s"
	default:
		return "le_300s"
	}
}

func traceStateObserved(t StateTrace) bool {
	return t.ResolutionSource != "" ||
		t.SelectedInstanceID != "" ||
		t.SelectedInstanceIDAtTurnStart != "" ||
		t.FactCacheOldestAgeBucket != "" ||
		t.SessionStateHydrated
}
