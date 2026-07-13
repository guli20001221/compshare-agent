package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// rollCappedSession continues a conversation that has hit the per-session turn cap, in a
// SUCCESSOR session that carries its context, instead of refusing the turn outright.
//
// What it replaces: `return nil, ErrSessionTurnLimit` — 「本会话轮数已达上限，请新开会话继续」.
// The user then opened a new session and it was born EMPTY, so the agent had never read a
// word of the conversation the user was still looking at. That is amnesia by design, and
// unlike the other context boundaries it needed no instrumentation to prove.
//
// Measured on the 2026-06-26..07-02 production export: 24 of 439 sessions reach exactly the
// cap and NONE goes past it — against 5 that stop at 9 turns, so the spike at the boundary is
// truncation, not conversations that happened to end. Those 24 hold 21% of all user turns,
// because the sessions that hit the wall are the long, engaged ones. Ten of the 24 owners
// opened a new session within FIVE MINUTES of the wall (median 4 min, fastest 36 s); five
// never came back at all.
//
// The cap is NOT removed. Its purpose is a resource guard — "resource-wise they consumed a
// slot" — and the successor honours it: it starts from a bounded handoff
// (engine.SessionHandoffMessages trailing messages plus the structured SessionState), never a
// replay of all ten turns, so it costs LESS than the session it continues.
//
// The client needs no new protocol. The `meta` frame already carries SessionId and the front
// end already adopts a backend-changed session id while keeping the transcript on screen. The
// very mechanism that makes a silent swap into an EMPTY session so damaging is the right one
// here — because the session it swaps to has the context.
// It returns only the successor: the handoff it persisted is read back out of that session's
// envelope by the caller on EVERY subsequent turn, not just this one, because the successor's
// engine is rebuilt from scratch whenever the pool evicts it.
func (h *Handlers) rollCappedSession(ctx context.Context, owner store.Owner, capped store.Session) (store.Session, error) {
	// The predecessor's structured state: selected instance, last intent, recent tool facts,
	// any pending workflow frame. Carried WHOLE — it is the part of the context the engine
	// already knows how to consume, and dropping it would strand a user mid-workflow.
	prior, err := engine.ParsePersistedContext(capped.Context)
	if err != nil {
		// An unparseable / unknown-schema envelope must not be rewritten: ParsePersistedContext
		// documents that persisting after a parse error turns a transient rollout condition
		// into permanent state loss. Refuse the rollover; the cap still applies.
		return store.Session{}, fmt.Errorf("parse capped session context: %w", err)
	}

	msgs, _, err := h.messages.ListBySession(ctx, capped.ID, 100, "")
	if err != nil {
		return store.Session{}, fmt.Errorf("list capped session messages: %w", err)
	}
	tail := handoffTail(msgs, engine.SessionHandoffMessages)
	if len(tail) == 0 {
		// Nothing to carry means we cannot hand the successor the conversation — and a
		// successor without it IS the empty session this whole path exists to avoid.
		return store.Session{}, errors.New("capped session has no carryable history")
	}

	raw, err := json.Marshal(engine.PersistedContext{
		AgentSessionState: prior.AgentSessionState,
		ClientContext:     prior.ClientContext,
		Handoff: &engine.SessionHandoff{
			FromSessionID: capped.ID,
			Messages:      tail,
		},
	})
	if err != nil {
		return store.Session{}, fmt.Errorf("marshal successor context: %w", err)
	}

	// The handoff is PERSISTED with the successor, not merely seeded into its engine: the pool
	// evicts at capacity / 30 min idle, and a handoff that lived only in memory would vanish on
	// the first cold rebuild — recreating, deliberately, the bug this fixes.
	successor, err := h.sessions.Create(ctx, owner, capped.Title, raw)
	if err != nil {
		return store.Session{}, fmt.Errorf("create successor session: %w", err)
	}
	log.Printf("session %s hit the turn cap; continuing in %s with %d carried messages",
		capped.ID, successor.ID, len(tail))
	return successor, nil
}

// handoffTail returns the last n user/assistant messages of a capped session, oldest first.
//
// It applies the same status=="ok" / role filter that agentpool.filterHistory uses on
// rehydration, so the successor inherits exactly the turns a rebuild of the predecessor would
// have shown — never a pending, errored or aborted row, which would carry a question the agent
// never actually answered into the new conversation as though it had.
func handoffTail(msgs []store.Message, n int) []engine.HistoryMessage {
	kept := make([]engine.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Status != "ok" || (m.Role != "user" && m.Role != "assistant") {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		kept = append(kept, engine.HistoryMessage{Role: m.Role, Content: m.Content})
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return kept
}
