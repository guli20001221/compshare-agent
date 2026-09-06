package agentpool

import (
	"context"
	"fmt"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/store"
)

// denyConfirm is used as the ConfirmFunc for HTTP-path engines. All L1
// mutating actions are denied — confirmation requires human interaction
// which is not available over the HTTP API.
func denyConfirm(_ string, _ map[string]any) bool { return false }

// filterHistory converts a slice of store.Message rows into the
// engine.HistoryMessage slice used for rehydration. Only messages with
// status == "ok" and role "user" or "assistant" are included. Aborted/error
// assistant rows carry only their transcript: display placeholders are not
// answers. Pending rows remain excluded.
func filterHistory(messages []store.Message) []engine.HistoryMessage {
	history := make([]engine.HistoryMessage, 0, len(messages))
	for _, msg := range messages {
		interrupted := msg.Role == "assistant" && (msg.Status == "aborted" || msg.Status == "error")
		if msg.Status != "ok" && !interrupted {
			continue
		}
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		content := msg.Content
		if interrupted {
			content = ""
		}
		history = append(history, engine.HistoryMessage{
			Role:       msg.Role,
			Content:    content,
			Transcript: msg.Metadata,
		})
	}
	return history
}

// buildEngine constructs a fresh *engine.Engine for the given owner+session, then
// rehydrates its history from the MessageStore. engine.Init() is deliberately
// NOT called (HTTP path skips the welcome/suggestion pre-warm — see design §6.3).
func (p *Pool) buildEngine(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error) {
	eng := engine.NewSession(p.deps, engine.SessionOptions{
		Subject:              governance.AnonymousSubjectKey,
		ConfirmFn:            denyConfirm,
		MutatingToolsEnabled: p.mutatingToolsEnabled,
	})

	// Fetch only the latest 100 rows, not the first page of a long conversation.
	// The engine independently applies its model-request size budget.
	// owner is threaded through so that future callers may pass it to an
	// owner-scoped recent-history variant without API changes here.
	msgs, err := p.messageStore.ListRecentBySession(ctx, sessionID, 100)
	if err != nil {
		return nil, fmt.Errorf("agentpool: list messages for session %q: %w", sessionID, err)
	}

	eng.RehydrateHistory(filterHistory(msgs))
	return eng, nil
}
