package observability

// StateTrace records how the current-instance binding entered and left a turn.
// It is populated at Finish and omitted when no state signal was observed.
type StateTrace struct {
	// SessionStateHydrated is the denominator. It is false when session context
	// was not supplied or could not be parsed, so
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
	// single_host / unresolved. explicit_id / planner are RESERVED
	// (a mid-turn binding-source stamp is a follow-up — see the const block).
	ResolutionSource string `json:"resolution_source,omitempty"`
	// SelectedInstanceID is the bound instance at turn END (final), and the
	// *AtTurnStart fields capture the carried identity and provenance after
	// turn-entry expiry but before any mid-turn re-bind. An id alone cannot tell a
	// real missing-context failure from the intended "observed is not selected" or
	// "selection expired" safety gates.
	SelectedInstanceID                   string `json:"selected_instance_id,omitempty"`
	SelectedInstanceIDAtTurnStart        string `json:"selected_instance_id_at_turn_start,omitempty"`
	SelectedInstanceSource               string `json:"selected_instance_source,omitempty"`
	SelectedInstanceFreshness            string `json:"selected_instance_freshness,omitempty"`
	SelectedInstanceSourceAtTurnStart    string `json:"selected_instance_source_at_turn_start,omitempty"`
	SelectedInstanceFreshnessAtTurnStart string `json:"selected_instance_freshness_at_turn_start,omitempty"`
}

// ResolutionSource* are the StateTrace.ResolutionSource values — how the turn's
// "current instance" binding was determined at turn start.
//
// The turn-start derivation (refreshSystemPrompt) emits the first four; the
// priority is session_state > single_host > unresolved. explicit_id and planner
// are RESERVED schema values:
// distinguishing "user named an id this turn" / "planner resolved a ref mid-turn"
// from the carried turn-start binding is genuinely ambiguous without a deeper
// signal (the same tool that acts on a session-bound instance also fires the
// mid-turn record), so v1 folds those into the turn-start source rather than
// mis-stamp them. They are defined so the schema is complete and a later
// binding-source stamp can emit them without a schema change.
const (
	ResolutionSourceSessionState = "session_state"
	ResolutionSourceSingleHost   = "single_host"
	ResolutionSourceExplicitID   = "explicit_id"
	ResolutionSourcePlanner      = "planner"
	ResolutionSourceUnresolved   = "unresolved"
)

func traceStateObserved(t StateTrace) bool {
	return t.ResolutionSource != "" ||
		t.SelectedInstanceID != "" ||
		t.SelectedInstanceIDAtTurnStart != "" ||
		t.SelectedInstanceSource != "" ||
		t.SelectedInstanceFreshness != "" ||
		t.SelectedInstanceSourceAtTurnStart != "" ||
		t.SelectedInstanceFreshnessAtTurnStart != "" ||
		t.SessionStateHydrated
}
