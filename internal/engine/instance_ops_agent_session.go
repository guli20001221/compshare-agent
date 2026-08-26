package engine

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/compshare-agent/internal/opscontext"
	"github.com/google/uuid"
)

const (
	// Bump this whenever the inner system prompt, tool semantics or authorization contract changes
	// incompatibly. A prior SDK transcript then becomes reference history rather than executable
	// continuation and is not resumed.
	instanceOpsAgentSessionContract = opscontext.AgentSessionContract
	instanceOpsAgentSessionTTL      = 30 * time.Minute
	maxInstanceOpsAgentTextBytes    = 128
	maxInstanceOpsAgentModelBytes   = 200
)

func normalizePersistedInstanceOpsAgentSession(in PersistedInstanceOpsAgentSession) PersistedInstanceOpsAgentSession {
	in.InstanceID = strings.TrimSpace(in.InstanceID)
	in.SessionID = strings.TrimSpace(in.SessionID)
	in.WorkdirID = strings.TrimSpace(in.WorkdirID)
	in.Contract = strings.TrimSpace(in.Contract)
	in.Model = strings.TrimSpace(in.Model)
	in.ConversationAnchor = strings.TrimSpace(in.ConversationAnchor)
	in.UpdatedAt = strings.TrimSpace(in.UpdatedAt)
	if in.InstanceID == "" || len(in.InstanceID) > maxInstanceOpsAgentTextBytes ||
		in.Contract != instanceOpsAgentSessionContract || len(in.Model) > maxInstanceOpsAgentModelBytes ||
		len(in.UpdatedAt) > maxInstanceOpsAgentTextBytes {
		return PersistedInstanceOpsAgentSession{}
	}
	if _, err := uuid.Parse(in.SessionID); err != nil {
		return PersistedInstanceOpsAgentSession{}
	}
	if _, err := uuid.Parse(in.WorkdirID); err != nil {
		return PersistedInstanceOpsAgentSession{}
	}
	if _, err := time.Parse(time.RFC3339Nano, in.UpdatedAt); err != nil {
		return PersistedInstanceOpsAgentSession{}
	}
	decoded, err := hex.DecodeString(in.ConversationAnchor)
	if err != nil || len(decoded) != 32 || strings.ToLower(in.ConversationAnchor) != in.ConversationAnchor {
		return PersistedInstanceOpsAgentSession{}
	}
	return in
}

func persistedInstanceOpsAgentSessionFresh(in PersistedInstanceOpsAgentSession, now time.Time) bool {
	in = normalizePersistedInstanceOpsAgentSession(in)
	if in.IsZero() {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, in.UpdatedAt)
	if err != nil || updated.After(now.Add(time.Minute)) {
		return false
	}
	return now.Sub(updated) <= instanceOpsAgentSessionTTL
}

// instanceOpsAgentSessionForRun returns either a same-instance fresh resume cursor or a new
// server-chosen UUID. It does not persist the new UUID: only a lifecycle event proving that the SDK
// model turn began may do that.
func (e *Engine) instanceOpsAgentSessionForRun(instanceID string) *opscontext.AgentSession {
	now := time.Now().UTC()
	current := e.sessionState.PersistedInstanceOpsAgent
	if persistedInstanceOpsAgentSessionFresh(current, now) &&
		strings.EqualFold(current.InstanceID, strings.TrimSpace(instanceID)) {
		return &opscontext.AgentSession{
			SessionID:          current.SessionID,
			WorkdirID:          current.WorkdirID,
			Contract:           current.Contract,
			Model:              current.Model,
			Resume:             true,
			ConversationAnchor: current.ConversationAnchor,
		}
	}
	sessionID := uuid.NewString()
	return &opscontext.AgentSession{
		SessionID: sessionID,
		WorkdirID: sessionID,
		Contract:  instanceOpsAgentSessionContract,
		Resume:    false,
	}
}

// observeInstanceOpsAgentSession persists only the SDK cursor after the harness has observed a
// genuine model event. No transcript, command, output or authorization rides this state.
func (e *Engine) observeInstanceOpsAgentSession(instanceID, sessionID, workdirID, contract, model, conversationAnchor string) {
	if e == nil || strings.TrimSpace(instanceID) == "" || contract != instanceOpsAgentSessionContract {
		return
	}
	candidate := normalizePersistedInstanceOpsAgentSession(PersistedInstanceOpsAgentSession{
		InstanceID:         strings.TrimSpace(instanceID),
		SessionID:          strings.TrimSpace(sessionID),
		WorkdirID:          strings.TrimSpace(workdirID),
		Contract:           contract,
		Model:              strings.TrimSpace(model),
		ConversationAnchor: strings.TrimSpace(conversationAnchor),
		UpdatedAt:          time.Now().UTC().Format(time.RFC3339Nano),
	})
	if candidate.IsZero() {
		return
	}
	e.sessionState.PersistedInstanceOpsAgent = candidate
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}
