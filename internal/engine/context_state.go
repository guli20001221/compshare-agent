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
// understand references but never bypasses binding, confirmation or policy.
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
	if state.SelectedInstanceFreshness != "" {
		return state.SelectedInstanceFreshness
	}
	if state.SelectedInstanceID == "" {
		return ""
	}
	if state.SelectedInstanceAtUnix <= 0 {
		// A legacy row with no timestamp has no bounded authorization window.
		// Treat it as expired: it may remain conversational provenance, but it
		// cannot silently bind an operation before a fresh confirmation.
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
