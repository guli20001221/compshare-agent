package engine

import (
	"strings"
	"time"
)

const (
	ContinuityFreshnessFresh   = "fresh"
	ContinuityFreshnessStale   = "stale"
	ContinuityFreshnessExpired = "expired"

	maxContextValueRunes = 320
)

// SelectedEntityHint is identity-only execution context. It helps the model
// understand references but never bypasses existence checks, confirmation or policy.
type SelectedEntityHint struct {
	Kind      string `json:"kind,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Ordinal   int    `json:"ordinal,omitempty"`
	Source    string `json:"source,omitempty"`
	Freshness string `json:"freshness,omitempty"`
}

func continuityFreshness(at int64, ttl int, now time.Time) string {
	if at <= 0 || ttl <= 0 {
		return ContinuityFreshnessStale
	}
	age := now.Unix() - at
	if age < 0 {
		age = 0
	}
	if age > int64(ttl) {
		return ContinuityFreshnessExpired
	}
	if age > int64(ttl)/2 {
		return ContinuityFreshnessStale
	}
	return ContinuityFreshnessFresh
}

func normalizedSelectedInstanceFreshness(state SessionState) string {
	// A stamped user_selected row is an explicit conversation binding, not a
	// short-lived observation. Older binaries may have persisted it as expired;
	// normalize that wire value to stale so the same conversation can resume
	// without pretending the selection was just made. Unstamped legacy rows stay
	// expired because their provenance cannot be tied to the current state shape.
	if state.SelectedInstanceSource == SelectedInstanceSourceUser && state.SelectedInstanceAtUnix > 0 {
		if state.SelectedInstanceFreshness == "" || state.SelectedInstanceFreshness == ContinuityFreshnessFresh {
			return ContinuityFreshnessFresh
		}
		return ContinuityFreshnessStale
	}
	if state.SelectedInstanceFreshness != "" {
		return state.SelectedInstanceFreshness
	}
	if state.SelectedInstanceID == "" {
		return ""
	}
	if state.SelectedInstanceAtUnix <= 0 {
		// A legacy row with no timestamp has unknown freshness, but its referent
		// remains available in the conversation.
		return ContinuityFreshnessExpired
	}
	return ContinuityFreshnessFresh
}

func compactContextText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > maxContextValueRunes {
		value = string(runes[:maxContextValueRunes]) + "…"
	}
	return value
}
