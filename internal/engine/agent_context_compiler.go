package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/security"
)

func cloneAgentContext(in AgentContext) AgentContext {
	out := in
	out.RecentConversation = append([]ConversationPair(nil), in.RecentConversation...)
	out.ConversationDigest = cloneConversationDigest(in.ConversationDigest)
	if in.ActiveTask != nil {
		copy := cloneTaskSnapshot(*in.ActiveTask)
		out.ActiveTask = &copy
	}
	out.SelectedEntities = cloneEntityHints(in.SelectedEntities)
	out.RecentObservations = append([]ToolObservationView(nil), in.RecentObservations...)
	out.VerifiedKnowledge = cloneVerifiedKnowledge(in.VerifiedKnowledge, len(in.VerifiedKnowledge))
	out.ContinuityNotices = append([]string(nil), in.ContinuityNotices...)
	return out
}

func cloneConversationDigest(in ConversationDigest) ConversationDigest {
	out := in
	out.Narrative = safeContextNarrative(in.Narrative)
	out.Goals = safeContextItems(in.Goals)
	out.Constraints = safeContextItems(in.Constraints)
	out.Decisions = safeContextItems(in.Decisions)
	out.UnresolvedTasks = safeContextItems(in.UnresolvedTasks)
	out.EntityHints = cloneEntityHints(in.EntityHints)
	out.Excerpts = make([]ConversationExcerpt, 0, len(in.Excerpts))
	for _, excerpt := range in.Excerpts {
		out.Excerpts = append(out.Excerpts, ConversationExcerpt{User: safeContextText(excerpt.User), Assistant: safeContextText(excerpt.Assistant)})
	}
	out.Sources = MemoryDelta{
		Goals:           cloneSourcedMemory(in.Sources.Goals),
		Constraints:     cloneSourcedMemory(in.Sources.Constraints),
		Decisions:       cloneSourcedMemory(in.Sources.Decisions),
		UnresolvedTasks: cloneSourcedMemory(in.Sources.UnresolvedTasks),
	}
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

func cloneVerifiedKnowledge(in []VerifiedKnowledgeTurn, limit int) []VerifiedKnowledgeTurn {
	if limit <= 0 || len(in) == 0 {
		return nil
	}
	if len(in) > limit {
		in = in[len(in)-limit:]
	}
	out := make([]VerifiedKnowledgeTurn, 0, len(in))
	for _, item := range in {
		copy := item
		copy.Question = safeContextText(item.Question)
		copy.Answer = safeContextNarrative(item.Answer)
		copy.Evidence = knowledge.EvidenceLedger{
			Query: safeContextText(item.Evidence.Query),
			Items: cloneEvidenceItems(item.Evidence.Items),
		}
		out = append(out, copy)
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

func cloneSourcedMemory(in []SourcedMemory) []SourcedMemory {
	out := make([]SourcedMemory, 0, len(in))
	for _, memory := range in {
		memory.Value = safeContextText(memory.Value)
		memory.Quote = safeContextText(memory.Quote)
		out = append(out, memory)
	}
	return out
}

func cloneEvidenceItems(in []knowledge.EvidenceItem) []knowledge.EvidenceItem {
	out := make([]knowledge.EvidenceItem, 0, len(in))
	for _, item := range in {
		item.ChunkID = safeContextText(item.ChunkID)
		item.Title = safeContextText(item.Title)
		item.ProductArea = safeContextText(item.ProductArea)
		item.SourceType = safeContextText(item.SourceType)
		item.ScoreBucket = safeContextText(item.ScoreBucket)
		item.Summary = safeContextNarrative(item.Summary)
		item.Snippet = safeContextNarrative(item.Snippet)
		out = append(out, item)
	}
	return out
}

func compileObservationViews(facts []ToolFact, now time.Time, notices []string) ([]ToolObservationView, []string) {
	if len(facts) == 0 {
		return nil, notices
	}
	limit := len(facts)
	if limit > maxAgentContextObservations {
		limit = maxAgentContextObservations
	}
	views := make([]ToolObservationView, 0, limit)
	for _, fact := range facts[:limit] {
		freshness := fact.Freshness
		if freshness == "" {
			freshness = continuityFreshness(fact.ProducedAtUnix, fact.TTLSeconds, now)
		}
		view := ToolObservationView{
			Kind:            safeContextText(fact.Kind),
			SubjectID:       safeContextText(fact.SubjectID),
			Source:          safeContextText(fact.Source),
			Completeness:    safeContextText(fact.Completeness),
			Freshness:       freshness,
			RefreshRequired: fact.RefreshRequired || freshness != ContinuityFreshnessFresh,
			ProducedAtUnix:  fact.ProducedAtUnix,
		}
		if freshness == ContinuityFreshnessFresh {
			view.Summary = observationSummary(fact, now)
		} else if view.Kind != "" || view.SubjectID != "" {
			notices = append(notices, fmt.Sprintf("历史观测 %s %s 已过期或不再新鲜，当前值必须重新查询", view.SubjectID, view.Kind))
		}
		views = append(views, view)
	}
	return views, compactSemanticItems(notices)
}

func observationSummary(fact ToolFact, now time.Time) string {
	rendered := assembleFactContext([]ToolFact{fact}, now)
	rendered = strings.TrimPrefix(rendered, recentObservationPrefix)
	rendered = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rendered), "- "))
	return safeContextNarrative(rendered)
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

// renderAgentContextCard serializes only structured semantic memory. Complete
// recent exchanges are restored as ordinary user/assistant messages by
// messagesFromAgentContext, so they are not duplicated here.
func renderAgentContextCard(view AgentContext) string {
	var lines []string
	lines = append(lines, "【本轮统一上下文；仅帮助理解，不授权任何写操作】")
	if task := view.ActiveTask; task != nil {
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
		// task-level constraints/decisions are not re-rendered here: refreshConversationDigest
		// merges them into ConversationDigest.Constraints/Decisions at turn entry, which the
		// card already surfaces below as 既有约束 / 已作决定. The task-expired caution has no
		// digest home, so it is restored here.
		if task.Status == TaskSnapshotStatusExpired {
			lines = append(lines, "该任务已过期；仅供理解，不得直接继续执行")
		}
	}
	if narrative := safeContextNarrative(view.ConversationDigest.Narrative); narrative != "" {
		lines = append(lines, "较早对话摘要："+narrative)
	}
	appendItems := func(label string, values []string) {
		if values = safeContextItems(values); len(values) > 0 {
			lines = append(lines, label+"："+strings.Join(values, "；"))
		}
	}
	appendItems("目标", view.ConversationDigest.Goals)
	appendItems("既有约束", view.ConversationDigest.Constraints)
	appendItems("已作决定", view.ConversationDigest.Decisions)
	appendItems("未完成事项", view.ConversationDigest.UnresolvedTasks)
	for _, excerpt := range view.ConversationDigest.Excerpts {
		if excerpt.User != "" && excerpt.Assistant != "" {
			lines = append(lines, "较早完整摘录：用户="+excerpt.User+"；助手="+excerpt.Assistant)
		}
	}
	for _, entity := range view.SelectedEntities {
		label := strings.TrimSpace(entity.Name + " " + entity.ID)
		if label != "" {
			ordinal := ""
			if entity.Ordinal > 0 {
				ordinal = fmt.Sprintf("，序号=%d", entity.Ordinal)
			}
			lines = append(lines, fmt.Sprintf("相关对象：%s（类型=%s，来源=%s，新鲜度=%s%s）", safeContextText(label), safeContextText(entity.Kind), safeContextText(entity.Source), safeContextText(entity.Freshness), ordinal))
		}
	}
	for _, observation := range view.RecentObservations {
		if observation.Summary != "" {
			lines = append(lines, "近期可信观测："+observation.Summary)
		}
	}
	for _, memory := range view.VerifiedKnowledge {
		if memory.Question != "" && memory.Answer != "" {
			lines = append(lines, "已验证知识：问="+memory.Question+"；答="+memory.Answer)
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
