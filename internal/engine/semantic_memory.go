package engine

import (
	"strings"
	"time"
)

const (
	ContinuityFreshnessFresh   = "fresh"
	ContinuityFreshnessStale   = "stale"
	ContinuityFreshnessExpired = "expired"

	ToolFactSourceTool             = "tool"
	ToolFactCompletenessProjection = "semantic_projection"

	maxSemanticItems  = 12
	maxSemanticRunes  = 320
	maxNarrativeRunes = 1200
)

// SemanticEntityHint is identity-only conversational context. A fresh,
// unambiguous entity may become a Resolver candidate, but it never bypasses the
// normal confirmation, sealing, permission or journal gates.
type SemanticEntityHint struct {
	Kind      string `json:"kind,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Ordinal   int    `json:"ordinal,omitempty"`
	Source    string `json:"source,omitempty"`
	Freshness string `json:"freshness,omitempty"`
}

// ContinuityAdvisories is an ephemeral coordinator-to-engine view. It MUST NOT
// be embedded in SessionState: durable turn/action truth lives in turn tables.
type ContinuityAdvisories struct {
	ReadOnly bool
	Notices  []string
}

func (e *Engine) SetContinuityAdvisories(in ContinuityAdvisories) {
	if e == nil {
		return
	}
	e.continuityAdvisories = ContinuityAdvisories{
		ReadOnly: in.ReadOnly,
		Notices:  append([]string(nil), in.Notices...),
	}
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

func effectiveContextFrameTTL(frame ContextFrame) int {
	if frame.TTLSeconds > 0 {
		return frame.TTLSeconds
	}
	return ContextFrameTTLSeconds
}

func (e *Engine) expireStaleToolFacts(now time.Time) {
	if e == nil || !e.sessionStateHydrated {
		return
	}
	changed := false
	for i := range e.sessionState.RecentFacts {
		fact := &e.sessionState.RecentFacts[i]
		if fact.Source == "" {
			fact.Source = ToolFactSourceTool
			changed = true
		}
		if fact.Completeness == "" {
			fact.Completeness = ToolFactCompletenessProjection
			changed = true
		}
		freshness := continuityFreshness(fact.ProducedAtUnix, fact.TTLSeconds, now)
		if fact.Freshness != freshness {
			fact.Freshness = freshness
			changed = true
		}
		mustRefresh := freshness == ContinuityFreshnessExpired
		if fact.RefreshRequired != mustRefresh {
			fact.RefreshRequired = mustRefresh
			changed = true
		}
		if mustRefresh && len(fact.Payload) > 0 {
			// Expired observations keep only topic and observation time.
			fact.Payload = nil
			changed = true
		}
	}
	if changed {
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
	}
}

func normalizeToolFactForStore(fact ToolFact) ToolFact {
	if fact.Source == "" {
		fact.Source = ToolFactSourceTool
	}
	if fact.Completeness == "" {
		fact.Completeness = ToolFactCompletenessProjection
	}
	if fact.Freshness == "" {
		fact.Freshness = ContinuityFreshnessFresh
	}
	if fact.Freshness != ContinuityFreshnessExpired {
		fact.RefreshRequired = false
	}
	return fact
}

func normalizedSelectedInstanceFreshness(state SessionState) string {
	if state.SelectedInstanceFreshness != "" {
		return state.SelectedInstanceFreshness
	}
	if state.SelectedInstanceID == "" {
		return ""
	}
	if state.SelectedInstanceAtUnix <= 0 {
		return ContinuityFreshnessStale
	}
	return ContinuityFreshnessFresh
}

func compactSemanticText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > maxSemanticRunes {
		value = string(runes[:maxSemanticRunes]) + "…"
	}
	return value
}

func compactSemanticNarrative(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > maxNarrativeRunes {
		value = string(runes[:maxNarrativeRunes]) + "…"
	}
	return value
}

func compactSemanticItems(values []string) []string {
	return mergeSemanticItems(nil, values)
}

func mergeSemanticItems(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	appendOne := func(value string) {
		value = compactSemanticText(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	for _, value := range existing {
		appendOne(value)
	}
	for _, value := range incoming {
		appendOne(value)
	}
	if len(out) > maxSemanticItems {
		out = out[len(out)-maxSemanticItems:]
	}
	return out
}
