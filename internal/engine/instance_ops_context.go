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
// already held by the canonical outer conversation. Unlike complete-pair consumers,
// it retains user turns whose assistant ended pending/error/aborted: a later "继续"
// must not erase the request whose inner SDK work is being resumed.
func (e *Engine) instanceOpsConversationHistory() []opscontext.ConversationMessage {
	if e == nil || strings.TrimSpace(e.lastUserMsg) == "" {
		return nil
	}
	authored, _ := security.CaptureUserAuthorizationHeaders(userAuthoredText(e.lastUserMsg))
	groups := make([][]opscontext.ConversationMessage, 0, len(e.messages)/2+1)
	for _, message := range e.messages {
		var projected opscontext.ConversationMessage
		switch message.Role {
		case openai.ChatMessageRoleUser:
			content := strings.TrimSpace(historyConversationText(message.Role, message.Content))
			if content == "" {
				continue
			}
			projected = opscontext.ConversationMessage{Role: opscontext.ConversationRoleUser, Content: content}
			groups = append(groups, []opscontext.ConversationMessage{projected})
		case openai.ChatMessageRoleAssistant:
			if len(message.ToolCalls) > 0 {
				continue
			}
			content := strings.TrimSpace(historyConversationText(message.Role, message.Content))
			if content == "" {
				continue
			}
			projected = opscontext.ConversationMessage{Role: opscontext.ConversationRoleAssistant, Content: content}
			if len(groups) > 0 && len(groups[len(groups)-1]) == 1 &&
				groups[len(groups)-1][0].Role == opscontext.ConversationRoleUser {
				groups[len(groups)-1] = append(groups[len(groups)-1], projected)
			} else {
				groups = append(groups, []opscontext.ConversationMessage{projected})
			}
		}
	}
	// During ChatWithOptions the current user has already been appended and is the
	// final visible endpoint while the outer Agent is invoking this tool. Do not
	// identify it by searching every historical turn: repeated messages such as
	// "继续" are ordinary, and an older completed exchange with the same bytes must
	// not suppress the new unanswered user in direct/cold-rebuild callers.
	currentIncluded := false
	if len(groups) > 0 {
		lastGroup := groups[len(groups)-1]
		if len(lastGroup) == 1 && lastGroup[0].Role == opscontext.ConversationRoleUser {
			canonicalAuthored := strings.TrimSpace(historyConversationText(openai.ChatMessageRoleUser, authored))
			currentIncluded = strings.TrimSpace(userAuthoredText(lastGroup[0].Content)) == canonicalAuthored
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
			groups = append(groups, []opscontext.ConversationMessage{{
				Role: opscontext.ConversationRoleUser, Content: content,
			}})
		}
	}
	return budgetInstanceOpsConversationGroups(groups, maxReplayedHistoryRunes)
}

func budgetInstanceOpsConversationGroups(groups [][]opscontext.ConversationMessage, budgetRunes int) []opscontext.ConversationMessage {
	if len(groups) == 0 {
		return nil
	}
	spent := 0
	keepFrom := len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		cost := 0
		for _, message := range groups[i] {
			cost += len([]rune(message.Content))
		}
		if i < len(groups)-1 && budgetRunes > 0 && spent+cost > budgetRunes {
			break
		}
		spent += cost
		keepFrom = i
	}
	var out []opscontext.ConversationMessage
	for _, group := range groups[keepFrom:] {
		out = append(out, group...)
	}
	return out
}
