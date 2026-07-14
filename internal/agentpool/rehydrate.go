package agentpool

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/store"
)

// committedTailTurnLimit fills the engine's 120 non-system message budget
// with 60 complete conversational turns. Tool transcripts are deliberately
// not persisted into this history; durable context facts are loaded separately
// by the turn coordinator.
const committedTailTurnLimit = 60

// denyConfirm is used as the ConfirmFunc for HTTP-path engines. All L1
// mutating actions are denied — confirmation requires human interaction
// which is not available over the HTTP API.
func denyConfirm(_ string, _ map[string]any) bool { return false }

// filterHistory converts a slice of store.Message rows into the
// engine.HistoryMessage slice used for rehydration. Only messages with
// status == "ok" and role "user" or "assistant" are included; all others
// (pending, error, aborted, …) are silently skipped.
func filterHistory(messages []store.Message) []engine.HistoryMessage {
	history := make([]engine.HistoryMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Status != "ok" {
			continue
		}
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		history = append(history, engine.HistoryMessage{Role: msg.Role, Content: msg.Content})
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

	// Fetch up to 100 prior messages for the session (sufficient for context
	// window; engine.RehydrateHistory will trim to maxHistoryMessages anyway).
	// owner is threaded through so that future callers may pass it to an
	// owner-scoped ListBySession variant without API changes here.
	msgs, _, err := p.messageStore.ListBySession(ctx, sessionID, 100, "")
	if err != nil {
		return nil, fmt.Errorf("agentpool: list messages for session %q: %w", sessionID, err)
	}

	eng.RehydrateHistory(filterHistory(msgs))
	return eng, nil
}

// NewTurnEngine creates a private mutable engine for one durable turn. It
// shares process-wide dependencies but is never inserted into the LRU, so a
// failed/uncommitted attempt cannot contaminate a later turn's memory.
func (p *Pool) NewTurnEngine(ctx context.Context, owner store.Owner, sessionID string) (*engine.Engine, error) {
	subject, _ := governance.SubjectKeyFromOrganization(owner.TopOrganizationID, owner.OrganizationID)
	return p.NewTurnEngineWithOptions(ctx, owner, sessionID, engine.SessionOptions{
		Subject:              subject,
		ConfirmFn:            denyConfirm,
		MutatingToolsEnabled: p.mutatingToolsEnabled,
	})
}

// NewTurnEngineWithOptions creates a private durable-turn engine while
// retaining the pool's process-wide dependency sharing. The boot-level
// mutation gate is an upper bound: a caller may force read-only, never enable
// writes that the process disabled.
func (p *Pool) NewTurnEngineWithOptions(
	ctx context.Context,
	owner store.Owner,
	sessionID string,
	opts engine.SessionOptions,
) (*engine.Engine, error) {
	tailStore, ok := p.messageStore.(store.CommittedTailMessageStore)
	if !ok {
		return nil, fmt.Errorf("agentpool: message store lacks committed tail capability")
	}
	msgs, err := tailStore.ListCommittedTail(ctx, owner, sessionID, committedTailTurnLimit)
	if err != nil {
		return nil, fmt.Errorf("agentpool: list committed tail for session %q: %w", sessionID, err)
	}
	history, err := validateCommittedTail(msgs)
	if err != nil {
		return nil, fmt.Errorf("agentpool: invalid committed tail for session %q: %w", sessionID, err)
	}

	if opts.Subject == "" {
		opts.Subject, _ = governance.SubjectKeyFromOrganization(owner.TopOrganizationID, owner.OrganizationID)
	}
	opts.MutatingToolsEnabled = opts.MutatingToolsEnabled && p.mutatingToolsEnabled
	eng := engine.NewSession(p.deps, opts)
	eng.RehydrateHistory(history)
	return eng, nil
}

func validateCommittedTail(messages []store.Message) ([]engine.HistoryMessage, error) {
	if len(messages)%2 != 0 {
		return nil, fmt.Errorf("odd message count %d", len(messages))
	}
	history := make([]engine.HistoryMessage, 0, len(messages))
	for i := 0; i < len(messages); i += 2 {
		userMsg, assistantMsg := messages[i], messages[i+1]
		if userMsg.Role != "user" || userMsg.Status != "ok" ||
			assistantMsg.Role != "assistant" || assistantMsg.Status != "ok" ||
			strings.TrimSpace(userMsg.Content) == "" || strings.TrimSpace(assistantMsg.Content) == "" {
			return nil, fmt.Errorf("messages %d-%d are not a committed user/assistant pair", i, i+1)
		}
		history = append(history,
			engine.HistoryMessage{Role: userMsg.Role, Content: userMsg.Content},
			engine.HistoryMessage{Role: assistantMsg.Role, Content: assistantMsg.Content},
		)
	}
	return history, nil
}
