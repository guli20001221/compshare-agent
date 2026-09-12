package engine

import (
	"strings"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
)

// instanceOpsModelContext projects the same canonical, role-preserving conversation
// the outer agent receives, including completed tool observations. A resumed SDK
// session receives an exact suffix after the prior bridge anchor rather than a
// second copy of the same user request or prior tool results. Prior assistant prose is
// conversation context, not live instance evidence or execution authority; current
// platform facts and SSH observations remain separate.
func (e *Engine) instanceOpsModelContext() opscontext.Context {
	ctx := opscontext.Context{SchemaVersion: opscontext.SchemaVersion}
	if e == nil {
		return ctx
	}
	// Only the current USER-TYPED text may mint an ephemeral Authorization
	// capability. OCR and prior turns remain reference evidence and are redacted
	// below, never promoted into executable credentials.
	_, authorizationRefs := security.CaptureUserAuthorizationHeaders(userAuthoredText(e.lastUserMsg))
	// An HTTP request has one Authorization header. Multiple different values in
	// one user turn have no deterministic target association, so expose none and
	// let the agent request one unambiguous value instead of guessing.
	if len(authorizationRefs) == 1 {
		item := authorizationRefs[0]
		ctx.ProbeAuthorizations = []opscontext.ProbeAuthorization{{
			Reference: item.Reference,
			Value:     item.Value,
		}}
	}
	ctx.ConversationHistory = e.instanceOpsConversationHistory()
	return ctx
}

// instanceOpsConversationHistory projects the chronological visible role endpoints
// and completed observations already held by the canonical outer conversation. It retains user
// turns whose assistant ended pending/error/aborted: a later "继续"
// must not erase the request whose inner SDK work is being resumed.
func (e *Engine) instanceOpsConversationHistory() []opscontext.ConversationMessage {
	if e == nil || strings.TrimSpace(e.lastUserMsg) == "" {
		return nil
	}
	authored, _ := security.CaptureUserAuthorizationHeaders(userAuthoredText(e.lastUserMsg))
	pairs := e.attachRecordedTranscripts(conversationPairsFromMessages(e.messages))
	// During ChatWithOptions the current user has already been appended and is the
	// final visible endpoint while the outer Agent is invoking this tool. Do not
	// identify it by searching every historical turn: repeated messages such as
	// "继续" are ordinary, and an older completed exchange with the same bytes must
	// not suppress the new unanswered user in direct/cold-rebuild callers.
	currentIncluded := false
	if len(pairs) > 0 {
		lastPair := pairs[len(pairs)-1]
		if lastPair.Assistant == "" {
			canonicalAuthored := strings.TrimSpace(historyConversationText(openai.ChatMessageRoleUser, authored))
			currentIncluded = strings.TrimSpace(userAuthoredText(lastPair.User)) == canonicalAuthored
		}
	}
	if currentIncluded {
		// The current round's Diagnose invocation has no result yet. Stop at the
		// last settled result, then reuse canonical pairing/redaction/bounding;
		// neither an unanswered call nor its planner arguments become evidence.
		start := currentTurnStart(e.messages)
		pairs[len(pairs)-1].Transcript = nil
		for i := len(e.messages) - 1; i > start; i-- {
			if e.messages[i].Role == openai.ChatMessageRoleTool {
				pairs[len(pairs)-1].Transcript = ProjectTranscript(buildTranscriptV1(e.messages[:i+1]))
				break
			}
		}
	}
	if !currentIncluded {
		// Direct unit callers have not appended the current user yet. Reconstruct
		// the same stable wrapper as the production append/persistence path.
		raw := e.lastUserMsg
		if strings.TrimSpace(e.imageContextThisTurn) != "" {
			raw = WrapScreenshotContext(e.imageContextThisTurn, raw)
		}
		if content := strings.TrimSpace(historyConversationText(openai.ChatMessageRoleUser, raw)); content != "" {
			pairs = append(pairs, ConversationPair{User: content})
		}
	}
	// Keep the existing dialogue budget (including the current user), while
	// treating current observations like the outer active transcript, not optional
	// historical detail. They already have the canonical transcript bound and must
	// not disappear merely because the plain conversation fills its history budget.
	if len(pairs) > 0 {
		current := pairs[len(pairs)-1]
		pairs[len(pairs)-1].Transcript = nil
		pairs = budgetReplayedPairs(pairs, maxReplayedHistoryRunes)
		pairs[len(pairs)-1] = current
	}
	var out []opscontext.ConversationMessage
	for _, pair := range pairs {
		out = append(out, opscontext.ConversationMessage{Role: opscontext.ConversationRoleUser, Content: strings.TrimSpace(pair.User)})
		names := make(map[string]string)
		for _, message := range pair.Transcript {
			for _, call := range message.ToolCalls {
				names[call.ID] = call.Function.Name
			}
			if message.Role == openai.ChatMessageRoleTool {
				out = append(out, opscontext.ConversationMessage{
					Role:    opscontext.ConversationRoleTool,
					Content: names[message.ToolCallID] + ":\n" + message.Content,
				})
			}
		}
		if assistant := strings.TrimSpace(pair.Assistant); assistant != "" {
			out = append(out, opscontext.ConversationMessage{Role: opscontext.ConversationRoleAssistant, Content: assistant})
		}
	}
	return out
}
