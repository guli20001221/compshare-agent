package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
	reactHistorySummaryPrefix        = "【会话摘要，自动生成】"
	reactHistoryCompactedToolPrefix  = "【工具结果已压缩："
	recentRetrievableToolResultsKeep = 3
)

var retrievableToolResultActions = map[string]struct{}{
	"DescribeCompShareInstance":               {},
	"GetCompShareInstanceMonitor":             {},
	"GetCompShareInstancePrice":               {},
	"GetCompShareInstanceUserPrice":           {},
	"DescribeAvailableCompShareInstanceTypes": {},
	"DescribeCompShareImages":                 {},
	"DescribeCompShareCustomImages":           {},
	"DescribeCompShareSharingImages":          {},
	"DescribeCommunityImages":                 {},
}

// stripHistoricalToolTranscript makes hot-engine history match cold rebuilds:
// only user/final-assistant text crosses a turn boundary. Structured ToolFacts
// and the durable semantic summaries carry forward what is safe; raw tool JSON
// must be queried again when current values matter.
func stripHistoricalToolTranscript(messages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == openai.ChatMessageRoleTool {
			continue
		}
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0 {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func (e *Engine) trimHistoryByCompaction(now time.Time) {
	if len(e.messages) <= 1+maxHistoryMessages {
		if hasReactHistorySummary(e.messages) {
			e.messages = replaceReactHistorySummary(e.messages, e.buildReActHistorySummary(now))
		}
		return
	}

	hadSummary := hasReactHistorySummary(e.messages)
	msgs := replaceReactHistorySummary(e.messages, "")
	if hadSummary && len(msgs) <= 1+maxHistoryMessages {
		e.messages = replaceReactHistorySummary(msgs, e.buildReActHistorySummary(now))
		return
	}
	safeStart := safeHistoryStart(msgs, len(msgs)-maxHistoryMessages)
	if safeStart < 0 {
		return
	}

	e.absorbConversationDigest(msgs[1:safeStart], now)
	keep := append([]openai.ChatCompletionMessage(nil), msgs[safeStart:]...)
	compactOldRetrievableToolResults(keep, recentRetrievableToolResultsKeep)

	out := make([]openai.ChatCompletionMessage, 0, 2+len(keep))
	out = append(out, msgs[0])
	out = append(out, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: e.buildReActHistorySummary(now),
	})
	out = append(out, keep...)
	e.messages = out
	e.historyTrimmedThisSession = true
}

func (e *Engine) absorbConversationDigest(messages []openai.ChatCompletionMessage, now time.Time) {
	if e == nil || !e.sessionStateHydrated || len(messages) == 0 {
		return
	}
	var goals, decisions []string
	for _, msg := range messages {
		if msg.Role != openai.ChatMessageRoleUser {
			continue
		}
		text := compactSemanticText(msg.Content)
		if text == "" {
			continue
		}
		goals = append(goals, text)
		if containsAnyKeyword(text, []string{"就用", "选择", "决定", "改成", "换成", "确认"}) {
			decisions = append(decisions, text)
		}
	}
	if len(goals) == 0 && len(decisions) == 0 {
		return
	}
	digest := e.sessionState.ConversationDigest
	digest.Goals = mergeSemanticItems(digest.Goals, goals)
	digest.Decisions = mergeSemanticItems(digest.Decisions, decisions)
	digest.Narrative = buildConversationNarrative(digest)
	digest.UpdatedAtUnix = now.Unix()
	e.sessionState.ConversationDigest = digest
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func hasReactHistorySummary(messages []openai.ChatCompletionMessage) bool {
	for _, msg := range messages[1:] {
		if msg.Role == openai.ChatMessageRoleSystem && strings.HasPrefix(msg.Content, reactHistorySummaryPrefix) {
			return true
		}
	}
	return false
}

func safeHistoryStart(messages []openai.ChatCompletionMessage, candidateStart int) int {
	if candidateStart <= 1 {
		return -1
	}
	for i := candidateStart; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == openai.ChatMessageRoleUser {
			return i
		}
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) == 0 {
			return i
		}
	}
	return -1
}

func replaceReactHistorySummary(messages []openai.ChatCompletionMessage, summary string) []openai.ChatCompletionMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]openai.ChatCompletionMessage, 0, len(messages)+1)
	out = append(out, messages[0])
	if summary != "" {
		out = append(out, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: summary,
		})
	}
	for _, msg := range messages[1:] {
		if msg.Role == openai.ChatMessageRoleSystem && strings.HasPrefix(msg.Content, reactHistorySummaryPrefix) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func (e *Engine) buildReActHistorySummary(now time.Time) string {
	var lines []string
	if e.sessionStateHydrated {
		if e.sessionState.SelectedInstanceID != "" {
			prefix := "已选实例："
			if e.sessionState.SelectedInstanceFreshness == ContinuityFreshnessExpired ||
				(e.sessionState.SelectedInstanceAtUnix <= 0 && e.sessionState.SelectedInstanceSource == "") {
				prefix = "历史实例提示（不可授权写操作）："
			}
			if e.sessionState.SelectedInstanceName != "" {
				lines = append(lines, fmt.Sprintf("%s%s（%s）", prefix, e.sessionState.SelectedInstanceName, e.sessionState.SelectedInstanceID))
			} else {
				lines = append(lines, prefix+e.sessionState.SelectedInstanceID)
			}
		}
		if e.sessionState.LastIntent != "" {
			lines = append(lines, "上次意图："+e.sessionState.LastIntent)
		}
		task := e.sessionState.TaskSnapshot
		if task.Goal != "" {
			lines = append(lines, "任务目标："+task.Goal)
		}
		if len(task.Constraints) > 0 {
			lines = append(lines, "任务限制："+strings.Join(task.Constraints, "；"))
		}
		if len(task.Decisions) > 0 {
			lines = append(lines, "已作决定："+strings.Join(task.Decisions, "；"))
		}
		if len(task.MissingSlots) > 0 && task.Status != TaskSnapshotStatusResolved {
			lines = append(lines, "未完成任务待补充："+strings.Join(task.MissingSlots, "、"))
		}
		if task.Status == TaskSnapshotStatusExpired {
			lines = append(lines, "该任务已过期；仅供理解，不得直接继续执行")
		}
		if digest := compactSemanticNarrative(e.sessionState.ConversationDigest.Narrative); digest != "" {
			lines = append(lines, "早期对话摘要（只作参考）："+digest)
		}
		for i := len(e.sessionState.VerifiedKnowledge) - 1; i >= 0 && len(e.sessionState.VerifiedKnowledge)-i <= 3; i-- {
			memory := e.sessionState.VerifiedKnowledge[i]
			if strings.TrimSpace(memory.Question) == "" || strings.TrimSpace(memory.Answer) == "" {
				continue
			}
			lines = append(lines, "已验证知识结论（非实时状态）："+
				truncateRunes(strings.TrimSpace(memory.Question), 180)+" → "+
				truncateRunes(strings.TrimSpace(memory.Answer), 360))
		}
		lines = append(lines, recentFactBreadcrumbs(e.sessionState.RecentFacts, now)...)
		lines = append(lines, expiredFactBreadcrumbs(e.sessionState.RecentFacts, now)...)
	}
	if e.lastPlannerActionThisTurn != "" {
		lines = append(lines, "最近生命周期动作："+string(e.lastPlannerActionThisTurn))
	}
	if len(lines) == 0 {
		lines = append(lines, "暂无稳定结构化信号。")
	}
	return reactHistorySummaryPrefix + "\n" + strings.Join(lines, "\n")
}

func expiredFactBreadcrumbs(facts []ToolFact, now time.Time) []string {
	var out []string
	for _, fact := range facts {
		freshness := fact.Freshness
		if freshness == "" {
			freshness = continuityFreshness(fact.ProducedAtUnix, fact.TTLSeconds, now)
		}
		if freshness != ContinuityFreshnessExpired || fact.SubjectID == "" || fact.Kind == "" {
			continue
		}
		out = append(out, fmt.Sprintf(
			"历史事实主题：%s %s @%d（当前值必须重新查询）",
			fact.SubjectID, fact.Kind, fact.ProducedAtUnix,
		))
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func recentFactBreadcrumbs(facts []ToolFact, now time.Time) []string {
	type breadcrumb struct {
		subjectID string
		kind      string
		produced  int64
	}
	var items []breadcrumb
	for _, fact := range facts {
		if fact.SubjectID == "" || fact.Kind == "" || fact.ProducedAtUnix <= 0 || fact.TTLSeconds <= 0 {
			continue
		}
		if now.Unix()-fact.ProducedAtUnix > int64(fact.TTLSeconds) {
			continue
		}
		items = append(items, breadcrumb{
			subjectID: fact.SubjectID,
			kind:      fact.Kind,
			produced:  fact.ProducedAtUnix,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].produced != items[j].produced {
			return items[i].produced > items[j].produced
		}
		if items[i].subjectID != items[j].subjectID {
			return items[i].subjectID < items[j].subjectID
		}
		return items[i].kind < items[j].kind
	})
	if len(items) > 8 {
		items = items[:8]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("近期事实引用：%s %s @%d", item.subjectID, item.kind, item.produced))
	}
	return out
}

func compactOldRetrievableToolResults(messages []openai.ChatCompletionMessage, keepRecent int) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	toolActions := toolCallActionsByID(messages)
	seenRecent := 0
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != openai.ChatMessageRoleTool || msg.ToolCallID == "" {
			continue
		}
		action := toolActions[msg.ToolCallID]
		if _, ok := retrievableToolResultActions[action]; !ok {
			continue
		}
		if strings.HasPrefix(msg.Content, reactHistoryCompactedToolPrefix) {
			continue
		}
		if !toolResultContentCanBeCompacted(msg.Content) {
			continue
		}
		if seenRecent < keepRecent {
			seenRecent++
			continue
		}
		messages[i].Content = fmt.Sprintf("%s%s；如需最新或完整结果，请重新调用工具】", reactHistoryCompactedToolPrefix, action)
	}
}

func toolCallActionsByID(messages []openai.ChatCompletionMessage) map[string]string {
	actions := make(map[string]string)
	for _, msg := range messages {
		if msg.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			if call.ID == "" {
				continue
			}
			actions[call.ID] = call.Function.Name
		}
	}
	return actions
}

func toolResultContentCanBeCompacted(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" || strings.HasPrefix(content, reactHistoryCompactedToolPrefix) {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return false
	}
	ret, ok := payload["RetCode"]
	if !ok {
		return true
	}
	switch v := ret.(type) {
	case float64:
		return v == 0
	case int:
		return v == 0
	case string:
		return v == "" || v == "0"
	default:
		return false
	}
}
