package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
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
// only user/final-assistant text crosses a turn boundary. Raw tool JSON must be
// queried again when current values matter — or, with the canonical transcript
// on, is replayed verbatim from the persisted record by attachRecordedTranscripts.
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

// trimHistoryByCompactionContext bounds the raw transcript. It trims and shrinks
// and does nothing else — evicted turns leave no summary behind.
//
// They used to: compactEvictedConversation distilled them into ConversationDigest
// and the context card rendered it. Nothing has read that digest since the card's
// digest blocks were deleted, and the LLM call that produced it never ran on the
// shipped path anyway — measured against the replay database, 0 of 127 sessions
// had a single digest excerpt.
//
// What the model reads is unaffected by where this cut lands, and that is now an
// invariant rather than an observation about two constants: it shares
// rawHistoryCutPoint and maxRawHistoryRunes with the plain trim, and that budget
// is set above anything maxReplayedHistoryRunes can admit. It used to be an
// observation — "maxAgentContextPairs is 20 and the cut leaves
// maxHistoryMessages/2 = 60 pairs" — which held only while both were counts and
// only for exchanges of an assumed size.
func (e *Engine) trimHistoryByCompactionContext(ctx context.Context, now time.Time) {
	safeStart := rawHistoryCutPoint(e.messages, maxRawHistoryRunes)
	if safeStart < 0 {
		return
	}

	keep := append([]openai.ChatCompletionMessage(nil), e.messages[safeStart:]...)
	compactOldRetrievableToolResults(keep, recentRetrievableToolResultsKeep)

	out := make([]openai.ChatCompletionMessage, 0, 1+len(keep))
	out = append(out, e.messages[0])
	out = append(out, keep...)
	e.messages = out
	e.historyTrimmedThisSession = true
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
