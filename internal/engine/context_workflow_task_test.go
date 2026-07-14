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

func TestWorkflowArgsFromTaskSlotsAllowsReinstallImagePref(t *testing.T) {
	args, missing := workflowArgsFromTaskSlots("ReinstallInstanceWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"image_pref":  "Ubuntu-nvidia 22.04",
	})

	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "ImageName": "Ubuntu-nvidia 22.04"}, args)
}

func TestRecordWorkflowMissingSlotsFrameKeepsOnlySafeSlots(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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
	assert.NotContains(t, frame.SlotSources, "instance_id", "model-provided target from workflow args is not trusted without user source")
}

func TestRecordWorkflowMissingSlotsFrameMarksNameMentionedTargetTrusted(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := newEngineForSessionStateTest(t)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "给训练机A加一块数据盘"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "训练机A", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "训练机B", "State": "Running"},
		},
	}, "test"))

	recorded := eng.recordWorkflowMissingSlotsFrame("CreateDiskWorkflow", map[string]any{
		"UHostId": "uhost-a",
	}, []string{"size_gb"}, "缺少大小")

	require.True(t, recorded)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-a", state.ContextFrame.Slots["instance_id"])
	assert.Equal(t, SelectedInstanceSourceUser, state.ContextFrame.SlotSources["instance_id"])
}

func TestRecordWorkflowMissingSlotsFrameUsesCurrentSelectedInstanceOverStaleTask(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := newEngineForSessionStateTest(t)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-new",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			Slots:          map[string]string{"instance_id": "cpod-old"},
			MissingSlots:   []string{"size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 2)

	recorded := eng.recordWorkflowMissingSlotsFrame("CreateDiskWorkflow", nil, []string{"size_gb"}, "缺少大小")

	require.True(t, recorded)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-new", state.ContextFrame.Slots["instance_id"])
	assert.Equal(t, SelectedInstanceSourceUser, state.ContextFrame.SlotSources["instance_id"])
}

func TestTrustedWorkflowFrameTargetRequiresUserSourceOrExplicitID(t *testing.T) {
	frame := ContextFrame{
		Kind:     ContextFrameKindWorkflowTask,
		Workflow: "CreateDiskWorkflow",
		Slots:    map[string]string{"instance_id": "uhost-a"},
	}
	assert.Empty(t, trustedWorkflowFrameTarget(frame, map[string]string{"instance_id": "uhost-a", "size_gb": "200"}, "200G"),
		"old frames without target source must not authorize a mutating workflow")

	frame.SlotSources = map[string]string{"instance_id": SelectedInstanceSourceUser}
	assert.Equal(t, "uhost-a", trustedWorkflowFrameTarget(frame, map[string]string{"instance_id": "uhost-a", "size_gb": "200"}, "200G"))

	frame.SlotSources = nil
	assert.Equal(t, "uhost-a", trustedWorkflowFrameTarget(frame, map[string]string{"instance_id": "uhost-a", "size_gb": "200"}, "给 uhost-a 加 200G"))
}

func TestRecordWorkflowMissingSlotsFrame_FlagOffDoesNotPersistFrame(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := newEngineForSessionStateTest(t)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	recorded := eng.recordWorkflowMissingSlotsFrame("CreateDiskWorkflow", map[string]any{
		"UHostId": "uhost-1",
	}, []string{"size_gb"}, "缺少大小")

	assert.False(t, recorded)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}

func TestExecuteWorkflowMissingSlotRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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

func TestOperationLifecycleMissingRenameNameRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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
		case "ModifyCompShareInstanceName":
			t.Fatalf("missing rename target must stop before mutating; args=%v", args)
		default:
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionRename,
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

	reply, handled := eng.tryOperationLifecycleDispatch(context.Background(), dispatch, "把 host-1 改名一下", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "新名称")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "RenameInstanceWorkflow", frame.Workflow)
	assert.Equal(t, []string{"name"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestOperationLifecycleMissingRenameName_FlagOffKeepsLegacyPrompt(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "host-1", "State": "Running", "Zone": "cn-wlcb-01", "Region": "cn-wlcb",
			}}}, nil
		case "ModifyCompShareInstanceName":
			t.Fatalf("flag-off missing rename target must not mutate; args=%v", args)
		default:
			t.Fatalf("unexpected tool call in flag-off missing rename path; action=%s args=%v", action, args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionRename,
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

	reply, handled := eng.tryOperationLifecycleDispatch(context.Background(), dispatch, "把 host-1 改名一下", noopStep)

	require.True(t, handled)
	assert.Equal(t, "请告诉我要把实例改成什么名称。", reply)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}

func TestSelectedInstanceCreateDiskMissingSizeRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "host-1",
		SelectedInstanceSource: SelectedInstanceSourceUser,
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

func TestExplicitIDCreateDiskMissingSizeRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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

	reply, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "给 uhost-1 加一块数据盘", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "数据盘大小")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateDiskWorkflow", frame.Workflow)
	assert.Equal(t, []string{"size_gb"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestDirectStopSchedulerMissingTimeRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

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

	reply, handled := eng.tryDirectStopSchedulerFromUserText(context.Background(), "给 host-1 设置定时关机", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "多久后关机")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "SetStopSchedulerWorkflow", frame.Workflow)
	assert.Equal(t, []string{"stop_time"}, frame.MissingSlots)
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
	eng.lastUserMsg = "200G"
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
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
	assert.Equal(t, ContextFrameKindWorkflowTask, state.ContextFrame.Kind,
		"an unresolved/declined confirmation is not a successful workflow and must remain resumable")
	assert.Empty(t, state.ContextFrame.MissingSlots)
	assert.Equal(t, "200G", state.ContextFrame.Slots["size_gb"])
}

func TestResumeWorkflowContextFrameIgnoresModelChangedInstanceWithoutUserText(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmArgs map[string]any
	confirm := func(action string, args map[string]any) bool {
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
	eng.lastUserMsg = "200G"
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
		SlotUpdates: map[string]string{
			"instance_id": "uhost-2",
			"size_gb":     "200G",
		},
	}})
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "200G", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "uhost-1", confirmArgs["UHostId"], "model-only instance_id updates must not replace the original workflow target")
	assert.Equal(t, float64(200), confirmArgs["disk_size_gb"])
}

func TestResumeWorkflowContextFrameRejectsModelInsertedInstanceWithoutFrameOrUserText(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmAction string
	confirm := func(action string, args map[string]any) bool {
		confirmAction = action
		return false
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, confirm)
	eng.lastUserMsg = "200G"
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateDiskWorkflow",
			MissingSlots:   []string{"instance_id", "size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
		SlotUpdates: map[string]string{
			"instance_id": "uhost-2",
			"size_gb":     "200G",
		},
	}})
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "200G", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "必要参数")
	assert.Empty(t, confirmAction, "model-only target insertion must not reach a mutating confirmation")
	state, _, _ := eng.SessionStateSnapshot()
	assert.NotContains(t, state.ContextFrame.Slots, "instance_id")
}

func TestResumeWorkflowContextFrameAllowsUserExplicitChangedInstanceID(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmArgs map[string]any
	confirm := func(action string, args map[string]any) bool {
		confirmArgs = args
		return false
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-2",
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
	userMsg := "给 uhost-2 加 200G"
	eng.lastUserMsg = userMsg
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
		Decision: ContextDecisionContinueTask,
		SlotUpdates: map[string]string{
			"instance_id": "uhost-2",
			"size_gb":     "200G",
		},
	}})
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, userMsg, noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "uhost-2", confirmArgs["UHostId"], "explicit user-provided instance IDs may update the workflow target")
}

func TestResumeWorkflowContextFrameDoesNotParseSlotsWhenDecisionIsNewTask(t *testing.T) {
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
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionNewTask,
	}})
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

	assert.False(t, handled)
	assert.Empty(t, reply)
	assert.Empty(t, confirmAction)
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

func TestDirectCreateCFSMissingNameAndZoneRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutorFn{}, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentOperationLifecycle,
	}}}

	reply, handled := eng.tryCFSWorkflowDispatch(context.Background(), dispatch, "帮我创建一个100G的CFS共享文件存储", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "CFS 名称")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateCFSWorkflow", frame.Workflow)
	assert.Equal(t, []string{"name", "zone"}, frame.MissingSlots)
	assert.Equal(t, "100", frame.Slots["size_gb"])
}

func TestDirectCreateCFSMissingZoneRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutorFn{}, okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentOperationLifecycle,
	}}}

	reply, handled := eng.tryCFSWorkflowDispatch(context.Background(), dispatch, "创建一个100G的CFS共享文件存储，名字叫shared-train", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "CFS 可用区")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "CreateCFSWorkflow", frame.Workflow)
	assert.Equal(t, []string{"zone"}, frame.MissingSlots)
	assert.Equal(t, "shared-train", frame.Slots["name"])
	assert.Equal(t, "100", frame.Slots["size_gb"])
}

func TestResumeWorkflowContextFrame_CreateCFSNameFromContextDecision(t *testing.T) {
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
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-pod-01", "Region": "cn-pod", "ZoneId": float64(9001), "RegionId": float64(3001), "Describe": "容器一区", "IsPod": true,
			}}}, nil
		case "GetCompShareCFSPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(99)}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"name": "shared-train"},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateCFSWorkflow",
			Slots:          map[string]string{"size_gb": "100GB", "zone": "cn-pod-01"},
			MissingSlots:   []string{"name"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "shared-train", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateCFSWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "shared-train", confirmArgs["Name"])
	assert.Equal(t, float64(100), confirmArgs["Size"])
	assert.Equal(t, "cn-pod-01", confirmArgs["Zone"])
}

func TestResumeWorkflowContextFrame_CreateCFSNameFallbackReachesConfirm(t *testing.T) {
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
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-pod-01", "Region": "cn-pod", "ZoneId": float64(9001), "RegionId": float64(3001), "Describe": "容器一区", "IsPod": true,
			}}}, nil
		case "GetCompShareCFSPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(99)}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateCFSWorkflow",
			Slots:          map[string]string{"size_gb": "100GB", "zone": "cn-pod-01"},
			MissingSlots:   []string{"name"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "shared-train", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateCFSWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "shared-train", confirmArgs["Name"])
	assert.Equal(t, float64(100), confirmArgs["Size"])
	assert.Equal(t, "cn-pod-01", confirmArgs["Zone"])
}

func TestResumeWorkflowContextFrame_CreateCFSZoneFromContextDecision(t *testing.T) {
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
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-pod-01", "Region": "cn-pod", "ZoneId": float64(9001), "RegionId": float64(3001), "Describe": "容器一区", "IsPod": true,
			}}}, nil
		case "GetCompShareCFSPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(99)}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"zone": "cn-pod-01"},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateCFSWorkflow",
			Slots:          map[string]string{"name": "shared-train", "size_gb": "100GB"},
			MissingSlots:   []string{"zone"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "cn-pod-01", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateCFSWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "shared-train", confirmArgs["Name"])
	assert.Equal(t, float64(100), confirmArgs["Size"])
	assert.Equal(t, "cn-pod-01", confirmArgs["Zone"])
}

func TestResumeWorkflowContextFrame_SetStopSchedulerTimeReachesConfirm(t *testing.T) {
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
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Region": "cn-wlcb", "Zone": "cn-wlcb-01", "ChargeType": "Dynamic",
			}}}, nil
		case "UpdateCompShareStopScheduler":
			t.Fatalf("scheduler continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "SetStopSchedulerWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"stop_time"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "30分钟后", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "SetStopSchedulerWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "uhost-1", confirmArgs["UHostId"])
	assert.Contains(t, confirmArgs, "shutdownTime")
}

func TestResumeWorkflowContextFrame_ReinstallImageIDReachesConfirm(t *testing.T) {
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
				"UHostId": "uhost-1", "Name": "train-a", "State": "Stopped", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
				"DiskSet": []any{map[string]any{"DiskId": "boot-1", "IsBoot": true, "Size": float64(100)}},
			}}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Container": false}}}, nil
		case "ReinstallCompShareInstance":
			t.Fatalf("reinstall continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"image_id": "img-ubuntu"},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "ReinstallInstanceWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"image_id"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "Ubuntu-nvidia 22.04", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "ReinstallInstanceWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "img-ubuntu", confirmArgs["target_image_id"])
	assert.Equal(t, "Ubuntu-nvidia 22.04", confirmArgs["target_image_name"])
}

func TestResumeWorkflowContextFrame_ReinstallImagePrefReachesConfirm(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmAction string
	var confirmArgs map[string]any
	var imageArgs map[string]any
	confirm := func(action string, args map[string]any) bool {
		confirmAction = action
		confirmArgs = args
		return false
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Stopped", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
				"DiskSet": []any{map[string]any{"DiskId": "boot-1", "IsBoot": true, "Size": float64(100)}},
			}}}, nil
		case "DescribeCompShareImages":
			imageArgs = map[string]any{}
			for k, v := range args {
				imageArgs[k] = v
			}
			return map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Container": false}}}, nil
		case "ReinstallCompShareInstance":
			t.Fatalf("reinstall continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"image_pref": "Ubuntu-nvidia 22.04"},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "ReinstallInstanceWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"image_id"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "Ubuntu-nvidia 22.04", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "ReinstallInstanceWorkflow", confirmAction)
	require.NotNil(t, imageArgs)
	assert.Equal(t, "Ubuntu-nvidia 22.04", imageArgs["Name"])
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "img-ubuntu", confirmArgs["target_image_id"])
	assert.Equal(t, "Ubuntu-nvidia 22.04", confirmArgs["target_image_name"])
}

func TestResumeWorkflowContextFrame_ReinstallImageSourceRestrictsLookup(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmAction string
	var confirmArgs map[string]any
	var calledPlatform bool
	var communityArgs map[string]any
	confirm := func(action string, args map[string]any) bool {
		confirmAction = action
		confirmArgs = args
		return false
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Stopped", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
				"DiskSet": []any{map[string]any{"DiskId": "boot-1", "IsBoot": true, "Size": float64(100)}},
			}}}, nil
		case "DescribeCompShareImages":
			calledPlatform = true
			return map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "img-platform", "Name": "Ubuntu-nvidia 22.04", "Container": false}}}, nil
		case "DescribeCommunityImages":
			communityArgs = map[string]any{}
			for k, v := range args {
				communityArgs[k] = v
			}
			return map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "comm-img-ubuntu", "Name": "Ubuntu-nvidia 22.04", "Container": false}}}, nil
		case "ReinstallCompShareInstance":
			t.Fatalf("reinstall continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
		SlotUpdates: map[string]string{
			"image_pref":   "Ubuntu-nvidia 22.04",
			"image_source": "community",
		},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "ReinstallInstanceWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"image_id"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "社区镜像 Ubuntu-nvidia 22.04", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.False(t, calledPlatform, "explicit community source must not select a platform image first")
	require.NotNil(t, communityArgs)
	assert.Equal(t, "Ubuntu-nvidia 22.04", communityArgs["FuzzySearch"])
	assert.Equal(t, "ReinstallInstanceWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "comm-img-ubuntu", confirmArgs["target_image_id"])
	assert.Equal(t, "community", confirmArgs["target_image_source"])
}

func TestResumeWorkflowContextFrame_ResizeCFSSizeReachesConfirm(t *testing.T) {
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
		case "DescribeCFS":
			return map[string]any{"CFSSet": []any{map[string]any{
				"CfsId": "cfs-test", "Name": "shared-train", "ZoneId": float64(9001), "Size": float64(100), "ChargeType": "Month",
			}}}, nil
		case "GetCompShareCFSUpgradePrice":
			return map[string]any{"Price": float64(49), "OriginalPrice": float64(60)}, nil
		case "ResizeCFS":
			t.Fatalf("CFS resize continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "ResizeCFSWorkflow",
			Slots:          map[string]string{"cfs_id": "cfs-test"},
			MissingSlots:   []string{"target_size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "200G", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "ResizeCFSWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "cfs-test", confirmArgs["CfsId"])
	assert.Equal(t, float64(200), confirmArgs["target_size_gb"])
	assert.Equal(t, float64(49), confirmArgs["price_delta"])
}

func TestPlannerDispatchResumesCFSFrameBeforeDirectCFSParser(t *testing.T) {
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
		case "DescribeCFS":
			return map[string]any{"CFSSet": []any{map[string]any{
				"CfsId": "cfs-test", "Name": "shared-train", "ZoneId": float64(9001), "Size": float64(100), "ChargeType": "Month",
			}}}, nil
		case "GetCompShareCFSUpgradePrice":
			return map[string]any{"Price": float64(49), "OriginalPrice": float64(60)}, nil
		case "ResizeCFS":
			t.Fatalf("CFS resize continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "ResizeCFSWorkflow",
			Slots:          map[string]string{"cfs_id": "cfs-test"},
			MissingSlots:   []string{"target_size_gb"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentOperationLifecycle,
		Confidence:    0.95,
	}}}}
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		Model:          "deepseek-v4-flash",
		EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle},
	})

	reply, err := eng.Chat(context.Background(), "这个 CFS 扩容到 200G", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "ResizeCFSWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "cfs-test", confirmArgs["CfsId"])
	assert.Equal(t, float64(200), confirmArgs["target_size_gb"])
}

func TestResumeWorkflowContextFrame_EnableNetOptimizerZoneReachesConfirm(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var confirmAction string
	var confirmArgs map[string]any
	var checkArgs map[string]any
	confirm := func(action string, args map[string]any) bool {
		confirmAction = action
		confirmArgs = args
		return false
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{
				"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true,
			}}}, nil
		case "CheckCompShareNetOptimizer":
			checkArgs = map[string]any{}
			for k, v := range args {
				checkArgs[k] = v
			}
			return map[string]any{"Optimized": false}, nil
		case "SyncCompShareNetOptimizer":
			t.Fatalf("network optimizer continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.confirmFn = confirm
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision: ContextDecisionContinueTask,
	}})
	eng.lastUserMsg = "华北一C"
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "EnableNetOptimizerWorkflow",
			MissingSlots:   []string{"zone"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(zoneUserCtx(), dispatch, "华北一C", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	require.NotNil(t, checkArgs)
	assert.Equal(t, "cn-bj2-03", checkArgs["Zone"])
	assert.Equal(t, uint32(3003), checkArgs["az_group"])
	assert.Equal(t, "EnableNetOptimizerWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "cn-bj2-03", confirmArgs["Zone"])
	assert.NotContains(t, confirmArgs, "az_group")
	assert.NotContains(t, confirmArgs, "zone_id")
}

func TestResumeWorkflowContextFrame_CreateCustomImageNameReachesConfirm(t *testing.T) {
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
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
			}}}, nil
		case "CreateCompShareCustomImage":
			t.Fatalf("custom-image continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.selectedInstanceIDAtTurnStart = "uhost-1"
	eng.selectedInstanceSourceAtTurnStart = SelectedInstanceSourceUser
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"name": "snapshot-v1"},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "CreateCustomImageWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1", "description": "training environment"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"name"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "snapshot-v1", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "CreateCustomImageWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "uhost-1", confirmArgs["UHostId"])
	assert.Equal(t, "snapshot-v1", confirmArgs["Name"])
	assert.Equal(t, "training environment", confirmArgs["Description"])
	assert.Equal(t, "cn-wlcb", confirmArgs["Region"])
	assert.Equal(t, "cn-wlcb-01", confirmArgs["Zone"])
}

func TestResumeWorkflowContextFrame_RenameNameReachesConfirm(t *testing.T) {
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
				"UHostId": "uhost-1", "Name": "old-host", "State": "Running", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
			}}}, nil
		case "ModifyCompShareInstanceName":
			t.Fatalf("rename continuation must stop at confirm before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.selectedInstanceIDAtTurnStart = "uhost-1"
	eng.selectedInstanceSourceAtTurnStart = SelectedInstanceSourceUser
	eng.SetContextDecisionLayer(&fakeContextDecisionLayer{decision: &ContextDecision{
		Decision:    ContextDecisionContinueTask,
		SlotUpdates: map[string]string{"name": "renamed-host"},
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindWorkflowTask,
			Status:         ContextFrameStatusFailedRecoverable,
			Workflow:       "RenameInstanceWorkflow",
			Slots:          map[string]string{"instance_id": "uhost-1"},
			SlotSources:    map[string]string{"instance_id": SelectedInstanceSourceUser},
			MissingSlots:   []string{"name"},
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentOperationLifecycle}}}

	reply, handled := eng.tryResumeWorkflowContextFrame(context.Background(), dispatch, "renamed-host", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.Equal(t, "RenameInstanceWorkflow", confirmAction)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "old-host", confirmArgs["Name"])
	assert.Equal(t, "renamed-host", confirmArgs["NewName"])
}

func TestSelectedInstanceRenameMissingNameRecordsGenericTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "old-host", "State": "Running", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
			}}}, nil
		case "ModifyCompShareInstanceName":
			t.Fatalf("missing rename target must stop before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "old-host",
		SelectedInstanceSource: SelectedInstanceSourceUser,
	}, 1)

	reply, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "把这台改名一下", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "新名称")
	state, _, _ := eng.SessionStateSnapshot()
	frame := state.ContextFrame
	assert.Equal(t, ContextFrameKindWorkflowTask, frame.Kind)
	assert.Equal(t, "RenameInstanceWorkflow", frame.Workflow)
	assert.Equal(t, []string{"name"}, frame.MissingSlots)
	assert.Equal(t, "uhost-1", frame.Slots["instance_id"])
}

func TestDirectLifecycleClearsStaleWorkflowTaskFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var rebootCalled bool
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
			}}}, nil
		case "RebootCompShareInstance":
			rebootCalled = true
			return map[string]any{"RetCode": float64(0)}, nil
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "train-a",
		SelectedInstanceSource: SelectedInstanceSourceUser,
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

	reply, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "帮我重启这台", noopStep)

	require.True(t, handled)
	assert.True(t, rebootCalled)
	assert.Contains(t, reply, "重启")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind, "a new direct lifecycle command must clear the stale missing-slot task")
}

func TestSelectedInstanceRenameMissingName_FlagOffKeepsLegacyPrompt(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, okConfirm)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "old-host",
		SelectedInstanceSource: SelectedInstanceSourceUser,
	}, 1)

	reply, handled := eng.tryDirectLifecycleFromUserText(context.Background(), "把这台改名一下", noopStep)

	require.True(t, handled)
	assert.Equal(t, "请告诉我要把实例改成什么名称。", reply)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}

func TestCreateCustomImageMissingName_FlagOffDoesNotPersistContextFrame(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Region": "cn-wlcb", "Zone": "cn-wlcb-01",
			}}}, nil
		case "CreateCompShareCustomImage":
			t.Fatalf("missing custom-image name must stop before mutating; args=%v", args)
		}
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.selectedInstanceIDAtTurnStart = "uhost-1"
	eng.selectedInstanceSourceAtTurnStart = SelectedInstanceSourceUser
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceSource: SelectedInstanceSourceUser,
	}, 1)

	reply := eng.executeWorkflow(context.Background(), "CreateCustomImageWorkflow", map[string]any{
		"UHostId":     "uhost-1",
		"Description": "training environment",
	}, noopStep)

	assert.Contains(t, reply, "需要先确认自制镜像名称")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}
