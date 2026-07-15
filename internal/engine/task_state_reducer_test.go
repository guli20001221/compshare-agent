package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

func stringList(values ...string) *[]string { return &values }

func TestTaskStateReducerOwnsReplaceContinueCompleteAndClear(t *testing.T) {
	now := time.Unix(100, 0)
	replaced, err := reduceTaskState(TaskSnapshot{}, TaskStateDelta{Relation: TaskRelationReplace, Task: &TaskDeltaBody{
		Goal: "扩容训练机", Intent: "operation_lifecycle", Workflow: "ResizeDiskWorkflow", Stage: "collecting",
		Constraints: stringList("不中断训练"), MissingSlots: stringList("Size"),
	}}, now)
	require.NoError(t, err)
	require.Equal(t, TaskSnapshotStatusActive, replaced.Status)
	require.Equal(t, []string{"不中断训练"}, replaced.Constraints)

	continued, err := reduceTaskState(replaced, TaskStateDelta{Relation: TaskRelationContinue, Task: &TaskDeltaBody{Stage: "ready"}}, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, "ready", continued.Stage)
	require.Equal(t, []string{"不中断训练"}, continued.Constraints, "an omitted collection must not erase durable context")
	require.Equal(t, []string{"Size"}, continued.MissingSlots)

	_, err = reduceTaskState(continued, TaskStateDelta{Relation: TaskRelationContinue, Task: &TaskDeltaBody{Workflow: "StopInstanceWorkflow"}}, now)
	require.ErrorContains(t, err, "conflicts")

	completed, err := reduceTaskState(continued, TaskStateDelta{Relation: TaskRelationComplete}, now.Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, TaskSnapshotStatusResolved, completed.Status)

	cleared, err := reduceTaskState(completed, TaskStateDelta{Relation: TaskRelationClear}, now)
	require.NoError(t, err)
	require.True(t, taskSnapshotEmpty(cleared))
}

func TestUpdateTaskStatePersistsIntoNextAgentContextWithoutExecutor(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.enableCentralAgentRuntimeForTest()
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	out := eng.executeTool(context.Background(), toolCall("task", tools.UpdateTaskStateName,
		`{"relation":"replace","task":{"goal":"给训练机扩容","intent":"operation_lifecycle","workflow":"ResizeDiskWorkflow","stage":"collecting","missing_slots":["Size"]}}`), noopStep)
	var result TaskStateResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.True(t, result.Applied)
	require.Empty(t, executor.calls)

	view := (ContextCompiler{}).CompileForTurn(eng, "改成 200G", "next-turn", time.Now())
	require.NotNil(t, view.ActiveTask)
	require.Equal(t, "给训练机扩容", view.ActiveTask.Goal)
	require.Equal(t, []string{"Size"}, view.ActiveTask.MissingSlots)
}

func TestCentralToolWindowSeparatesUnderstandingFromWriteAuthority(t *testing.T) {
	readOnly := toolNames(centralAgentToolWindow(false))
	require.Contains(t, readOnly, tools.UpdateTaskStateName)
	require.NotContains(t, readOnly, tools.ProposeActionName)
	require.NotContains(t, readOnly, "StopInstanceWorkflow")

	mutating := toolNames(centralAgentToolWindow(true))
	require.Contains(t, mutating, tools.UpdateTaskStateName)
	require.Contains(t, mutating, tools.ProposeActionName)
	require.NotContains(t, mutating, "StopInstanceWorkflow")
}

func TestUpdateTaskStateRejectsUnstructuredAndLegacyExecution(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.enableCentralAgentRuntimeForTest()
	out := eng.executeTaskStateDelta(map[string]any{"relation": "continue", "raw_user_text": "就用之前那个"}, noopStep)
	require.Contains(t, out, "unknown field")

	legacy := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	out = legacy.executeTool(context.Background(), toolCall("task", tools.UpdateTaskStateName,
		`{"relation":"clear"}`), noopStep)
	require.Contains(t, out, "not enabled")
}
