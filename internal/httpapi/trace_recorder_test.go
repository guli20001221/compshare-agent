package httpapi

import (
	"errors"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatTraceRecorderMarksChatError(t *testing.T) {
	writer := &captureTraceWriter{}
	recorder := newChatTraceRecorder(
		writer,
		BaseRequest{
			RequestUUID: "req-error",
			Owner: store.Owner{
				TopOrganizationID: 1,
				OrganizationID:    2,
			},
		},
		"sess-error",
		1,
		"hi",
		time.Now(),
	)

	err := recorder.Finish(errors.New("boom"), time.Now())
	require.NoError(t, err)
	require.Len(t, writer.records, 1)
	assert.Equal(t, observability.EngineHardBlockTrace{
		Hit:      true,
		Category: "chat_error",
	}, writer.records[0].EngineHardBlock)
}

func TestChatTraceRecorderPreservesContextDecisionInStateTrace(t *testing.T) {
	writer := &captureTraceWriter{}
	recorder := newChatTraceRecorder(
		writer,
		BaseRequest{
			RequestUUID: "req-context-decision",
			Owner: store.Owner{
				TopOrganizationID: 1,
				OrganizationID:    2,
			},
		},
		"sess-context-decision",
		1,
		"那华北二A呢",
		time.Now(),
	)

	recorder.SetContextDecisionTrace(engine.ContextDecisionTrace{
		Decision:       engine.ContextDecisionContinueTask,
		Target:         engine.ContextDecisionTargetCreate,
		Reason:         "followup",
		ActiveTaskKind: "deploy",
		ReadSet:        []string{"user_text", "router_intent", "active_task"},
		StateDelta:     []string{"task:continue"},
		ToolScope:      "named",
		ToolNames:      []string{"CreateInstanceWorkflow"},
	})
	recorder.SetStateTrace(observability.StateTrace{
		SessionStateHydrated: true,
		ResolutionSource:     observability.ResolutionSourceSessionState,
	})

	err := recorder.Finish(nil, time.Now())
	require.NoError(t, err)
	require.Len(t, writer.records, 1)
	state := writer.records[0].State
	assert.Equal(t, engine.ContextDecisionContinueTask, state.ContextDecision)
	assert.Equal(t, engine.ContextDecisionTargetCreate, state.ContextDecisionTarget)
	assert.Equal(t, "followup", state.ContextDecisionReason)
	assert.Equal(t, "deploy", state.ContextDecisionActiveTask)
	assert.Equal(t, []string{"user_text", "router_intent", "active_task"}, state.ContextDecisionReadSet)
	assert.Equal(t, []string{"task:continue"}, state.ContextDecisionStateDelta)
	assert.Equal(t, "named", state.ContextDecisionToolScope)
	assert.Equal(t, []string{"CreateInstanceWorkflow"}, state.ContextDecisionToolNames)
	assert.True(t, state.SessionStateHydrated)
	assert.Equal(t, observability.ResolutionSourceSessionState, state.ResolutionSource)
}
