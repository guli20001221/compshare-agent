package engine

import (
	"strings"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
)

// instanceOpsModelContext projects the same canonical, role-preserving conversation
// the outer agent receives. The current unanswered user message is the final user
// item, so a resumed SDK session can receive an exact suffix after the prior bridge
// anchor rather than a second copy of the same user request. Prior assistant prose is
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
// already held by the canonical outer conversation. Both projections retain user
// turns whose assistant ended pending/error/aborted: a later "继续"
// must not erase the request whose inner SDK work is being resumed.
func (e *Engine) instanceOpsConversationHistory() []opscontext.ConversationMessage {
	if e == nil || strings.TrimSpace(e.lastUserMsg) == "" {
		return nil
	}
	authored, _ := security.CaptureUserAuthorizationHeaders(userAuthoredText(e.lastUserMsg))
	pairs := conversationPairsFromMessages(e.messages)
	for i := range pairs {
		// Preserve the bridge's established endpoint bytes and SDK anchors. The
		// outer canonical replay keeps original whitespace; the bridge trims only
		// its role endpoints, as it did before sharing the projection.
		pairs[i].User = strings.TrimSpace(pairs[i].User)
		pairs[i].Assistant = strings.TrimSpace(pairs[i].Assistant)
	}
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
	var out []opscontext.ConversationMessage
	for _, pair := range budgetReplayedPairs(pairs, maxReplayedHistoryRunes) {
		out = append(out, opscontext.ConversationMessage{Role: opscontext.ConversationRoleUser, Content: pair.User})
		if pair.Assistant != "" {
			out = append(out, opscontext.ConversationMessage{Role: opscontext.ConversationRoleAssistant, Content: pair.Assistant})
		}
	}
	return out
}
