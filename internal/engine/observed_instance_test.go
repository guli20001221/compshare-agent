package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newEngineForObservedInstanceTest(t *testing.T) *Engine {
	t.Helper()
	e := NewSession(&SharedDeps{
		LLMClient:        &mockLLM{},
		RateLimiter:      governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
		ExternalExecutor: &mockExecutor{results: map[string]map[string]any{}},
	}, SessionOptions{Subject: "test-subject"})
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)
	return e
}

func TestSingleObservedInstanceRemainsReadOnlyContext(t *testing.T) {
	e := newEngineForObservedInstanceTest(t)
	e.recordObservedInstanceFromDescribe(map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-solo", "Name": "only-one"},
	}})
	assert.Equal(t, "uhost-solo", e.sessionState.SelectedInstanceID)
	assert.Equal(t, "only-one", e.sessionState.SelectedInstanceName)
	assert.Equal(t, SelectedInstanceSourceObserved, e.sessionState.SelectedInstanceSource)
}

func TestOnlyDirectToolCallsRecordObservedInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-a", "Name": "a"}}},
	}}
	e := NewSession(&SharedDeps{
		LLMClient:        &mockLLM{},
		RateLimiter:      governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
		ExternalExecutor: executor,
	}, SessionOptions{Subject: "test-subject"})
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	_, err := e.executeSafeTool(context.Background(), tools.SafeToolRequest{Action: "DescribeCompShareInstance", Args: map[string]any{"Limit": 100}, Origin: tools.OriginWorkflowInternal})
	require.NoError(t, err)
	assert.Empty(t, e.sessionState.SelectedInstanceID, "workflow-internal reads cannot create conversational references")

	_, err = e.executeSafeTool(context.Background(), tools.SafeToolRequest{Action: "DescribeCompShareInstance", Args: map[string]any{"Limit": 100}, Origin: tools.OriginDirectLLM})
	require.NoError(t, err)
	assert.Equal(t, "uhost-a", e.sessionState.SelectedInstanceID)
	assert.Equal(t, SelectedInstanceSourceObserved, e.sessionState.SelectedInstanceSource)
	persisted, err := json.Marshal(e.sessionState)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), "recent_facts",
		"a direct read records only the live selection needed by the binder")
}
