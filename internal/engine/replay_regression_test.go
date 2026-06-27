package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

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
