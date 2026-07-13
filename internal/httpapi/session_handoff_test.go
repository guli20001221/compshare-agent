package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session is refused at agent.http.max_session_turns (default 10) QA pairs, and the user
// opens a new one that is born EMPTY — so the agent has never read the conversation still on
// the user's screen. That is amnesia by design, and the production export shows it is not
// rare: 24 of 439 sessions land on exactly the cap and NONE goes past it (5 stop at 9, so the
// spike is truncation, not conversations ending), those sessions hold 21% of all user turns,
// and 10 of the 24 owners opened a new session within FIVE MINUTES of the wall.
//
// These tests pin the whole contract, including the parts that must NOT change.

// cappedSessionHandlers builds a Handlers whose session is exactly at the 10-turn cap and
// whose message store holds `turns` completed QA pairs.
func cappedSessionHandlers(t *testing.T, priorState engine.SessionState, turns int) (*Handlers, *mockSessions, *recordingPool) {
	t.Helper()

	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	pool := &recordingPool{eng: eng}

	priorCtx, err := json.Marshal(engine.PersistedContext{AgentSessionState: priorState})
	require.NoError(t, err)

	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-capped": {
			ID:                "sess-capped",
			TopOrganizationID: 1,
			OrganizationID:    2,
			Context:           priorCtx,
			MessageCount:      turns * 2, // each completed turn persists a user + an assistant row
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}

	msgs := make([]store.Message, 0, turns*2)
	for i := 1; i <= turns; i++ {
		msgs = append(msgs,
			store.Message{SessionID: "sess-capped", Role: "user", Status: "ok", Content: fmt.Sprintf("问题%d", i)},
			store.Message{SessionID: "sess-capped", Role: "assistant", Status: "ok", Content: fmt.Sprintf("回答%d", i)},
		)
	}

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{mockMessages: mockMessages{list: msgs}},
		mockFeedback{},
		pool,
		nil,
	)
	return h, sessions, pool
}

const cappedChatReq = `{"Action":"SendCSAgentChat","SessionId":"sess-capped","Message":"那怎么办","request_uuid":"req-cap","top_organization_id":1,"organization_id":2}`

// Flag OFF is the behavior that ships today, and it must stay byte-identical: the turn is
// refused and nothing is rolled over. This is the rollback path, so it is pinned first.
func TestTurnCap_FlagOff_StillRefuses(t *testing.T) {
	h, sessions, pool := cappedSessionHandlers(t, engine.SessionState{}, 10)

	_, apiErr := runChatJSON(t, h, cappedChatReq)

	require.NotNil(t, apiErr, "at the cap with handoff off, the turn must still be refused")
	assert.Equal(t, ErrSessionTurnLimit.Code, apiErr.Code)
	assert.Empty(t, pool.sessionID, "a refused turn must never lease an engine")
	_, created := sessions.byID[successorSessionID("sess-capped")]
	assert.False(t, created, "handoff is off — no successor session may be created")
}

// The fix. The turn is answered, in a successor that was handed the conversation.
func TestTurnCap_RollsOverIntoASuccessorThatHasTheConversation(t *testing.T) {
	prior := engine.SessionState{
		SchemaVersion:      engine.SessionStateSchemaCurrent,
		SelectedInstanceID: "uhost-1exampleaa01",
		LastIntent:         "diagnosis",
	}
	h, sessions, pool := cappedSessionHandlers(t, prior, 10)
	h.SetSessionHandoffEnabled(true)

	sink, apiErr := runChatJSON(t, h, cappedChatReq)
	require.Nil(t, apiErr, "the turn must be ANSWERED, not refused")
	require.True(t, sink.has("done"))

	// The turn ran in the successor, and the client is told which one — via the SAME meta
	// frame the front end already adopts. No new protocol.
	wantID := successorSessionID("sess-capped")
	assert.Equal(t, []string{wantID}, pool.sessionID, "the turn must run in the successor")
	assert.Equal(t, wantID, firstMetaEvent(t, sink).SessionID,
		"the client must learn the new session id, or it keeps writing to the capped one")

	// THE INVARIANT. The successor's engine was handed the conversation. Without this the
	// rollover is strictly worse than the refusal: the user gets an answer from an agent that
	// has read nothing, which is the amnesia this exists to remove, not a softer refusal.
	require.NotEmpty(t, pool.handoff, "the successor's engine must be handed the conversation")
	assert.Equal(t, "回答10", pool.handoff[len(pool.handoff)-1].Content,
		"the carried tail must END at the last thing the agent actually said")
	assert.Len(t, pool.handoff, engine.SessionHandoffMessages,
		"the tail is BOUNDED — carrying all 20 would hand the successor the cost the cap exists to bound")

	// The handoff is PERSISTED, not just seeded in memory. The pool evicts at capacity / 30 min
	// idle; a handoff that lived only in the engine would vanish on the first cold rebuild —
	// recreating this very bug two turns later instead of immediately.
	successor := sessions.byID[wantID]
	pc, err := engine.ParsePersistedContext(successor.Context)
	require.NoError(t, err)
	require.NotNil(t, pc.Handoff, "the successor must carry its handoff in its own envelope")
	assert.Equal(t, "sess-capped", pc.Handoff.FromSessionID)
	assert.Len(t, pc.Handoff.Messages, engine.SessionHandoffMessages)

	// The structured state comes across too — a user cut off mid-troubleshoot must not lose the
	// instance they had selected.
	assert.Equal(t, "uhost-1exampleaa01", pc.AgentSessionState.SelectedInstanceID)
	assert.Equal(t, "diagnosis", pc.AgentSessionState.LastIntent)
}

// The successor's SECOND turn. This is the bug I nearly shipped: the prefix was passed only on
// the rollover turn, so once the pool evicted the successor (capacity / 30 min idle) its cold
// rebuild had no handoff and the conversation vanished — the same empty-session bug, just two
// turns later. The handoff must be re-read from the successor's own envelope EVERY turn.
func TestTurnCap_SuccessorKeepsItsHandoffOnLaterTurns(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	pool := &recordingPool{eng: eng}

	carried := []engine.HistoryMessage{
		{Role: "user", Content: "问题9"},
		{Role: "assistant", Content: "回答9"},
		{Role: "user", Content: "问题10"},
		{Role: "assistant", Content: "回答10"},
	}
	ctxJSON, err := json.Marshal(engine.PersistedContext{
		AgentSessionState: engine.SessionState{SchemaVersion: engine.SessionStateSchemaCurrent},
		Handoff:           &engine.SessionHandoff{FromSessionID: "sess-capped", Messages: carried},
	})
	require.NoError(t, err)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			// A successor mid-life: well under the cap, so this turn does NOT roll over.
			"sess-rolled": {
				ID: "sess-rolled", TopOrganizationID: 1, OrganizationID: 2,
				Context: ctxJSON, MessageCount: 2,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			},
		}},
		&recordingMessages{},
		mockFeedback{},
		pool,
		nil,
	)
	h.SetSessionHandoffEnabled(true)

	_, apiErr := runChatJSON(t, h,
		`{"Action":"SendCSAgentChat","SessionId":"sess-rolled","Message":"继续","request_uuid":"req-2","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)

	assert.Equal(t, carried, pool.handoff,
		"a rollover's history must be re-supplied on EVERY turn — the pool rebuilds the engine cold whenever it evicts it, and a prefix given only once is lost there")
}

// handoffTail is where a half-finished turn could leak into the new conversation as though it
// had been answered. It must apply the same status/role filter that rehydration does.
func TestHandoffTail_SkipsRowsRehydrationWouldSkip(t *testing.T) {
	got := handoffTail([]store.Message{
		{Role: "user", Status: "ok", Content: "keep me"},
		{Role: "assistant", Status: "error", Content: "the turn that failed"},
		{Role: "user", Status: "pending", Content: "the question we never answered"},
		{Role: "system", Status: "ok", Content: "not a conversational turn"},
		{Role: "assistant", Status: "ok", Content: "   "},
		{Role: "assistant", Status: "ok", Content: "keep me too"},
	}, engine.SessionHandoffMessages)

	assert.Equal(t, []engine.HistoryMessage{
		{Role: "user", Content: "keep me"},
		{Role: "assistant", Content: "keep me too"},
	}, got, "an errored/pending/system/empty row must never be carried forward as a real turn")
}
