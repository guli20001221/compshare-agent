package httpapi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatTraceRecorderReceivesEngineCompletion(t *testing.T) {
	writer := &captureTraceWriter{}
	recorder := newChatTraceRecorder(
		writer,
		BaseRequest{RequestUUID: "req-completion", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}},
		"sess-completion",
		1,
		"介绍平台",
		time.Now(),
	)
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	attachChatTraceObservers(eng, recorder)
	t.Cleanup(func() { clearChatTraceObservers(eng) })

	_, chatErr := eng.Chat(context.Background(), "介绍平台", nil)
	require.NoError(t, chatErr)
	require.NoError(t, recorder.Finish(nil, time.Now()))
	require.Len(t, writer.records, 1)
	got := writer.records[0]
	assert.Equal(t, observability.CompletionClassAgent, got.Completion.Class)
	assert.Equal(t, observability.CompletionReasonAgentLoop, got.Completion.Reason)
	assert.Equal(t, observability.TerminatedByDone, got.Outcome.TerminatedBy)
}

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
	recorder.SetEngineSnapshot(engine.TraceSnapshot{ResponseContract: "agent"}, false)

	err := recorder.Finish(errors.New("boom"), time.Now())
	require.NoError(t, err)
	require.Len(t, writer.records, 1)
	assert.Equal(t, observability.EngineHardBlockTrace{
		Hit:      true,
		Category: "chat_error",
	}, writer.records[0].EngineHardBlock)
	assert.Equal(t, "failure", writer.records[0].Outcome.ResponseContract)
}

func TestChatTraceRecorderProjectsOneEngineSnapshotIntoTheRootTrace(t *testing.T) {
	writer := &captureTraceWriter{}
	recorder := newChatTraceRecorder(
		writer,
		BaseRequest{RequestUUID: "req-snapshot", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}},
		"sess-snapshot",
		1,
		"hi",
		time.Now(),
	)
	recorder.SetEngineSnapshot(engine.TraceSnapshot{
		Registry: observability.EntityRegistryTrace{
			SnapshotID: "registry-snapshot",
			AgeSeconds: 12,
			SyncEvent:  "describe_instances",
		},
		ReactRounds:               3,
		ActionProposalDisposition: "rejected:Zone=invalid_value",
		SessionState: engine.SessionState{
			SelectedInstanceID:        "uhost-final",
			SelectedInstanceSource:    engine.SelectedInstanceSourceObserved,
			SelectedInstanceFreshness: engine.ContinuityFreshnessFresh,
		},
		SessionStateHydrated:             true,
		ResolutionSource:                 observability.ResolutionSourceSessionState,
		SelectedInstanceIDAtStart:        "uhost-start",
		SelectedInstanceSourceAtStart:    engine.SelectedInstanceSourceUser,
		SelectedInstanceFreshnessAtStart: engine.ContinuityFreshnessStale,
		ContextSources:                   []string{"recent_pairs", "selected_entities"},
		ResponseContract:                 "agent",
		PromptSectionIDs:                 []string{"identity", "tool_use"},
		EvidenceUpdateSource:             "none",
		GroundingOutcome:                 "supported",
		GroundingCitationScope:           "mixed",
		PromptMessagesRawPeak:            19,
		PromptMessagesAssembledPeak:      15,
		PromptMessagesCapApplied:         true,
	}, true)

	require.NoError(t, recorder.Finish(nil, time.Now()))
	require.Len(t, writer.records, 1)
	got := writer.records[0]
	assert.Equal(t, observability.EntityRegistryTrace{
		SnapshotID: "registry-snapshot",
		AgeSeconds: 12,
		SyncEvent:  "describe_instances",
	}, got.EntityRegistry)
	assert.Equal(t, observability.StateTrace{
		SessionStateHydrated:                 true,
		ResolutionSource:                     observability.ResolutionSourceSessionState,
		SelectedInstanceID:                   "uhost-final",
		SelectedInstanceIDAtTurnStart:        "uhost-start",
		SelectedInstanceSource:               engine.SelectedInstanceSourceObserved,
		SelectedInstanceFreshness:            engine.ContinuityFreshnessFresh,
		SelectedInstanceSourceAtTurnStart:    engine.SelectedInstanceSourceUser,
		SelectedInstanceFreshnessAtTurnStart: engine.ContinuityFreshnessStale,
	}, got.State)
	assert.Equal(t, observability.TerminatedByEmptyReply, got.Outcome.TerminatedBy)
	assert.Equal(t, observability.AbortCauseLLMEmptyStream, got.Outcome.AbortCause)
	assert.Equal(t, 3, got.Outcome.ReactRounds)
	assert.Equal(t, "rejected:Zone=invalid_value", got.Outcome.ActionProposalDisposition)
	assert.Equal(t, []string{"recent_pairs", "selected_entities"}, got.Outcome.ContextSources)
	assert.Equal(t, "agent", got.Outcome.ResponseContract)
	assert.Equal(t, []string{"identity", "tool_use"}, got.Outcome.PromptSectionIDs)
	assert.Equal(t, "none", got.Outcome.EvidenceUpdateSource)
	assert.Equal(t, "supported", got.Outcome.GroundingOutcome)
	assert.Equal(t, "mixed", got.Outcome.GroundingCitationScope)
	assert.Equal(t, 19, got.Outcome.PromptMessagesRawPeak)
	assert.Equal(t, 15, got.Outcome.PromptMessagesAssembledPeak)
	assert.True(t, got.Outcome.PromptMessagesCapApplied)
}

func TestChatTraceRecorderPreservesTheModelSelectedFunction(t *testing.T) {
	recorder := newChatTraceRecorder(
		&captureTraceWriter{},
		BaseRequest{RequestUUID: "req-selected-function", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}},
		"sess-selected-function",
		1,
		"停止这台实例",
		time.Now(),
	)

	recorder.OnStep(engine.StepEvent{
		Type: engine.StepToolCall, Action: tools.ProposeActionName,
		SelectedFunctionName: "RequestStopInstance", Source: observability.ToolSourceMainReAct,
	})
	recorder.OnStep(engine.StepEvent{
		Type: engine.StepToolResult, Action: tools.ProposeActionName,
		Source: observability.ToolSourceMainReAct,
	})

	require.Len(t, recorder.record.ToolCalls, 1)
	assert.Equal(t, tools.ProposeActionName, recorder.record.ToolCalls[0].Action)
	assert.Equal(t, "RequestStopInstance", recorder.record.ToolCalls[0].SelectedFunctionName)
}

func TestChatTraceRecorderPreservesASelectionWithoutAStartedCall(t *testing.T) {
	for _, stepType := range []engine.StepType{engine.StepToolResult, engine.StepError, engine.StepBlocked} {
		t.Run(fmt.Sprintf("step-%d", stepType), func(t *testing.T) {
			recorder := newChatTraceRecorder(
				&captureTraceWriter{},
				BaseRequest{RequestUUID: "req-terminal-selection", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}},
				"sess-terminal-selection",
				1,
				"查询或操作",
				time.Now(),
			)
			recorder.OnStep(engine.StepEvent{
				Type: stepType, Action: "ProjectedAction", SelectedFunctionName: "RawSelectedFunction",
				Source: observability.ToolSourceMainReAct,
			})

			require.Len(t, recorder.record.ToolCalls, 1)
			assert.Equal(t, "RawSelectedFunction", recorder.record.ToolCalls[0].SelectedFunctionName)
		})
	}
}

func TestChatTraceRecorderKeepsARepeatedModelSelectionSeparateAfterCompletion(t *testing.T) {
	recorder := newChatTraceRecorder(
		&captureTraceWriter{},
		BaseRequest{RequestUUID: "req-repeated-selection", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}},
		"sess-repeated-selection",
		1,
		"再查一次",
		time.Now(),
	)
	recorder.OnStep(engine.StepEvent{
		Type: engine.StepToolCall, Action: "DiagnoseBilling",
		SelectedFunctionName: "DiagnoseBilling", Source: observability.ToolSourceMainReAct,
	})
	recorder.OnStep(engine.StepEvent{
		Type: engine.StepToolResult, Action: "DiagnoseBilling", Source: observability.ToolSourceMainReAct,
	})
	recorder.OnStep(engine.StepEvent{
		Type: engine.StepToolResult, Action: "DiagnoseBilling",
		SelectedFunctionName: "DiagnoseBilling", Source: observability.ToolSourceMainReAct,
	})

	require.Len(t, recorder.record.ToolCalls, 2)
	assert.Equal(t, "DiagnoseBilling", recorder.record.ToolCalls[0].SelectedFunctionName)
	assert.Equal(t, "DiagnoseBilling", recorder.record.ToolCalls[1].SelectedFunctionName)
}
