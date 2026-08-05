package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/security"
)

func cloneAgentContext(in AgentContext) AgentContext {
	out := in
	out.RecentConversation = append([]ConversationPair(nil), in.RecentConversation...)
	if in.ActiveTask != nil {
		copy := cloneTaskSnapshot(*in.ActiveTask)
		out.ActiveTask = &copy
	}
	out.SelectedEntities = cloneEntityHints(in.SelectedEntities)
	out.ContinuityNotices = append([]string(nil), in.ContinuityNotices...)
	return out
}

func cloneTaskSnapshot(in TaskSnapshot) TaskSnapshot {
	out := in
	out.Goal = safeContextText(in.Goal)
	out.Intent = safeContextText(in.Intent)
	out.Workflow = safeContextText(in.Workflow)
	out.Stage = safeContextText(in.Stage)
	out.Constraints = safeContextItems(in.Constraints)
	out.Decisions = safeContextItems(in.Decisions)
	out.MissingSlots = safeContextItems(in.MissingSlots)
	out.Entities = cloneEntityHints(in.Entities)
	return out
}

func cloneEntityHints(in []SemanticEntityHint) []SemanticEntityHint {
	out := make([]SemanticEntityHint, 0, len(in))
	for _, hint := range in {
		hint.Kind = safeContextText(hint.Kind)
		hint.ID = safeContextText(hint.ID)
		hint.Name = safeContextText(hint.Name)
		hint.Source = safeContextText(hint.Source)
		hint.Freshness = safeContextText(hint.Freshness)
		out = append(out, hint)
	}
	return out
}

func safeContextItems(in []string) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value = safeContextText(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}


// staleObservationNotices is what survives of the RecentFacts projection.
//
// The per-fact SUMMARY it used to build (rendered as 近期可信观测) restated a tool
// result the canonical transcript now replays verbatim, so it was a second and
// lossier copy and it is gone. The staleness notice is the opposite case: the
// transcript carries the ORIGINAL tool output, and that output has no expiry in
// it. Only the fact's TTL knows the value has since gone stale, so with the
// transcript on this notice is needed MORE than before, not less — it is the
// only thing standing between a replayed stock or price number and the model
// quoting it as current.
func staleObservationNotices(facts []ToolFact, now time.Time, notices []string) []string {
	if len(facts) == 0 {
		return notices
	}
	limit := len(facts)
	if limit > maxAgentContextObservations {
		limit = maxAgentContextObservations
	}
	for _, fact := range facts[:limit] {
		freshness := fact.Freshness
		if freshness == "" {
			freshness = continuityFreshness(fact.ProducedAtUnix, fact.TTLSeconds, now)
		}
		if freshness == ContinuityFreshnessFresh {
			continue
		}
		kind := safeContextText(fact.Kind)
		subject := safeContextText(fact.SubjectID)
		if kind == "" && subject == "" {
			continue
		}
		notices = append(notices, fmt.Sprintf("历史观测 %s %s 已过期或不再新鲜，当前值必须重新查询", subject, kind))
	}
	return compactSemanticItems(notices)
}

func safeContextText(value string) string {
	return compactSemanticText(security.RedactOperationalTokensInText(value))
}

// safeConversationText redacts a replayed exchange WITHOUT compacting it.
//
// A restored conversation pair is a real user/assistant message, not semantic
// memory, so the two transforms that safeContextText applies are wrong for it:
// collapsing whitespace destroys pasted terminal output and code, and cutting at
// maxSemanticRunes truncates the reply the model is being asked to remember.
// Measured on the 2026-07 production exports (3867 completed exchanges): 41.8% of
// assistant replies exceed 320 runes (median 255, p90 782), so 43% of exchanges
// lost content on the way into the next turn. It bought little — replaying a real
// session's whole history is 33 runes at the median and 5,764 at p99.
//
// Size is bounded instead by maxReplayedHistoryRunes, which drops whole older
// exchanges. That budget, not this function, is what keeps history affordable:
// per-request size alone cannot be compared against max_tokens_per_turn, because
// that ceiling is cumulative over every model call in the turn and history is
// re-sent on each one.
//
// Redaction is NOT part of that trade and stays: the hot engine holds the raw text
// (only the persisted copy is redacted at write time), so this call is the sole
// thing between an operational credential in an earlier turn and the model.
func safeConversationText(value string) string {
	return security.RedactOperationalTokensInText(value)
}

func safeContextNarrative(value string) string {
	return compactSemanticNarrative(security.RedactOperationalTokensInText(value))
}

// isLiveSelectionHint reports whether one SelectedEntities row is live execution
// state rather than semantic memory about the conversation.
//
// CompileForTurn assembles SelectedEntities from five sources and only three are
// execution state: the account's sole instance, the pending selection card's
// numbered candidates, and the current SelectedInstanceID with its provenance
// (user_selected or observed). The other two — TaskSnapshot.Entities and
// ConversationDigest.EntityHints — are the semantic layer the canonical transcript
// replaces, and they arrive carrying actionresolver CandidateSource values
// (user_explicit / verified_context / tool_observation / user_confirmation /
// agent_inference).
//
// An allowlist rather than a denylist, so a semantic source added later is
// excluded by default instead of admitted by an out-of-date list.
func isLiveSelectionHint(hint SemanticEntityHint) bool {
	switch hint.Source {
	case selectionSourceAccountSingle, selectionSourcePendingCard,
		SelectedInstanceSourceUser, SelectedInstanceSourceObserved:
		return true
	}
	return false
}

// renderAgentContextCard serializes structured semantic memory. Complete recent
// exchanges are restored as ordinary user/assistant messages by
// messagesFromAgentContext, so they are not duplicated here.
//
// With the canonical transcript on, that transcript IS the semantic history — the
// model reads prior turns' messages, tool calls and tool results verbatim — so a
// card restating a summary of them is a second, lossier memory of the same thing.
// The semantic blocks are therefore suppressed and what remains is what a
// transcript cannot carry: live execution state (current selection, pending
// selection card) and this turn's continuity notices (read-only, recovered
// operation), under the same no-write header.
//
// The suppression is HERE, at the model-facing serialization, and deliberately NOT
// in CompileForTurn. selection_binder (selection_binder.go:143, which keeps its own
// two-source allowlist over this same slice) and the write-proposal path read
// view.SelectedEntities as a struct and never see this string; filtering upstream
// would silently narrow the write path's target binding instead of only changing
// what the model reads.
//
// Every suppression below is guarded by `semantic`, which is true whenever the flag
// is off — so with the flag off this function is byte-identical to before.
func renderAgentContextCard(view AgentContext) string {
	semantic := !canonicalTranscriptEnabled
	var lines []string
	lines = append(lines, "【本轮统一上下文；仅帮助理解，不授权任何写操作】")
	if task := view.ActiveTask; semantic && task != nil {
		parts := []string{"目标=" + safeContextText(task.Goal)}
		if task.Stage != "" {
			parts = append(parts, "阶段="+safeContextText(task.Stage))
		}
		if len(task.MissingSlots) > 0 {
			parts = append(parts, "待补充="+strings.Join(compactSemanticItems(task.MissingSlots), "、"))
		}
		if task.Freshness != "" {
			parts = append(parts, "新鲜度="+safeContextText(task.Freshness))
		}
		lines = append(lines, "活动任务："+strings.Join(parts, "；"))
		// Task-level constraints and decisions used to reach the card indirectly:
		// refreshConversationDigest merged them into the digest and the card
		// rendered them as 既有约束 / 已作决定. Those digest lines are deleted, so
		// they no longer surface anywhere the model reads. The merge itself is left
		// alone — it feeds the compaction accumulator, which is the next step's
		// problem, not this one's.
		if task.Status == TaskSnapshotStatusExpired {
			lines = append(lines, "该任务已过期；仅供理解，不得直接继续执行")
		}
	}
	for _, entity := range view.SelectedEntities {
		// The live rows stay: dropping them would take away the current selection,
		// not a memory of the conversation.
		if !semantic && !isLiveSelectionHint(entity) {
			continue
		}
		label := strings.TrimSpace(entity.Name + " " + entity.ID)
		if label != "" {
			ordinal := ""
			if entity.Ordinal > 0 {
				ordinal = fmt.Sprintf("，序号=%d", entity.Ordinal)
			}
			lines = append(lines, fmt.Sprintf("相关对象：%s（类型=%s，来源=%s，新鲜度=%s%s）", safeContextText(label), safeContextText(entity.Kind), safeContextText(entity.Source), safeContextText(entity.Freshness), ordinal))
		}
	}
	for _, notice := range view.ContinuityNotices {
		lines = append(lines, "上下文提示："+safeContextNarrative(notice))
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}
