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

func TestWorkflowRequiresInstanceTarget(t *testing.T) {
	for _, action := range []string{
		"CreateInstanceWorkflow",
		"EnableNetOptimizerWorkflow",
		"CreateCFSWorkflow",
		"ResizeCFSWorkflow",
	} {
		assert.False(t, workflowRequiresInstanceTarget(action), action)
	}

	for _, action := range []string{
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"RebootInstanceWorkflow",
		"ResizeDiskWorkflow",
		"CreateDiskWorkflow",
		"CreateCustomImageWorkflow",
	} {
		assert.True(t, workflowRequiresInstanceTarget(action), action)
	}
}

func TestExecuteWorkflowBlocksUntrustedModelChosenInstanceTarget(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "怎么关机"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))
	onStep, events := collectSteps()

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, onStep)

	assert.Contains(t, reply, "请先确认要操作的实例")
	assert.Empty(t, exec.calls, "untrusted target must be blocked before workflow tools run")
	assertStepWithType(t, *events, StepBlocked, "StopInstanceWorkflow", "请先确认")
}

func TestExecuteWorkflowBlocksUntrustedTargetWithoutHydratedSession(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.lastUserMsg = "怎么关机"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	assert.Contains(t, reply, "请先确认要操作的实例")
	assert.Empty(t, exec.calls, "untrusted target must be blocked even without hydrated session state")
}

func TestExecuteWorkflowDoesNotTrustContextFrameUnlessResumed(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Kind:     ContextFrameKindWorkflowTask,
			Workflow: "CreateDiskWorkflow",
			Slots:    map[string]string{"instance_id": "uhost-a", "size_gb": "200"},
		},
	}, 1)
	eng.lastUserMsg = "200G"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "CreateDiskWorkflow", map[string]any{"UHostId": "uhost-a", "Size": float64(200)}, noopStep)

	assert.Contains(t, reply, "请先确认要操作的实例")
	assert.Empty(t, exec.calls, "context frame alone must not let a direct model workflow call pick a target")
}

func TestExecuteWorkflowAllowsExplicitIDTarget(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.lastUserMsg = "帮我关机 uhost-a"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	assert.Contains(t, reply, "执行关机")
	assert.Contains(t, exec.calls, "StopCompShareInstance")
}

func TestExecuteWorkflowAllowsOrdinalPendingSelectionTarget(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "帮我关机第1台"
	eng.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "alpha", "Running"),
		testInstance("uhost-b", "beta", "Running"),
	}, intent.IntentResourceInfo, "我有哪些实例", 2, false)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	assert.Contains(t, reply, "执行关机")
	assert.Contains(t, exec.calls, "StopCompShareInstance")
}

func TestExecuteWorkflowAllowsSelectedInstanceTarget(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceUser,
	}, 1)
	// "它" refers to an instance selected in a PRIOR turn. In the real Chat()
	// flow that carried binding is captured into the turn-start snapshot
	// (engine.go:1254) before the turn runs; the guard trusts only that frozen
	// snapshot, never a value written mid-turn by a model tool call.
	eng.selectedInstanceIDAtTurnStart = "uhost-a"
	eng.selectedInstanceSourceAtTurnStart = SelectedInstanceSourceUser
	eng.lastUserMsg = "帮我关机它"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	assert.Contains(t, reply, "执行关机")
	assert.Contains(t, exec.calls, "StopCompShareInstance")
}

// A legacy persisted selection has no timestamp, so there is no evidence that
// the user's authorization is still fresh. Keep the id for conversational
// understanding, but do not let a bare pronoun turn that unknown-age memory
// into an infrastructure write.
func TestExecuteWorkflowBlocksUnstampedLegacySelectionTarget(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{
				map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
			}}, nil
		case "StopCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		// SelectedInstanceAtUnix intentionally zero: this is a pre-TTL row.
	}, 1)

	// Chat() performs this downgrade before freezing the turn-start snapshot.
	eng.expireStaleSelectedInstance(time.Unix(1_900_000_000, 0))
	state, _, _ := eng.SessionStateSnapshot()
	eng.selectedInstanceIDAtTurnStart = state.SelectedInstanceID
	eng.selectedInstanceSourceAtTurnStart = state.SelectedInstanceSource
	eng.lastUserMsg = "帮我关掉它"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	assert.Equal(t, "uhost-a", state.SelectedInstanceID,
		"the legacy binding remains available for understanding and clarification")
	assert.Contains(t, reply, "请先确认要操作的实例")
	assert.NotContains(t, exec.calls, "StopCompShareInstance",
		"an unknown-age binding must not authorize a write")
}

func TestExecuteWorkflowBlocksObservedSelectedInstanceTarget(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return true })
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceObserved,
	}, 1)
	eng.selectedInstanceIDAtTurnStart = "uhost-a"
	eng.selectedInstanceSourceAtTurnStart = SelectedInstanceSourceObserved
	eng.lastUserMsg = "帮我关机它"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-a"}, noopStep)

	assert.Contains(t, reply, "请先确认要操作的实例")
	assert.Empty(t, exec.calls, "tool-observed selected instance must not authorize a mutating workflow")
}

func TestExecuteWorkflowCreateCFSResolvesPodZone(t *testing.T) {
	var priceArgs map[string]any
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "GetCompShareCFSPrice":
			priceArgs = args
			return map[string]any{
				"PriceDetails": []any{
					map[string]any{"ChargeType": "Month", "Disks": float64(99)},
				},
			}, nil
		case "CreateCFS":
			createArgs = args
			return map[string]any{"CfsId": "cfs-new"}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我在华北一C创建一个 50GB 的 CFS"

	_ = eng.executeWorkflow(zoneUserCtx(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-wlcb-01",
	}, noopStep)

	require.NotNil(t, priceArgs)
	require.NotNil(t, createArgs)
	assert.Equal(t, uint32(5001), priceArgs["zone_id"])
	assert.Equal(t, uint32(5001), createArgs["zone_id"])
	assert.Equal(t, uint32(3003), priceArgs["az_group"])
	assert.Equal(t, uint32(3003), createArgs["az_group"])
}

func TestExecuteWorkflowCreateCFSRejectsNonPodZoneDeterministically(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我在 cn-wlcb-01 创建一个 50GB 的 CFS"

	reply := eng.executeWorkflow(zoneUserCtx(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-wlcb-01",
	}, noopStep)

	assert.Contains(t, reply, "CFS 创建没有成功")
	assert.Contains(t, reply, "只支持 Pod")
	assert.NotContains(t, reply, "cn-pod-01")
}

func TestExecuteWorkflowEnableNetOptimizerResolvesAzGroup(t *testing.T) {
	var syncArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "CheckCompShareNetOptimizer":
			return map[string]any{"Optimized": false}, nil
		case "SyncCompShareNetOptimizer":
			syncArgs = args
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我开启华北一C网络加速"

	_ = eng.executeWorkflow(zoneUserCtx(), "EnableNetOptimizerWorkflow", map[string]any{
		"Zone": "cn-bj2-03",
	}, noopStep)

	require.NotNil(t, syncArgs)
	assert.Equal(t, uint32(3003), syncArgs["az_group"])
	assert.Equal(t, uint32(1), syncArgs["top_organization_id"])
	assert.Equal(t, uint32(2), syncArgs["organization_id"])
}
