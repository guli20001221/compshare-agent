package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func twoDigit(n int) string {
	return fmt.Sprintf("%02d", n)
}

func TestReplayRegression_ResourceInfoResolvesNamedInstanceFromFullSnapshot(t *testing.T) {
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": manyInstancesWithNamedTarget("claude-write-test", "Stopped"),
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentResourceInfo,
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo}})

	reply, err := eng.Chat(context.Background(), "claude-write-test 关机了吗", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "claude-write-test")
	require.Contains(t, reply, "Stopped")
	require.NotContains(t, reply, "未找到")
	require.Len(t, mock.calls, 0, "resource_info must be answered by deterministic handler, not final LLM narration")
}

func TestReplayRegression_MonitorEmbeddedOrdinalUsesFreshSelection(t *testing.T) {
	hosts := make([]any, 0, 11)
	for i := 1; i <= 11; i++ {
		id := "uhost-" + twoDigit(i)
		hosts = append(hosts, map[string]any{
			"UHostId": id,
			"Name":    "host-" + twoDigit(i),
			"State":   "Running",
			"GpuType": "V100S",
			"GPU":     float64(1),
			"CPU":     float64(10),
			"Memory":  float64(65536),
			"Zone":    "cn-wlcb-01",
		})
	}
	data := map[string]any{"TotalCount": float64(11), "UHostSet": hosts}
	var monitorIDs []string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return data, nil
		case "GetCompShareInstanceMonitor":
			monitorIDs = stringSliceArg(args["UHostIds"])
			return monitorPayload([]monitorPayloadHost{{
				UHostID: "uhost-11",
				Metrics: []monitorPayloadMetric{{
					Key:    "uhost_gpu_used",
					Values: [][2]any{{1716530000, "12.0"}},
				}},
			}}), nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentMonitorQuery,
			Slots:         intent.Slots{Metrics: []intent.Metric{"gpu_usage"}},
			RequiredTools: []string{"GetCompShareInstanceMonitor"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		PendingSelectionItems: []PendingSelectionItem{
			{ID: "uhost-01", Name: "host-01", State: "Running"},
			{ID: "uhost-02", Name: "host-02", State: "Running"},
			{ID: "uhost-03", Name: "host-03", State: "Running"},
			{ID: "uhost-04", Name: "host-04", State: "Running"},
			{ID: "uhost-05", Name: "host-05", State: "Running"},
			{ID: "uhost-06", Name: "host-06", State: "Running"},
			{ID: "uhost-07", Name: "host-07", State: "Running"},
			{ID: "uhost-08", Name: "host-08", State: "Running"},
			{ID: "uhost-09", Name: "host-09", State: "Running"},
			{ID: "uhost-10", Name: "host-10", State: "Running"},
			{ID: "uhost-11", Name: "host-11", State: "Running"},
		},
		PendingSelectionKind:            "instance",
		PendingSelectionIntent:          "resource_info",
		PendingSelectionOriginalUserMsg: "我有哪些实例",
		PendingSelectionCreatedTurn:     1,
		PendingSelectionProducedAtUnix:  time.Now().Unix(),
		PendingSelectionTTLSeconds:      pendingSelectionTTLSeconds,
		PendingSelectionTotalCount:      11,
	}, 2)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentMonitorQuery}})

	reply, err := eng.Chat(context.Background(), "第11台 GPU 忙不忙", noopStep)

	require.NoError(t, err)
	require.Empty(t, monitorIDs)
	require.Contains(t, reply, "请选择")
	require.Empty(t, eng.sessionState.SelectedInstanceID)
}

func TestReplayRegression_MonitorEmbeddedOrdinalWithoutSavedSelectionAsksUser(t *testing.T) {
	hosts := make([]any, 0, 11)
	for i := 1; i <= 11; i++ {
		id := "uhost-" + twoDigit(i)
		hosts = append(hosts, map[string]any{
			"UHostId": id,
			"Name":    "host-" + twoDigit(i),
			"State":   "Running",
			"GpuType": "V100S",
			"GPU":     float64(1),
		})
	}
	data := map[string]any{"TotalCount": float64(11), "UHostSet": hosts}
	monitorCalled := false
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return data, nil
		case "GetCompShareInstanceMonitor":
			monitorCalled = true
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentMonitorQuery,
			Slots:         intent.Slots{Metrics: []intent.Metric{"gpu_usage"}},
			RequiredTools: []string{"GetCompShareInstanceMonitor"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentMonitorQuery}})

	reply, err := eng.Chat(context.Background(), "第11台 GPU 忙不忙", noopStep)

	require.NoError(t, err)
	require.False(t, monitorCalled)
	require.Contains(t, reply, "请选择")
	require.Empty(t, eng.sessionState.SelectedInstanceID)
}

func TestReplayRegression_LifecycleStopExactNameUsesStateGate(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("内网ping勿删", "Running"))
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionStop,
			},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	confirmed := false
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmed = true
		require.Equal(t, "StopInstanceWorkflow", action)
		require.Equal(t, "uhost-target", args["UHostId"])
		return true
	})
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "把内网ping勿删停了", noopStep)

	require.NoError(t, err)
	require.True(t, confirmed, "resolved running target should reach the workflow confirmation")
	require.Contains(t, reply, "已为实例 uhost-target 执行关机")
	require.NotContains(t, reply, "请选择")
	require.NotContains(t, reply, "未找到")
}

func TestReplayRegression_LifecycleStopAlreadyStoppedDoesNotAskOrCreate(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("claude-write-test", "Stopped"))
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionStop,
			},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, func(string, map[string]any) bool {
		t.Fatal("already-stopped no-op must not ask for mutating confirmation")
		return false
	})
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "把 claude-write-test 关机", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "claude-write-test")
	require.Contains(t, reply, "已经是关机状态")
	require.NotContains(t, exec.calls, "StopCompShareInstance")
}

func TestReplayRegression_WithoutGPUStartOverridesPlannerStop(t *testing.T) {
	data := map[string]any{
		"TotalCount": float64(1),
		"UHostSet": []any{map[string]any{
			"UHostId":                "uhost-withoutgpu",
			"Name":                   "without-gpu-target",
			"State":                  "Stopped",
			"GpuType":                "V100S",
			"GPU":                    float64(1),
			"CPU":                    float64(10),
			"Memory":                 float64(65536),
			"Region":                 "cn-wlcb",
			"Zone":                   "cn-wlcb-01",
			"SupportWithoutGpuStart": true,
			"WithoutGpuSpec": map[string]any{
				"Cpu":    float64(2),
				"Memory": float64(4096),
				"Gpu":    float64(0),
			},
		}},
	}
	var resizeArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if ids := stringSliceArg(args["UHostIds"]); len(ids) > 0 {
				return filterDescribeInstances(data, ids), nil
			}
			return data, nil
		case "ResizeCompShareInstance":
			resizeArgs = map[string]any{}
			for k, v := range args {
				resizeArgs[k] = v
			}
			return map[string]any{"RetCode": 0}, nil
		case "StartCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		case "StopCompShareInstance":
			t.Fatal("无卡启动不能因为路由误判而执行关机")
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionStop,
			},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		require.Equal(t, "StartInstanceWorkflow", action)
		require.Equal(t, "uhost-withoutgpu", args["UHostId"])
		return true
	})
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "请无卡启动实例 uhost-withoutgpu", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "开机")
	require.Contains(t, exec.calls, "ResizeCompShareInstance")
	require.Contains(t, exec.calls, "StartCompShareInstance")
	require.NotContains(t, exec.calls, "StopCompShareInstance")
	require.NotNil(t, resizeArgs)
	require.Equal(t, true, resizeArgs["WithoutGpu"])
	require.Equal(t, float64(0), resizeArgs["Gpu"])
}

func TestReplayRegression_ResizeCommandEntersResizeWorkflow(t *testing.T) {
	data := manyInstancesWithNamedTarget("resize-target", "Stopped")
	var resizeArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if ids := stringSliceArg(args["UHostIds"]); len(ids) > 0 {
				return filterDescribeInstances(data, ids), nil
			}
			return data, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{
				"Name":   "4090",
				"Zone":   "cn-wlcb-01",
				"Status": "Normal",
				"MachineSizes": []any{map[string]any{
					"Gpu": float64(1),
					"Collection": []any{map[string]any{
						"Cpu":    float64(4),
						"Memory": []any{float64(8)},
					}},
				}},
			}}}, nil
		case "GetCompShareInstanceUpgradePrice":
			return map[string]any{"Price": float64(1.2), "OriginalPrice": float64(1.5)}, nil
		case "ResizeCompShareInstance":
			resizeArgs = map[string]any{}
			for k, v := range args {
				resizeArgs[k] = v
			}
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{TargetRefs: []intent.TargetRef{{
				Type:   intent.TargetRefName,
				Value:  "resize-target",
				Source: intent.SourceUserText,
			}}},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		require.Equal(t, "ResizeInstanceWorkflow", action)
		return true
	})
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "把 resize-target 4090卡改配为 4C8G", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "变配")
	require.NotNil(t, resizeArgs)
	require.Equal(t, "uhost-target", resizeArgs["UHostId"])
	require.Equal(t, float64(4), resizeArgs["Cpu"])
	require.Equal(t, float64(8192), resizeArgs["Memory"])
	require.NotContains(t, resizeArgs, "Gpu")
}

func TestReplayRegression_RenameCommandEntersRenameWorkflow(t *testing.T) {
	data := manyInstancesWithNamedTarget("rename-target", "Running")
	var renameArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if ids := stringSliceArg(args["UHostIds"]); len(ids) > 0 {
				return filterDescribeInstances(data, ids), nil
			}
			return data, nil
		case "ModifyCompShareInstanceName":
			renameArgs = map[string]any{}
			for k, v := range args {
				renameArgs[k] = v
			}
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		require.Equal(t, "RenameInstanceWorkflow", action)
		require.Equal(t, "uhost-target", args["UHostId"])
		require.Equal(t, "gate1-rename-smoke", args["NewName"])
		return true
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "把 rename-target 改名为 gate1-rename-smoke", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "gate1-rename-smoke")
	require.NotNil(t, renameArgs)
	require.Equal(t, "uhost-target", renameArgs["UHostId"])
	require.Equal(t, "gate1-rename-smoke", renameArgs["Name"])
	require.Len(t, mock.calls, 0, "explicit rename command with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_ResetPasswordPreconditionDoesNotBecomeStopWorkflow(t *testing.T) {
	data := manyInstancesWithNamedTarget("reset-target", "Running")
	for _, item := range data["UHostSet"].([]any) {
		row := item.(map[string]any)
		if row["UHostId"] == "uhost-target" {
			row["InstanceType"] = "Normal"
			row["Region"] = "cn-wlcb"
		}
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if ids := stringSliceArg(args["UHostIds"]); len(ids) > 0 {
				return filterDescribeInstances(data, ids), nil
			}
			return data, nil
		case "StopCompShareInstance":
			t.Fatal("reset-password precondition failure must not switch to stop workflow")
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		t.Fatalf("running VM reset password must fail before confirmation, got %s %+v", action, args)
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "重置 reset-target 密码为 Aa123456!", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "需要先关机")
	require.NotContains(t, reply, "关机操作未执行")
	require.Len(t, mock.calls, 0, "explicit reset command with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_ResetPasswordAbbreviationEntersWorkflow(t *testing.T) {
	data := manyInstancesWithNamedTarget("reset-target", "Stopped")
	for _, item := range data["UHostSet"].([]any) {
		row := item.(map[string]any)
		if row["UHostId"] == "uhost-target" {
			row["InstanceType"] = "Normal"
		}
	}
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if ids := stringSliceArg(args["UHostIds"]); len(ids) > 0 {
				return filterDescribeInstances(data, ids), nil
			}
			return data, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		require.Equal(t, "ResetPasswordWorkflow", action)
		require.Equal(t, "uhost-target", args["UHostId"])
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "给 reset-target 改密为 Aa123456!", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "未执行")
	require.Equal(t, 1, confirmCalls)
	require.Len(t, mock.calls, 0, "explicit 改密 command with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_LifecycleBillingQuestionDoesNotEnterDirectWorkflow(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("billing-target", "Running"))
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionStop,
				TargetRefs: []intent.TargetRef{{
					Type:   intent.TargetRefName,
					Value:  "billing-target",
					Source: intent.SourceUserText,
				}},
			},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "关机后的计费要看当前资源和计费方式。"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		return false
	})
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "billing-target 关机后收费多少", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "计费")
	require.Equal(t, 0, confirmCalls)
	require.Len(t, mock.calls, 1, "billing consultation should continue normal answer path, not direct stop workflow")
}

func TestReplayRegression_PlannerLifecycleConsultationDoesNotEnterWorkflow(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("reset-target", "Stopped"))
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionResetPwd,
				TargetRefs: []intent.TargetRef{{
					Type:   intent.TargetRefName,
					Value:  "reset-target",
					Source: intent.SourceUserText,
				}},
			},
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "重置密码可能会影响登录方式，请先确认注意事项。"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		return false
	})
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle}})

	reply, err := eng.Chat(context.Background(), "reset-target 重置密码影响", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "影响")
	require.Equal(t, 0, confirmCalls)
	require.Len(t, mock.calls, 1, "planner-misclassified consultation should not enter mutating workflow")
}

func TestReplayRegression_ScheduledShutdownExactNameEntersWorkflow(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("schedule-target", "Running"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		require.Equal(t, "SetStopSchedulerWorkflow", action)
		require.Equal(t, "uhost-target", args["UHostId"])
		require.Contains(t, args, "shutdownTime")
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "给 schedule-target 设置1小时后自动关机", noopStep)

	require.NoError(t, err)
	require.NotContains(t, reply, "未找到")
	require.Equal(t, 1, confirmCalls, "explicit scheduled shutdown should reach the scheduler confirmation card")
	require.Len(t, mock.calls, 0, "explicit scheduled shutdown with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_CancelScheduledShutdownExactNameEntersWorkflow(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("schedule-target", "Running"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		require.Equal(t, "CancelStopSchedulerWorkflow", action)
		require.Equal(t, "uhost-target", args["UHostId"])
		require.NotContains(t, args, "shutdownTime")
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "取消 schedule-target 的定时关机", noopStep)

	require.NoError(t, err)
	require.NotContains(t, reply, "多久后")
	require.Equal(t, 1, confirmCalls, "explicit cancel scheduled shutdown should reach the cancel confirmation card")
	require.Len(t, mock.calls, 0, "explicit cancel scheduled shutdown with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_StopNamedInstanceWithoutCacheEntersWorkflow(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("codex-route-v100-test", "Running"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		require.Equal(t, "StopInstanceWorkflow", action)
		require.Equal(t, "uhost-target", args["UHostId"])
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "关机 codex-route-v100-test", noopStep)

	require.NoError(t, err)
	require.NotContains(t, reply, "未找到")
	require.Equal(t, 1, confirmCalls, "explicit stop by business name should reach the stop confirmation card")
	require.Len(t, mock.calls, 0, "explicit stop with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_LifecycleConsultationDoesNotEnterDirectWorkflow(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("reset-target", "Running"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "重置密码前请先确认注意事项。"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "reset-target 重置密码注意事项", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "注意事项")
	require.Equal(t, 0, confirmCalls)
	require.Len(t, mock.calls, 1, "consultation should go through normal answer path, not direct mutating workflow")
}

func TestReplayRegression_ScheduledShutdownConsultationDoesNotRefreshOrConfirm(t *testing.T) {
	var calls []string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		calls = append(calls, action)
		return map[string]any{}, nil
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "定时关机注意事项包括确认实例和时间。"}}}
	confirmCalls := 0
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalls++
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "schedule-target 定时关机注意事项", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "定时关机")
	require.Equal(t, 0, confirmCalls)
	require.NotContains(t, calls, "UpdateCompShareStopScheduler")
	require.NotContains(t, calls, "DeleteCompShareStopScheduler")
	require.Len(t, mock.calls, 1, "scheduled shutdown consultation should not enter direct scheduler workflow")
}

func TestReplayRegression_RefundFallbackForcesFreshAccountVisibility(t *testing.T) {
	turn := 0
	refundCalled := false
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			turn++
			if turn == 1 {
				return map[string]any{
					"TotalCount": float64(1),
					"UHostSet": []any{map[string]any{
						"UHostId": "uhost-selected",
						"Name":    "selected-host",
						"State":   "Running",
						"GpuType": "4090",
						"GPU":     float64(1),
						"CPU":     float64(16),
						"Memory":  float64(65536),
						"Zone":    "cn-wlcb-01",
					}},
				}, nil
			}
			return map[string]any{"TotalCount": float64(0), "UHostSet": []any{}}, nil
		case "GetCompShareRefundPrice":
			refundCalled = true
			return map[string]any{"RefundPriceSet": []any{}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{
			Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentResourceInfo,
				RequiredTools: []string{"DescribeCompShareInstance"},
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			},
		},
		{
			Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentRefundEstimate,
				RequiredTools: []string{"GetCompShareRefundPrice"},
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentRefundEstimate}})

	_, err := eng.Chat(context.Background(), "我有哪些实例", noopStep)
	require.NoError(t, err)
	eng.recordSelectedInstanceID("uhost-selected", "selected-host")

	reply, err := eng.Chat(context.Background(), "那现在退费多少", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "未找到")
	require.False(t, refundCalled, "refund fallback must verify fresh account visibility before calling refund estimate")
}

func TestReplayRegression_ResourceListThenEmbeddedOrdinalMonitorUsesPersistedSelection(t *testing.T) {
	hosts := make([]any, 0, 11)
	for i := 1; i <= 11; i++ {
		id := "uhost-" + twoDigit(i)
		hosts = append(hosts, map[string]any{
			"UHostId": id,
			"Name":    "host-" + twoDigit(i),
			"State":   "Running",
			"GpuType": "V100S",
			"GPU":     float64(1),
			"CPU":     float64(10),
			"Memory":  float64(65536),
			"Zone":    "cn-wlcb-01",
		})
	}
	data := map[string]any{"TotalCount": float64(11), "UHostSet": hosts}
	var monitorIDs []string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return data, nil
		case "GetCompShareInstanceMonitor":
			monitorIDs = stringSliceArg(args["UHostIds"])
			return monitorPayload([]monitorPayloadHost{{
				UHostID: "uhost-11",
				Metrics: []monitorPayloadMetric{{
					Key:    "uhost_gpu_used",
					Values: [][2]any{{1716530000, "12.0"}},
				}},
			}}), nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{
			Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentResourceInfo,
				RequiredTools: []string{"DescribeCompShareInstance"},
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			},
		},
		{
			Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentMonitorQuery,
				Slots:         intent.Slots{Metrics: []intent.Metric{"gpu_usage"}},
				RequiredTools: []string{"GetCompShareInstanceMonitor"},
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery}})

	_, err := eng.Chat(context.Background(), "我有哪些实例", noopStep)
	require.NoError(t, err)
	state, _, _ := eng.SessionStateSnapshot()
	require.Len(t, state.PendingSelectionItems, 10)

	reply, err := eng.Chat(context.Background(), "第11台 GPU 忙不忙", noopStep)

	require.NoError(t, err)
	require.Empty(t, monitorIDs)
	require.Contains(t, reply, "请选择")
	require.Empty(t, eng.sessionState.SelectedInstanceID)
}

func TestReplayRegression_ResourceListThenVisibleOrdinalMonitorUsesPersistedSelection(t *testing.T) {
	hosts := make([]any, 0, 11)
	for i := 1; i <= 11; i++ {
		id := "uhost-" + twoDigit(i)
		hosts = append(hosts, map[string]any{
			"UHostId": id,
			"Name":    "host-" + twoDigit(i),
			"State":   "Running",
			"GpuType": "V100S",
			"GPU":     float64(1),
			"CPU":     float64(10),
			"Memory":  float64(65536),
			"Zone":    "cn-wlcb-01",
		})
	}
	data := map[string]any{"TotalCount": float64(11), "UHostSet": hosts}
	var monitorIDs []string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return data, nil
		case "GetCompShareInstanceMonitor":
			monitorIDs = stringSliceArg(args["UHostIds"])
			return monitorPayload([]monitorPayloadHost{{
				UHostID: "uhost-10",
				Metrics: []monitorPayloadMetric{{
					Key:    "uhost_gpu_used",
					Values: [][2]any{{1716530000, "12.0"}},
				}},
			}}), nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{
			Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentResourceInfo,
				RequiredTools: []string{"DescribeCompShareInstance"},
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			},
		},
		{
			Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentMonitorQuery,
				Slots:         intent.Slots{Metrics: []intent.Metric{"gpu_usage"}},
				RequiredTools: []string{"GetCompShareInstanceMonitor"},
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery}})

	_, err := eng.Chat(context.Background(), "我有哪些实例", noopStep)
	require.NoError(t, err)
	state, _, _ := eng.SessionStateSnapshot()
	require.Len(t, state.PendingSelectionItems, 10)

	reply, err := eng.Chat(context.Background(), "第10台 GPU 忙不忙", noopStep)

	require.NoError(t, err)
	require.Equal(t, []string{"uhost-10"}, monitorIDs)
	require.NotContains(t, reply, "请选择")
	require.Equal(t, "uhost-10", eng.sessionState.SelectedInstanceID)
}

func TestReplayRegression_DirectLifecycleStopColloquialPhraseBypassesReActLoop(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("claude-write-test", "Stopped"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "claude-write-test ，帮我关下这台实例", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "claude-write-test")
	require.Contains(t, reply, "已经是关机状态")
	require.NotContains(t, reply, "轮次超限")
	require.Len(t, mock.calls, 0, "explicit lifecycle command with exact instance name must not enter the ReAct loop")
}

func TestReplayRegression_CodingPlanDeleteDoesNotInspectInstances(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("claude-write-test", "Stopped"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	exec.calls = nil

	reply, err := eng.Chat(context.Background(), "删除coding plan 包", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "Coding Plan")
	require.Contains(t, reply, "未提供")
	require.NotContains(t, reply, "轮次超限")
	require.Len(t, exec.calls, 0, "Coding Plan package management is product knowledge, not instance lookup")
	require.Len(t, mock.calls, 0, "deterministic knowledge floor must bypass the ReAct loop for this replay regression")
}

func TestReplayRegression_CurrentAccountGPUFactOverridesStale5090Knowledge(t *testing.T) {
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": manyInstancesWithNamedTargetGPU("new-5090", "Running", "5090"),
	}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentResourceInfo,
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "5090 尚未发布"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo}})

	reply, err := eng.Chat(context.Background(), "我的 5090 实例是哪台", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "5090")
	require.NotContains(t, reply, "尚未发布")
	require.Len(t, mock.calls, 0, "live instance facts must not be overridden by LLM common knowledge")
}

func TestReplayRegression_DiagnosisTargetFollowupResolvesNameFromFullSnapshot(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTarget("claude-write-test", "Stopped"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "ssh连不上"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "您有多个实例在运行，请问是哪一台实例 SSH 连不上？请提供实例名称或实例 ID。"},
	)

	reply, err := eng.Chat(context.Background(), "claude-write-test", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "claude-write-test")
	require.Contains(t, reply, "关机")
	require.NotContains(t, reply, "未找到")
	require.Len(t, mock.calls, 0, "target follow-up should continue diagnosis deterministically, not ask the LLM to rediscover the instance")
}

func TestReplayRegression_GPUInvisibleUsesUniqueGPUInstanceFromFullSnapshot(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTargetGPU("host-wyp", "Running", "5090"))
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentDiagnosis,
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "5090 尚未发布"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	eng.SetIntentPlanner(planner, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentDiagnosis}})

	reply, err := eng.Chat(context.Background(), "我选的5090套餐，进入实例后看不见显卡", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "host-wyp")
	require.Contains(t, reply, "5090")
	require.Contains(t, reply, "nvidia-smi")
	require.NotContains(t, reply, "尚未发布")
	require.NotContains(t, reply, "没有直接看到")
	require.Len(t, mock.calls, 0, "unique GPU instance diagnosis should be handled from live instance data")
}

func TestReplayRegression_DirectGPUInvisibleUsesLive5090Instance(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithNamedTargetGPU("host-wyp", "Running", "5090"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "5090 尚未发布"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "我选的5090套餐，进入实例后看不见显卡", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "host-wyp")
	require.Contains(t, reply, "5090")
	require.Contains(t, reply, "nvidia-smi")
	require.NotContains(t, reply, "尚未发布")
	require.NotContains(t, reply, "具体现象")
	require.Len(t, mock.calls, 0, "clear GPU-invisible diagnosis should not enter generic ReAct clarification")
}

func TestReplayRegression_DirectGPUInvisibleMultiple5090AsksForTarget(t *testing.T) {
	exec := replayInstanceExecutor(manyInstancesWithTwoGPUInstances("5090"))
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "我选的5090套餐，进入实例后看不见显卡", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "多台 5090")
	require.Contains(t, reply, "host-a")
	require.Contains(t, reply, "host-b")
	require.Contains(t, reply, "实例名称或实例 ID")
	require.NotContains(t, reply, "具体现象")
	require.Len(t, mock.calls, 0, "multiple same-GPU diagnosis should ask for target directly")
}

func TestReplayRegression_GenericNoResourceExplainsCapacityNotStatus(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "所有机型都是 Normal，没有售罄"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "一直暂无资源 是什么情况", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "不等于机型已经下架")
	require.Contains(t, reply, "Normal 只表示平台仍在售")
	require.Contains(t, reply, "容量预检")
	require.NotContains(t, reply, "所有机型都是 Normal")
	require.Len(t, mock.calls, 0, "generic stock shortage explanation should not let LLM turn status into stock")
}

func TestReplayRegression_DiskBillingQuestionDoesNotAskGPU(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "请先选择 GPU 型号"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "我没看懂收费，磁盘空间是如何收费的？100GB原始空间是免费的吗", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "系统盘默认 100GB 免费")
	require.Contains(t, reply, "数据盘")
	require.Contains(t, reply, "GPU、内存停止计费")
	require.NotContains(t, reply, "选择 GPU")
	require.Len(t, mock.calls, 0, "disk billing fact should not be routed to GPU pricing")
}

func TestReplayRegression_DiskBillingFollowupGPUModelStaysOnDisk(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "4090 价格如下"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.messages = append(eng.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: "磁盘空间是如何收费的？100GB原始空间是免费的吗",
	})

	reply, err := eng.Chat(context.Background(), "4090", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "磁盘空间收费和 GPU 型号无关")
	require.Contains(t, reply, "系统盘默认 100GB 免费")
	require.NotContains(t, reply, "4090 价格")
	require.Len(t, mock.calls, 0, "GPU-only follow-up after disk billing must stay in disk context")
}

func manyInstancesWithNamedTarget(name, state string) map[string]any {
	return manyInstancesWithNamedTargetGPU(name, state, "4090")
}

func manyInstancesWithNamedTargetGPU(name, state, gpuType string) map[string]any {
	rows := make([]any, 0, 17)
	for i := 0; i < 16; i++ {
		rows = append(rows, map[string]any{
			"UHostId": "uhost-fill",
			"Name":    "host-fill",
			"State":   "Running",
			"GpuType": "4090",
			"GPU":     float64(1),
			"CPU":     float64(16),
			"Memory":  float64(65536),
			"Zone":    "cn-wlcb-01",
		})
		rows[i].(map[string]any)["UHostId"] = "uhost-fill-" + string(rune('a'+i))
		rows[i].(map[string]any)["Name"] = "host-fill-" + string(rune('a'+i))
	}
	rows = append(rows, map[string]any{
		"UHostId": "uhost-target",
		"Name":    name,
		"State":   state,
		"GpuType": gpuType,
		"GPU":     float64(1),
		"CPU":     float64(16),
		"Memory":  float64(65536),
		"Zone":    "cn-wlcb-01",
	})
	return map[string]any{
		"TotalCount": float64(len(rows)),
		"UHostSet":   rows,
	}
}

func manyInstancesWithTwoGPUInstances(gpuType string) map[string]any {
	return map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-gpu-a",
				"Name":    "host-a",
				"State":   "Running",
				"GpuType": gpuType,
				"GPU":     float64(1),
				"CPU":     float64(16),
				"Memory":  float64(65536),
				"Zone":    "cn-wlcb-01",
			},
			map[string]any{
				"UHostId": "uhost-gpu-b",
				"Name":    "host-b",
				"State":   "Stopped",
				"GpuType": gpuType,
				"GPU":     float64(1),
				"CPU":     float64(16),
				"Memory":  float64(65536),
				"Zone":    "cn-wlcb-01",
			},
		},
	}
}

func replayInstanceExecutor(data map[string]any) *mockExecutorFn {
	return &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if ids := stringSliceArg(args["UHostIds"]); len(ids) > 0 {
				return filterDescribeInstances(data, ids), nil
			}
			return data, nil
		case "StopCompShareInstance", "StartCompShareInstance", "RebootCompShareInstance":
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
}

func stringSliceArg(value any) []string {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func filterDescribeInstances(data map[string]any, ids []string) map[string]any {
	wanted := map[string]struct{}{}
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var rows []any
	for _, item := range data["UHostSet"].([]any) {
		row := item.(map[string]any)
		if _, ok := wanted[row["UHostId"].(string)]; ok {
			rows = append(rows, item)
		}
	}
	return map[string]any{
		"TotalCount": float64(len(rows)),
		"UHostSet":   rows,
	}
}
