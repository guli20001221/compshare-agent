package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowArgsFromTaskSlotsMapsCommonWorkflowParameters(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		slots    map[string]string
		want     map[string]any
	}{
		{
			name:     "create data disk size",
			workflow: "CreateDiskWorkflow",
			slots:    map[string]string{"instance_id": "uhost-1", "size_gb": "200G"},
			want:     map[string]any{"UHostId": "uhost-1", "Size": float64(200)},
		},
		{
			name:     "resize disk target size",
			workflow: "ResizeDiskWorkflow",
			slots:    map[string]string{"instance_id": "uhost-1", "target_size_gb": "300GB"},
			want:     map[string]any{"UHostId": "uhost-1", "Size": float64(300)},
		},
		{
			name:     "resize instance spec",
			workflow: "ResizeInstanceWorkflow",
			slots:    map[string]string{"instance_id": "uhost-1", "cpu": "4", "memory_gb": "8", "gpu_count": "1"},
			want:     map[string]any{"UHostId": "uhost-1", "Cpu": float64(4), "Memory": float64(8), "Gpu": float64(1)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, missing := workflowArgsFromTaskSlots(tc.workflow, tc.slots)
			require.Empty(t, missing)
			assert.Equal(t, tc.want, args)
		})
	}
}

func TestWorkflowArgsFromTaskSlotsReportsMissingWithoutInventingDefaults(t *testing.T) {
	args, missing := workflowArgsFromTaskSlots("CreateDiskWorkflow", map[string]string{"instance_id": "uhost-1"})

	assert.Empty(t, args)
	assert.Equal(t, []string{"size_gb"}, missing)
}

func TestRecordWorkflowMissingSlotsFrameKeepsOnlySafeSlots(t *testing.T) {
	eng := newEngineForSessionStateTest(t)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	recorded := eng.recordWorkflowMissingSlotsFrame("CreateDiskWorkflow", map[string]any{
		"UHostId":  "uhost-1",
		"zone_id":  float64(5001),
		"az_group": "cn-wlcb",
		"Password": "secret",
	}, []string{"size_gb"}, "缺少大小")

	require.True(t, recorded)
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateDiskWorkflow", frame.Workflow)
	assert.Equal(t, []string{"size_gb"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
	assert.NotContains(t, frame.Slots, "zone_id")
	assert.NotContains(t, frame.Slots, "az_group")
	assert.NotContains(t, frame.Slots, "password")
}

func TestExecuteWorkflowMissingSlotRecordsGenericTaskFrame(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	reply := eng.executeWorkflow(context.Background(), "CreateDiskWorkflow", map[string]any{
		"UHostId": "uhost-1",
	}, noopStep)

	assert.Contains(t, reply, "数据盘大小")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateDiskWorkflow", frame.Workflow)
	assert.Equal(t, []string{"size_gb"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestOperationLifecycleMissingDiskSizeRecordsGenericTaskFrame(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1",
				"Name":    "host-1",
				"State":   "Running",
				"Zone":    "cn-wlcb-01",
				"Region":  "cn-wlcb",
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionCreateDisk,
				TargetRefs: []intent.TargetRef{{
					Type:   intent.TargetRefUHostIDUserInput,
					Value:  "uhost-1",
					Source: intent.SourceUserText,
				}},
			},
		}},
		snapshot: entity.RegistrySnapshot{
			Instances: map[string]entity.InstanceSnapshot{
				"uhost-1": {UHostId: "uhost-1", Name: "host-1", State: "Running", Zone: "cn-wlcb-01", Region: "cn-wlcb"},
			},
			LastFullSync: time.Now(),
		},
	}

	reply, handled := eng.tryOperationLifecycleDispatch(context.Background(), dispatch, "给 host-1 加一块数据盘", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "数据盘大小")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateDiskWorkflow", frame.Workflow)
	assert.Equal(t, []string{"size_gb"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestOperationLifecycleMissingResizeSpecRecordsGenericTaskFrame(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1",
				"Name":    "host-1",
				"State":   "Stopped",
				"Zone":    "cn-wlcb-01",
				"Region":  "cn-wlcb",
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionResize,
				TargetRefs: []intent.TargetRef{{
					Type:   intent.TargetRefUHostIDUserInput,
					Value:  "uhost-1",
					Source: intent.SourceUserText,
				}},
			},
		}},
		snapshot: entity.RegistrySnapshot{
			Instances: map[string]entity.InstanceSnapshot{
				"uhost-1": {UHostId: "uhost-1", Name: "host-1", State: "Stopped", Zone: "cn-wlcb-01", Region: "cn-wlcb"},
			},
			LastFullSync: time.Now(),
		},
	}

	reply, handled := eng.tryOperationLifecycleDispatch(context.Background(), dispatch, "把 host-1 改配一下", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "目标配置")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "ResizeInstanceWorkflow", frame.Workflow)
	assert.Equal(t, []string{"cpu", "memory_gb", "gpu_count"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestSelectedInstanceCreateDiskMissingSizeRecordsGenericTaskFrame(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1",
				"Name":    "host-1",
				"State":   "Running",
				"Zone":    "cn-wlcb-01",
				"Region":  "cn-wlcb",
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-1",
		SelectedInstanceName: "host-1",
	}, 1)

	reply, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "给这台加一块数据盘", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "数据盘大小")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateDiskWorkflow", frame.Workflow)
	assert.Equal(t, []string{"size_gb"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestResumeWorkflowContextFrameAppliesSlotUpdateAndReachesConfirm(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmAction string
	var confirmArgs map[string]any
	confirm := func(action string, args map[string]any) bool {
		confirmAction = action
		confirmArgs = args
		return false
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1",
				"State":   "Running",
				"Zone":    "cn-wlcb-01",
				"Region":  "cn-wlcb",
				"GpuType": "4090",
				"GPU":     float64(1),
				"CPU":     float64(16),
				"Memory":  float64(64),
			}}}, nil
		case "GetCompShareInstancePrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"Disks": float64(0.25)}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			MissingSlots:   []string{"size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"size_gb": "200G"},
	}})
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "200G", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateDiskWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, float64(200), confirmArgs["disk_size_gb"])
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind, "filled workflow task should not keep the stale missing-slot frame after reaching confirm")
}

func TestResumeWorkflowContextFrameUsesDeclaredMissingSlotWithoutLLM(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmAction string
	confirm := func(action string, args map[string]any) bool {
		confirmAction = action
		return false
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1",
				"State":   "Running",
				"Zone":    "cn-wlcb-01",
				"Region":  "cn-wlcb",
				"GpuType": "4090",
				"GPU":     float64(1),
				"CPU":     float64(16),
				"Memory":  float64(64),
			}}}, nil
		case "GetCompShareInstancePrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"Disks": float64(0.25)}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			MissingSlots:   []string{"size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentUnknown}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "200G", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateDiskWorkflow", confirmAction)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}

func TestResumeWorkflowContextFrame_ContextContinuationFlagOffDoesNotResumeMutatingWorkflow(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	layer := &fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"size_gb": "200G"},
	}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, okConfirm)
	eng.SetContextDecisionLayer(layer)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			MissingSlots:   []string{"size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "200G", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	assert.Empty(t, layer.calls, "flag-off must not call the context decision layer")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindWorkflowTask, state.ContextFrame.Kind, "flag-off should leave the pending task for legacy handling or a later enabled turn")
}
