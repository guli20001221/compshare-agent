package engine

import (
	"sort"
	"strings"
	"time"
)

const (
	ContinuityFreshnessFresh   = "fresh"
	ContinuityFreshnessStale   = "stale"
	ContinuityFreshnessExpired = "expired"

	TaskSnapshotStatusActive   = "active"
	TaskSnapshotStatusExpired  = "expired"
	TaskSnapshotStatusResolved = "resolved"

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

// TaskSnapshot is a durable semantic projection, never execution authority.
type TaskSnapshot struct {
	Goal          string               `json:"goal,omitempty"`
	Intent        string               `json:"intent,omitempty"`
	Workflow      string               `json:"workflow,omitempty"`
	Stage         string               `json:"stage,omitempty"`
	Constraints   []string             `json:"constraints,omitempty"`
	Decisions     []string             `json:"decisions,omitempty"`
	MissingSlots  []string             `json:"missing_slots,omitempty"`
	Entities      []SemanticEntityHint `json:"entities,omitempty"`
	Status        string               `json:"status,omitempty"`
	Freshness     string               `json:"freshness,omitempty"`
	EndReason     string               `json:"end_reason,omitempty"`
	UpdatedAtUnix int64                `json:"updated_at_unix,omitempty"`
}

// ConversationDigest is reference-only memory for early conversation
// semantics. It stores no model messages or raw tool JSON.
type ConversationDigest struct {
	Narrative       string                `json:"narrative,omitempty"`
	Goals           []string              `json:"goals,omitempty"`
	Constraints     []string              `json:"constraints,omitempty"`
	Decisions       []string              `json:"decisions,omitempty"`
	UnresolvedTasks []string              `json:"unresolved_tasks,omitempty"`
	EntityHints     []SemanticEntityHint  `json:"entity_hints,omitempty"`
	Sources         MemoryDelta           `json:"sources,omitempty"`
	Excerpts        []ConversationExcerpt `json:"excerpts,omitempty"`
	SummaryFrontier int64                 `json:"summary_frontier,omitempty"`
	UpdatedAtUnix   int64                 `json:"updated_at_unix,omitempty"`
}

// SourcedMemory is accepted only when Quote occurs verbatim in PairIndex.
// Value is the compact semantic projection shown to later turns.
type SourcedMemory struct {
	Value     string `json:"value,omitempty"`
	PairIndex int    `json:"pair_index"`
	Quote     string `json:"quote,omitempty"`
}

type MemoryDelta struct {
	Goals           []SourcedMemory `json:"goals,omitempty"`
	Constraints     []SourcedMemory `json:"constraints,omitempty"`
	Decisions       []SourcedMemory `json:"decisions,omitempty"`
	UnresolvedTasks []SourcedMemory `json:"unresolved_tasks,omitempty"`
}

// ConversationExcerpt is an unlabelled fallback. It preserves what was said
// without guessing whether the text was a goal, decision, or constraint.
type ConversationExcerpt struct {
	User      string `json:"user,omitempty"`
	Assistant string `json:"assistant,omitempty"`
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

func (e *Engine) syncTaskSnapshotFromFrame(frame ContextFrame, status, endReason string, now time.Time) {
	if e == nil || !e.sessionStateHydrated || contextFrameEmpty(frame) {
		return
	}
	freshness := frame.Freshness
	if freshness == "" {
		freshness = continuityFreshness(frame.ProducedAtUnix, effectiveContextFrameTTL(frame), now)
	}
	if status == "" {
		status = TaskSnapshotStatusActive
		if freshness == ContinuityFreshnessExpired {
			status = TaskSnapshotStatusExpired
		}
	}
	snapshot := TaskSnapshot{
		Goal:          compactSemanticText(frame.OriginalUserMsg),
		Intent:        compactSemanticText(frame.Intent),
		Workflow:      compactSemanticText(frame.Workflow),
		Stage:         compactSemanticText(frame.Stage),
		Constraints:   contextFrameConstraints(frame),
		Decisions:     contextFrameDecisions(frame),
		MissingSlots:  compactSemanticItems(frame.MissingSlots),
		Entities:      contextFrameEntities(frame, freshness),
		Status:        status,
		Freshness:     freshness,
		EndReason:     compactSemanticText(endReason),
		UpdatedAtUnix: now.Unix(),
	}
	if snapshot.Goal == "" {
		snapshot.Goal = firstNonEmptySemantic(frame.Workflow, frame.Intent, frame.Kind)
	}
	if snapshot.Stage == "" && len(snapshot.MissingSlots) > 0 {
		snapshot.Stage = "missing_slots"
	}
	if snapshot.EndReason == "" && status == TaskSnapshotStatusExpired {
		snapshot.EndReason = firstNonEmptySemantic(frame.FailureReason, "任务上下文已过期，需要重新确认后继续")
	}
	e.sessionState.TaskSnapshot = snapshot
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
	e.markMemoryUpdateSource(memoryUpdateStructured)
}

func (e *Engine) markTaskSnapshotResolved(frame ContextFrame, reason string, now time.Time) {
	if contextFrameEmpty(frame) {
		return
	}
	e.syncTaskSnapshotFromFrame(frame, TaskSnapshotStatusResolved, reason, now)
}

func contextFrameConstraints(frame ContextFrame) []string {
	return compactSemanticPairs(map[string]string{
		"gpu":          frame.GPU,
		"image":        frame.ImagePref,
		"image_source": frame.ImageSource,
		"workload":     frame.Workload,
		"zone":         firstNonEmptySemantic(frame.ZoneLabel, frame.Zone),
	})
}

func contextFrameDecisions(frame ContextFrame) []string {
	return compactSemanticPairs(frame.Slots)
}

func contextFrameEntities(frame ContextFrame, freshness string) []SemanticEntityHint {
	if catalog, err := defaultActionCatalog(); err == nil {
		if spec, ok := catalog.Lookup(frame.Workflow); ok {
			var out []SemanticEntityHint
			for name, field := range spec.Fields {
				if !field.Target {
					continue
				}
				if id := strings.TrimSpace(frame.Slots[name]); id != "" {
					out = append(out, SemanticEntityHint{Kind: field.TargetKind, ID: compactSemanticText(id), Source: compactSemanticText(frame.SlotSources[name]), Freshness: freshness})
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	// Compatibility for frames written before workflow fields became the
	// canonical task projection.
	id := strings.TrimSpace(frame.Slots["instance_id"])
	if id == "" {
		return nil
	}
	return []SemanticEntityHint{{Kind: "instance", ID: compactSemanticText(id), Source: compactSemanticText(frame.SlotSources["instance_id"]), Freshness: freshness}}
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

func (e *Engine) refreshConversationDigest(now time.Time) {
	if e == nil || !e.sessionStateHydrated {
		return
	}
	digest := e.sessionState.ConversationDigest
	task := e.sessionState.TaskSnapshot
	if task.Goal != "" {
		digest.Goals = mergeSemanticItems(digest.Goals, []string{task.Goal})
	}
	digest.Constraints = mergeSemanticItems(digest.Constraints, task.Constraints)
	digest.Decisions = mergeSemanticItems(digest.Decisions, task.Decisions)
	if task.Status == TaskSnapshotStatusActive || task.Status == TaskSnapshotStatusExpired {
		unresolved := task.Goal
		if unresolved == "" {
			unresolved = firstNonEmptySemantic(task.Workflow, task.Intent)
		}
		if len(task.MissingSlots) > 0 {
			unresolved += "；待补充：" + strings.Join(task.MissingSlots, "、")
		}
		digest.UnresolvedTasks = compactSemanticItems([]string{unresolved})
	} else if task.Status == TaskSnapshotStatusResolved {
		digest.UnresolvedTasks = nil
	}
	digest.EntityHints = mergeSemanticEntities(digest.EntityHints, task.Entities)
	if id := compactSemanticText(e.sessionState.SelectedInstanceID); id != "" {
		digest.EntityHints = mergeSemanticEntities(digest.EntityHints, []SemanticEntityHint{{
			Kind:      "instance",
			ID:        id,
			Name:      compactSemanticText(e.sessionState.SelectedInstanceName),
			Source:    compactSemanticText(e.sessionState.SelectedInstanceSource),
			Freshness: normalizedSelectedInstanceFreshness(e.sessionState),
		}})
	}
	digest.Narrative = buildConversationNarrative(digest)
	digest.UpdatedAtUnix = now.Unix()
	e.sessionState.ConversationDigest = digest
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
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

func buildConversationNarrative(digest ConversationDigest) string {
	var parts []string
	if len(digest.Goals) > 0 {
		parts = append(parts, "目标："+strings.Join(recentSemanticItems(digest.Goals, 4), "；"))
	}
	if len(digest.UnresolvedTasks) > 0 {
		parts = append(parts, "未完成："+strings.Join(recentSemanticItems(digest.UnresolvedTasks, 4), "；"))
	}
	if len(digest.Constraints) > 0 {
		parts = append(parts, "限制："+strings.Join(recentSemanticItems(digest.Constraints, 4), "；"))
	}
	if len(digest.Decisions) > 0 {
		parts = append(parts, "已决定："+strings.Join(recentSemanticItems(digest.Decisions, 4), "；"))
	}
	return compactSemanticNarrative(strings.Join(parts, "。"))
}

func compactSemanticPairs(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if compactSemanticText(key) != "" && compactSemanticText(value) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, compactSemanticText(key)+"="+compactSemanticText(values[key]))
	}
	return compactSemanticItems(out)
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

func recentSemanticItems(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[len(values)-limit:]
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

func mergeSemanticEntities(existing, incoming []SemanticEntityHint) []SemanticEntityHint {
	out := make([]SemanticEntityHint, 0, len(existing)+len(incoming))
	positions := map[string]int{}
	put := func(entity SemanticEntityHint) {
		entity.Kind = compactSemanticText(entity.Kind)
		entity.ID = compactSemanticText(entity.ID)
		entity.Name = compactSemanticText(entity.Name)
		entity.Source = compactSemanticText(entity.Source)
		if entity.ID == "" {
			return
		}
		key := strings.ToLower(entity.Kind + "\x00" + entity.ID)
		if idx, ok := positions[key]; ok {
			out[idx] = entity
			return
		}
		positions[key] = len(out)
		out = append(out, entity)
	}
	for _, entity := range existing {
		put(entity)
	}
	for _, entity := range incoming {
		put(entity)
	}
	if len(out) > maxSemanticItems {
		out = out[len(out)-maxSemanticItems:]
	}
	return out
}

func firstNonEmptySemantic(values ...string) string {
	for _, value := range values {
		if value = compactSemanticText(value); value != "" {
			return value
		}
	}
	return ""
}

func taskSnapshotEmpty(task TaskSnapshot) bool {
	return task.Goal == "" && task.Intent == "" && task.Workflow == "" && task.Stage == "" &&
		len(task.Constraints) == 0 && len(task.Decisions) == 0 && len(task.MissingSlots) == 0 &&
		len(task.Entities) == 0 && task.Status == "" && task.Freshness == "" && task.EndReason == "" &&
		task.UpdatedAtUnix == 0
}

func conversationDigestEmpty(digest ConversationDigest) bool {
	return digest.Narrative == "" && len(digest.Goals) == 0 && len(digest.Constraints) == 0 &&
		len(digest.Decisions) == 0 && len(digest.UnresolvedTasks) == 0 && len(digest.EntityHints) == 0 &&
		len(digest.Sources.Goals) == 0 && len(digest.Sources.Constraints) == 0 &&
		len(digest.Sources.Decisions) == 0 && len(digest.Sources.UnresolvedTasks) == 0 &&
		len(digest.Excerpts) == 0 && digest.SummaryFrontier == 0 &&
		digest.UpdatedAtUnix == 0
}
