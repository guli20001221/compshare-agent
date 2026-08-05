package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshSystemPrompt_InjectsSelectedInstance(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("暂无用户信息")

	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-abc123",
		SelectedInstanceName: "my-gpu-box",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	assert.Contains(t, modelInput, "my-gpu-box uhost-abc123",
		"unknown-age legacy identity must remain available for understanding")
	assert.Contains(t, modelInput, "不授权任何写操作")
}

func TestRefreshSystemPrompt_SkipsWhenNotHydrated(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user context")

	before := eng.messages[0].Content
	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	assert.Equal(t, before, eng.messages[0].Content,
		"system prompt must not change when session state is not hydrated")
}

func TestRefreshSystemPrompt_IDOnlyWhenNameEmpty(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("")

	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-xyz789",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	assert.Contains(t, modelInput, "相关对象：uhost-xyz789")
}

func TestRefreshSystemPrompt_PreservesBaseUserContext(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("您有 3 个实例（2 个运行中、1 个其他状态）")

	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-111",
		SelectedInstanceName: "train-node-1",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	assert.True(t, strings.Contains(modelInput, "您有 3 个实例"),
		"base user context must be preserved")
	assert.True(t, strings.Contains(modelInput, "train-node-1 uhost-111"),
		"legacy session identity must be appended without restoring write trust")
}

func TestRefreshSystemPrompt_ClearsStaleInstance(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "turn1"},
		{Content: "turn2"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("")

	// Turn 1: inject selected instance via SessionState.
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-stale",
		SelectedInstanceName: "stale-box",
	}, 1)
	_, err := eng.ChatWithOptions(context.Background(), "turn1", noopStep, ChatOptions{})
	require.NoError(t, err)
	assert.Contains(t, renderTestMessages(mock.calls[0].Messages), "uhost-stale",
		"turn 1 model context must contain selected instance")

	// Turn 2: ClearSessionState (mirrors HTTP handler flow), then
	// SetSessionState with empty instance — simulating a turn where the
	// persisted state no longer has a selected instance.
	eng.ClearSessionState()
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 2)
	_, err = eng.ChatWithOptions(context.Background(), "turn2", noopStep, ChatOptions{})
	require.NoError(t, err)
	assert.NotContains(t, renderTestMessages(mock.calls[1].Messages), "uhost-stale",
		"turn 2 model context must NOT contain stale instance from turn 1")
	assert.NotContains(t, renderTestMessages(mock.calls[1].Messages), "stale-box",
		"turn 2 model context must NOT contain stale instance name")
}

// TestFreshFactsNoLongerReachTheModelThroughTheContextCard replaces
// TestAgentContextIncludesFreshFactsWithoutLegacyPromptFlag, which asserted the
// behavior this deletion removed: with the legacy fact-context flag off, a fresh
// fact used to reach the model anyway, through the context card's 近期可信观测
// projection.
//
// That projection restated a tool result the canonical transcript now replays
// verbatim, so it is gone. What still carries a fresh fact into the model is the
// session fact context (USE_SESSION_FACT_CONTEXT, which the deploy config ships
// ON — see TestRefreshSystemPrompt_FactContextFlagOn) and the transcript itself.
// With BOTH off, as here, nothing does, and that is the intended contract rather
// than an accident: the card is no longer a second memory of prior tool results.
//
// What the deletion did NOT take is the staleness notice — see
// TestContextCompilerExpiresVolatileFactsWithoutPresentingTheirValues. The
// transcript replays the original tool output and that output has no expiry in
// it, so the TTL remains the only thing that knows a replayed number is stale.
func TestFreshFactsNoLongerReachTheModelThroughTheContextCard(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user context")
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		RecentFacts: []ToolFact{{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-fact-off",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     factTTLSecondsInstanceState,
			Payload:        map[string]any{"name": "off-box", "state": "Running"},
		}},
	}, 1)
	require.Len(t, eng.sessionState.RecentFacts, 1,
		"premise: the fact must actually be stored, or the assertions below pass on nothing")

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	require.Contains(t, modelInput, "test user context",
		"premise: the prompt was assembled; without this the NotContains below are free")

	assert.NotContains(t, modelInput, "off-box",
		"the card no longer restates a prior tool result; the transcript replays the original")
	assert.NotContains(t, modelInput, "近期可信观测",
		"the projection is deleted, not gated — no flag position brings it back")
}

func TestRefreshSystemPrompt_FactContextFlagOn(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user context")
	eng.SetSessionFactContextEnabled(true)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		RecentFacts: []ToolFact{{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-fact-on",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     factTTLSecondsInstanceState,
			Payload: map[string]any{
				"name":  "on-box",
				"state": "Running",
			},
		}},
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "hello", noopStep, ChatOptions{})
	require.NoError(t, err)

	modelInput := renderTestMessages(mock.calls[0].Messages)
	require.Contains(t, modelInput, "test user context",
		"premise: the prompt was assembled, so the NotContains below are not free")

	// USE_SESSION_FACT_CONTEXT never injected anything of its own. The only path
	// that put a RecentFact in front of the model was the context card's
	// 近期可信观测 projection, which is why the flag-OFF test asserted the same
	// text — and that projection is now deleted. assembleFactContext survives as a
	// boolean in refreshSystemPrompt, so what the flag still decides is the
	// TRACE's instance-resolution source, nothing the model reads.
	assert.NotContains(t, modelInput, "on-box")
	assert.NotContains(t, modelInput, "近期可信观测")
	assert.Equal(t, observability.ResolutionSourceFactCache, eng.instanceResolutionSourceThisTurn,
		"the flag's remaining job is labelling how the turn's instance binding was determined")
}

func TestRefreshSystemPrompt_FactContextDoesNotAccumulate(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "turn1"}, {Content: "turn2"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user context")
	eng.SetSessionFactContextEnabled(true)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		RecentFacts: []ToolFact{{
			Kind:           FactKindInstanceState,
			SubjectID:      "uhost-once",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     factTTLSecondsInstanceState,
			Payload:        map[string]any{"name": "once-box"},
		}},
		// A live selection so the card has something to render. Without it the
		// card is only its header, renderAgentContextCard returns "", and the
		// count below is 0 on both turns for a reason that has nothing to do
		// with accumulation.
		SelectedInstanceID:   "uhost-live",
		SelectedInstanceName: "live-box",
	}, 1)

	_, err := eng.ChatWithOptions(context.Background(), "turn1", noopStep, ChatOptions{})
	require.NoError(t, err)
	_, err = eng.ChatWithOptions(context.Background(), "turn2", noopStep, ChatOptions{})
	require.NoError(t, err)

	for _, call := range mock.calls {
		// Counted on the card header rather than on 近期可信观测: that block was the
		// marker until the projection was deleted, and the property under test —
		// the card is rebuilt once per turn instead of appended — is unchanged.
		assert.Equal(t, 1, strings.Count(renderTestMessages(call.Messages), "【本轮统一上下文"),
			"AgentContext must be rebuilt once per turn, not appended repeatedly")
	}
}
