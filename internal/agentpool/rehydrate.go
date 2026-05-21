package agentpool

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/engine"
)

// denyConfirm is used as the ConfirmFunc for HTTP-path engines. All L1
// mutating actions are denied — confirmation requires human interaction
// which is not available over the HTTP API.
func denyConfirm(_ string, _ map[string]any) bool { return false }

// buildEngine constructs a fresh *engine.Engine for the given session, then
// rehydrates its history from the MessageStore. engine.Init() is deliberately
// NOT called (HTTP path skips the welcome/suggestion pre-warm — see design §6.3).
func (p *Pool) buildEngine(ctx context.Context, sessionID string) (*engine.Engine, error) {
	eng := engine.New(p.cfg, denyConfirm)

	// Fetch up to 100 prior messages for the session (sufficient for context
	// window; engine.RehydrateHistory will trim to maxHistoryMessages anyway).
	msgs, _, err := p.messageStore.ListBySession(ctx, sessionID, 100, "")
	if err != nil {
		return nil, fmt.Errorf("agentpool: list messages for session %q: %w", sessionID, err)
	}

	// Convert store.Message rows to engine.HistoryMessage; skip non-conversational roles.
	histMsgs := make([]engine.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		histMsgs = append(histMsgs, engine.HistoryMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	eng.RehydrateHistory(histMsgs)
	return eng, nil
}
