package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// M2 — HTTP fact-persist end-to-end test
//
// Reviewer (P3, 2026-05-24) pointed out that the engine unit tests for
// the ToolFact writer plus the engine cutover integration test (in
// internal/engine) leave one surface uncovered: the full HTTP handleChat
// flow where hydrate → tool call inside ReAct → writer fires →
// SSE done → UpdateContext persists an envelope whose
// agent_session_state.recent_facts contains the fact.
//
// This test drives that flow end-to-end with a fake LLM that issues
// a DescribeCompShareInstance tool call on the first round and a final
// text reply on the second round, and a fake executor that returns
// a canned UHostSet for that action.
//
// Mutation reasoning:
//   - Remove the recordToolFacts call at engine.go:2334 → this test
//     fails on the assert.Len(facts, 1) line.
//   - Change executeSafeTool's OriginDirectLLM gate to also accept
//     workflow-internal → still passes (writer fires either way for
//     this test), but TestExecuteSafeTool_OriginWorkflowInternal_*
//     in the engine package would fail.
// ---------------------------------------------------------------------------

// factWritingLLM is a two-round mock: round 1 emits a tool_call for
// DescribeCompShareInstance; round 2 emits a final text reply. Used by
// TestSessionState_Persist_RecentFactsRoundTrip below.
type factWritingLLM struct{ round int }

func (m *factWritingLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.round++
	if m.round == 1 {
		return &llm.ChatResponse{
			ToolCalls: []openai.ToolCall{{
				ID:   "call-1",
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      "DescribeCompShareInstance",
					Arguments: `{"Limit":100}`,
				},
			}},
			Usage: llm.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}, nil
	}
	if req.OnTextDelta != nil {
		req.OnTextDelta("ok")
	}
	return &llm.ChatResponse{
		Content: "ok",
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

// factWritingExecutor returns a single-host UHostSet for
// DescribeCompShareInstance, so the engine's recordInstanceStateFacts
// path produces exactly one fact for SubjectID="uhost-e2e".
type factWritingExecutor struct{}

func (factWritingExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	if action == "DescribeCompShareInstance" {
		return map[string]any{
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-e2e",
					"Name":    "e2e-host",
					"State":   "Running",
					"GPU":     1,
					"GpuType": "RTX4090",
					"CPU":     16,
					"Memory":  65536,
					"Zone":    "cn-wlcb-01",
				},
			},
		}, nil
	}
	return map[string]any{}, nil
}

// TestSessionState_Persist_RecentFactsRoundTrip exercises the full chain:
//
//	handleChat (hydrate)
//	  → engine.ChatWithOptions
//	    → ReAct: LLM emits tool_call DescribeCompShareInstance
//	      → executeSafeTool (OriginDirectLLM)
//	        → recordToolFacts → recordInstanceStateFacts writes one fact
//	    → ReAct: LLM emits final text → return reply
//	  → SSE done
//	  → handleChat (persist branch): UpdateContext with envelope
//
// The persisted envelope's agent_session_state.recent_facts must contain
// exactly one fact with kind=instance_state, subject=uhost-e2e.
func TestSessionState_Persist_RecentFactsRoundTrip(t *testing.T) {
	llmFake := &factWritingLLM{}
	eng := engine.NewWithDeps(llmFake, factWritingExecutor{}, denyConfirm)
	eng.RehydrateHistory(nil)
	sess := store.Session{
		ID:                "sess-fact-persist",
		TopOrganizationID: 1,
		OrganizationID:    2,
		ContextVersion:    0,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	h := newChatTestHandlersWith(t, eng, sessions)

	rec := dispatchChatTurn(t, h, sess.ID, "show me my instances")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")
	require.Equal(t, 1, sessions.updateContextCalls,
		"expected exactly one UpdateContext on a successful turn with tool call")

	row := sessions.byID[sess.ID]
	require.Equal(t, 1, row.ContextVersion, "context_version must advance 0 → 1")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(row.Context, &pc),
		"persisted Context must decode into envelope")

	facts := pc.AgentSessionState.RecentFacts
	require.Len(t, facts, 1, "exactly one instance_state fact must be persisted")
	assert.Equal(t, engine.FactKindInstanceState, facts[0].Kind)
	assert.Equal(t, "uhost-e2e", facts[0].SubjectID)
	assert.Equal(t, "e2e-host", facts[0].Payload["name"])
	assert.Equal(t, "Running", facts[0].Payload["state"])
	assert.Equal(t, float64(1), facts[0].Payload["gpu"], "numeric must round-trip as float64 (toFactNumeric)")
	assert.Equal(t, "RTX4090", facts[0].Payload["gpu_type"])
	assert.Equal(t, "cn-wlcb-01", facts[0].Payload["zone"])
	assert.Greater(t, facts[0].ProducedAtUnix, int64(0),
		"ProducedAtUnix must be set by the writer (not the zero default)")
	assert.Equal(t, engine.SessionStateSchemaV1, pc.AgentSessionState.SchemaVersion)
}

// newChatTestHandlersWith is a variant of newChatTestHandlers that takes
// a pre-built engine + sessions (so the test can install a custom LLM
// before construction). Keeps the rest of the wiring identical to the
// other M1/M2 handler tests.
func newChatTestHandlersWith(t *testing.T, eng *engine.Engine, sessions *mockSessions) *Handlers {
	t.Helper()
	return NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)
}
