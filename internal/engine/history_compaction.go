package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const (
	reactHistoryCompactedToolPrefix  = "【工具结果已压缩："
	recentRetrievableToolResultsKeep = 3
	conversationExcerptKeep          = 6
)

const conversationMemoryCompactorPrompt = `你是对话长期记忆压缩器。只从给出的完整问答对中提取以后仍有用的信息，不补充常识，不猜测用户意图。

输出 JSON：
{"goals":[{"value":"...","pair_index":0,"quote":"逐字原文"}],"constraints":[],"decisions":[],"unresolved_tasks":[]}

要求：
1. pair_index 从 0 开始，quote 必须逐字出现在该问答对的 user 或 assistant 中。
2. 只保存明确表达的目标、限制、已确认决定、未完成事项；没有就返回空数组。
3. 不保存密码、令牌、临时监控值或推测。`

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
	e.trimHistoryByCompactionContext(context.Background(), now)
}

// trimHistoryByCompactionContext bounds the raw transcript. Memory of evicted
// turns is preserved structurally (compactEvictedConversation distills it into
// the durable ConversationDigest), which the single context card surfaces on the
// next assembly. It no longer injects a second "会话摘要" system block: that block
// duplicated renderAgentContextCard — both derive from SessionState — so the
// model received two overlapping memory blocks. The card is now the sole memory
// block (see messagesFromAgentContext); this path only trims and shrinks.
func (e *Engine) trimHistoryByCompactionContext(ctx context.Context, now time.Time) {
	if len(e.messages) <= 1+maxHistoryMessages {
		return
	}
	safeStart := safeHistoryStart(e.messages, len(e.messages)-maxHistoryMessages)
	if safeStart < 0 {
		return
	}

	e.compactEvictedConversation(ctx, e.messages[1:safeStart], now)
	keep := append([]openai.ChatCompletionMessage(nil), e.messages[safeStart:]...)
	compactOldRetrievableToolResults(keep, recentRetrievableToolResultsKeep)

	out := make([]openai.ChatCompletionMessage, 0, 1+len(keep))
	out = append(out, e.messages[0])
	out = append(out, keep...)
	e.messages = out
	e.historyTrimmedThisSession = true
}

func (e *Engine) absorbConversationDigest(messages []openai.ChatCompletionMessage, now time.Time) {
	if e == nil || !e.sessionStateHydrated || len(messages) == 0 {
		return
	}
	pairs := completeConversationExcerpts(messages)
	if len(pairs) == 0 {
		return
	}
	digest := e.sessionState.ConversationDigest
	digest.Excerpts = boundedConversationExcerpts(append(digest.Excerpts, pairs...))
	digest.SummaryFrontier += int64(len(pairs))
	digest.UpdatedAtUnix = now.Unix()
	e.sessionState.ConversationDigest = digest
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
	e.markMemoryUpdateSource(memoryUpdateExcerpt)
}

func (e *Engine) compactEvictedConversation(ctx context.Context, messages []openai.ChatCompletionMessage, now time.Time) {
	if e == nil || !e.sessionStateHydrated {
		return
	}
	newPairs := completeConversationExcerpts(messages)
	if len(newPairs) == 0 {
		return
	}
	digest := e.sessionState.ConversationDigest
	pairs := append(append([]ConversationExcerpt(nil), digest.Excerpts...), newPairs...)
	delta, ok := e.generateMemoryDelta(ctx, pairs)
	if ok {
		digest = applyMemoryDelta(digest, delta)
		digest.Excerpts = nil
		e.markMemoryUpdateSource(memoryUpdateCompactor)
	} else {
		digest.Excerpts = boundedConversationExcerpts(pairs)
		e.markMemoryUpdateSource(memoryUpdateExcerpt)
	}
	digest.SummaryFrontier += int64(len(newPairs))
	digest.Narrative = buildConversationNarrative(digest)
	digest.UpdatedAtUnix = now.Unix()
	e.sessionState.ConversationDigest = digest
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func (e *Engine) generateMemoryDelta(ctx context.Context, pairs []ConversationExcerpt) (MemoryDelta, bool) {
	if e.llmClient == nil || len(pairs) == 0 || e.tokenBudgetExceeded() {
		return MemoryDelta{}, false
	}
	payload, err := json.Marshal(struct {
		Pairs  []ConversationExcerpt `json:"pairs"`
		Digest ConversationDigest    `json:"current_digest"`
		Task   TaskSnapshot          `json:"current_task"`
	}{Pairs: pairs, Digest: e.sessionState.ConversationDigest, Task: e.sessionState.TaskSnapshot})
	if err != nil {
		return MemoryDelta{}, false
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: conversationMemoryCompactorPrompt},
		{Role: openai.ChatMessageRoleUser, Content: string(payload)},
	}})
	if err != nil || resp == nil {
		return MemoryDelta{}, false
	}
	e.emitTokenUsage(resp.Usage)
	var delta MemoryDelta
	if !parseFirstJSONObject(resp.Content, &delta) {
		return MemoryDelta{}, false
	}
	validated, validCount, claimedCount := validateMemoryDelta(delta, pairs)
	if claimedCount > 0 && validCount == 0 {
		return MemoryDelta{}, false
	}
	return validated, true
}

// parseFirstJSONObject extracts the first balanced {...} object from an LLM
// response (tolerating code fences / prose around it) and unmarshals it into dst.
// Returns false when no object is found or it does not unmarshal — fail-closed.
func parseFirstJSONObject(raw string, dst any) bool {
	text := strings.TrimSpace(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return false
	}
	return json.Unmarshal([]byte(text[start:end+1]), dst) == nil
}

func completeConversationExcerpts(messages []openai.ChatCompletionMessage) []ConversationExcerpt {
	var out []ConversationExcerpt
	var pending string
	for _, msg := range messages {
		switch {
		case msg.Role == openai.ChatMessageRoleUser:
			pending = strings.TrimSpace(msg.Content)
		case msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) == 0 && pending != "":
			assistant := strings.TrimSpace(msg.Content)
			if assistant != "" {
				out = append(out, ConversationExcerpt{
					User:      truncateRunes(guardrails.RedactCredentials(pending), 600),
					Assistant: truncateRunes(guardrails.RedactCredentials(assistant), 600),
				})
			}
			pending = ""
		}
	}
	return out
}

func boundedConversationExcerpts(in []ConversationExcerpt) []ConversationExcerpt {
	if len(in) > conversationExcerptKeep {
		in = in[len(in)-conversationExcerptKeep:]
	}
	return append([]ConversationExcerpt(nil), in...)
}

func validateMemoryDelta(delta MemoryDelta, pairs []ConversationExcerpt) (MemoryDelta, int, int) {
	validCount, claimedCount := 0, 0
	validate := func(items []SourcedMemory) []SourcedMemory {
		claimedCount += len(items)
		out := make([]SourcedMemory, 0, len(items))
		for _, item := range items {
			item.Value = compactSemanticText(item.Value)
			item.Quote = strings.TrimSpace(item.Quote)
			if item.Value == "" || item.Quote == "" || item.PairIndex < 0 || item.PairIndex >= len(pairs) {
				continue
			}
			pair := pairs[item.PairIndex]
			if !strings.Contains(pair.User, item.Quote) && !strings.Contains(pair.Assistant, item.Quote) {
				continue
			}
			out = append(out, item)
			validCount++
		}
		return out
	}
	return MemoryDelta{
		Goals: validate(delta.Goals), Constraints: validate(delta.Constraints),
		Decisions: validate(delta.Decisions), UnresolvedTasks: validate(delta.UnresolvedTasks),
	}, validCount, claimedCount
}

func applyMemoryDelta(digest ConversationDigest, delta MemoryDelta) ConversationDigest {
	digest.Sources.Goals = mergeSourcedMemory(digest.Sources.Goals, delta.Goals)
	digest.Sources.Constraints = mergeSourcedMemory(digest.Sources.Constraints, delta.Constraints)
	digest.Sources.Decisions = mergeSourcedMemory(digest.Sources.Decisions, delta.Decisions)
	digest.Sources.UnresolvedTasks = mergeSourcedMemory(digest.Sources.UnresolvedTasks, delta.UnresolvedTasks)
	digest.Goals = mergeSemanticItems(digest.Goals, sourcedMemoryValues(delta.Goals))
	digest.Constraints = mergeSemanticItems(digest.Constraints, sourcedMemoryValues(delta.Constraints))
	digest.Decisions = mergeSemanticItems(digest.Decisions, sourcedMemoryValues(delta.Decisions))
	digest.UnresolvedTasks = mergeSemanticItems(digest.UnresolvedTasks, sourcedMemoryValues(delta.UnresolvedTasks))
	return digest
}

func sourcedMemoryValues(items []SourcedMemory) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Value)
	}
	return out
}

func mergeSourcedMemory(existing, incoming []SourcedMemory) []SourcedMemory {
	out := append([]SourcedMemory(nil), existing...)
	seen := make(map[string]struct{}, len(out))
	for _, item := range out {
		seen[item.Value+"\x00"+item.Quote] = struct{}{}
	}
	for _, item := range incoming {
		key := item.Value + "\x00" + item.Quote
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	if len(out) > maxSemanticItems {
		out = out[len(out)-maxSemanticItems:]
	}
	return out
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

func recentConversationExcerpts(excerpts []ConversationExcerpt, limit int) []ConversationExcerpt {
	if limit <= 0 || len(excerpts) == 0 {
		return nil
	}
	if len(excerpts) > limit {
		excerpts = excerpts[len(excerpts)-limit:]
	}
	return excerpts
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
