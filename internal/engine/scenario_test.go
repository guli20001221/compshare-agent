package engine

// Scenario-based W4 verification tests.
// Simulate real user conversations end-to-end:
// user message → LLM tool selection → tool execution → LLM reply.
// Reuses mockLLM and mockExecutor from engine_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/workflow"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
)

func tc(name string, args map[string]any) openai.ToolCall {
	b, _ := json.Marshal(args)
	return openai.ToolCall{
		ID:   "call_" + name,
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      name,
			Arguments: string(b),
		},
	}
}

// ── Scenario 1: "SSH连不上" — instance stopped → conclude immediately ──────

func TestScenario_SSH_InstanceStopped(t *testing.T) {
	instanceData := map[string]any{
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-1", "Name": "my-gpu", "State": "Stopped", "GPU": float64(1), "Tag": "4090"},
		},
	}

	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": instanceData,
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseSSH", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "您的实例处于关机状态，这就是SSH连不上的原因。需要帮您开机吗？"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	var events []StepEvent
	reply, err := eng.Chat(context.Background(), "SSH连不上了", func(e StepEvent) {
		events = append(events, e)
	})

	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")

	// Diagnosis step 1 calls DescribeCompShareInstance, detects Stopped, stops
	assert.Contains(t, exec.calls, "DescribeCompShareInstance")
	// Should NOT reach port or monitor checks
	callStr := strings.Join(exec.calls, ",")
	assert.NotContains(t, callStr, "DescribeCompShareSoftwarePort")
	assert.NotContains(t, callStr, "GetCompShareInstanceMonitor")
}

// ── Scenario 2: "SSH连不上" — running, CPU 99% → resource exhausted ────────

func TestScenario_SSH_ResourceExhausted(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "State": "Running", "Tag": "4090", "GPU": float64(1), "Name": "gpu"},
				},
			},
			"DescribeCompShareSoftwarePort": {
				"SoftwarePort": []any{
					map[string]any{"Software": "SSH", "Port": float64(22)},
				},
			},
			"GetCompShareInstanceMonitor": {
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"Metrics": []any{
								map[string]any{
									"MetricKey": "uhost_cpu_used",
									"Results":   []any{map[string]any{"Values": []any{map[string]any{"Value": float64(99.5)}}}},
								},
								map[string]any{
									"MetricKey": "cloudwatch_memory_usage",
									"Results":   []any{map[string]any{"Values": []any{map[string]any{"Value": float64(45.0)}}}},
								},
							},
						},
					},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseSSH", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "CPU 使用率 99.5%，资源耗尽导致 SSH 无法响应。建议重启实例。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "SSH连不上怎么办", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "99.5")

	// All 3 steps should execute: state → port → monitor
	assert.Contains(t, exec.calls, "DescribeCompShareSoftwarePort")
	assert.Contains(t, exec.calls, "GetCompShareInstanceMonitor")
}

// ── Scenario 3: "SSH连不上" — all normal → fallback ──────────────────────

func TestScenario_SSH_AllNormal_Fallback(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet":   []any{map[string]any{"UHostId": "uhost-1", "State": "Running", "Tag": "4090", "GPU": float64(1), "Name": "ok"}},
			},
			"DescribeCompShareSoftwarePort": {
				"SoftwarePort": []any{map[string]any{"Software": "SSH", "Port": float64(22)}},
			},
			"GetCompShareInstanceMonitor": {
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"Metrics": []any{
								map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(30.0)}}}}},
								map[string]any{"MetricKey": "cloudwatch_memory_usage", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(40.0)}}}}},
							},
						},
					},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseSSH", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "所有检查均正常。建议检查本地网络或使用 JupyterLab 替代。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "SSH连接超时", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "JupyterLab")
	// All 3 steps ran
	assert.Contains(t, exec.calls, "GetCompShareInstanceMonitor")
}

// ── Scenario 4: "初始化失败了" → DiagnoseInitFailure ──────────────────────

func TestScenario_InitFailure(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-fail", "Name": "broken", "State": "Install Fail", "Tag": "A100", "GPU": float64(1), "CompShareImageName": "PyTorch 2.1"},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseInitFailure", map[string]any{"UHostId": "uhost-fail"})}},
			{Content: "实例初始化失败，镜像 PyTorch 2.1。建议删除重建或更换镜像。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "我的实例初始化失败了", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "初始化失败")

	// Verify the diagnosis chain result (tool message fed to LLM round 2)
	// contains meaningful conclusion, not just routing.
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var diagResult map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &diagResult)
	assert.NoError(t, err)
	assert.Equal(t, true, diagResult["success"])
	conclusion, _ := diagResult["conclusion"].(string)
	assert.Contains(t, conclusion, "初始化失败")
	assert.Contains(t, conclusion, "PyTorch 2.1")
	suggestion, _ := diagResult["suggestion"].(string)
	assert.Contains(t, suggestion, "删除")
}

// ── Scenario 4b: "实例卡在启动中" → DiagnoseInitFailure — Starting 不收费 ──

func TestScenario_InitFailure_Starting(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-boot", "Name": "boot-gpu", "State": "Starting", "GpuType": "4090", "GPU": float64(1)},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseInitFailure", map[string]any{"UHostId": "uhost-boot"})}},
			{Content: "实例正在启动中，此状态不收费，请等待 1-2 分钟。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "实例卡在启动中不动了", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "启动")

	// Verify diagnosis chain JSON locks the Starting semantics
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var diagResult map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &diagResult)
	assert.NoError(t, err)
	assert.Equal(t, true, diagResult["success"])
	conclusion, _ := diagResult["conclusion"].(string)
	assert.Contains(t, conclusion, "启动中")
	assert.Contains(t, conclusion, "不产生费用", "Starting state must state no billing")
	suggestion, _ := diagResult["suggestion"].(string)
	assert.Contains(t, suggestion, "1-2 分钟")
}

// ── Scenario 5: "帮我开一台4090" → CreateInstanceWorkflow ─────────────────

func TestScenario_CreateInstance(t *testing.T) {
	confirmCalled := 0
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance":      {"TotalCount": float64(0), "UHostSet": []any{}},
			"DescribeCompShareImages":        {"ImageSet": []any{map[string]any{"CompShareImageId": "img-pt", "CompShareImageName": "PyTorch 2.1"}}},
			"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}},
			}},
			"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}}},
			"GetCompShareInstanceUserPrice":      {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}},
			"CreateCompShareInstance":        {"UHostIds": []any{"uhost-new1"}},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{
				"GpuType": "4090", "Zone": "cn-wlcb-01", "CompShareImageId": "img-pt", "ChargeType": "Dynamic",
			})}},
			{Content: "已创建 4090 实例 uhost-new1，正在初始化。"},
		},
	}

	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmCalled++
		return true
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我开一台4090", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "uhost-new1")
	assert.Equal(t, 1, confirmCalled, "Should ask for confirmation once")
	assert.Contains(t, exec.calls, "CreateCompShareInstance")
}

// ── Scenario 6: "帮我关机" → StopInstanceWorkflow with fee warning ────────

func TestScenario_StopInstance_FeeWarning(t *testing.T) {
	var confirmAction string
	var confirmArgs map[string]any

	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet":   []any{map[string]any{"UHostId": "uhost-1", "State": "Running", "Tag": "4090", "GPU": float64(1), "Name": "my-gpu"}},
			},
			"StopCompShareInstance": {"UHostId": "uhost-1"},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("StopInstanceWorkflow", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "已关机。注意：关机后磁盘费用仍会产生。"},
		},
	}

	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		confirmAction = action
		confirmArgs = args
		return true
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我把实例关了", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")
	assert.Contains(t, exec.calls, "StopCompShareInstance")

	// Verify fee warning in confirmation
	_ = confirmAction
	if confirmArgs != nil {
		warning, _ := confirmArgs["warning"].(string)
		assert.Contains(t, warning, "磁盘", "Confirmation should warn about disk fees")
	}
}

// ── Scenario 7: GPU 推荐 — 本地执行，不调 API ─────────────────────────────

func TestScenario_GPURecommendation_LocalOnly(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("GetGPURecommendation", map[string]any{"scene": "训练7B模型"})}},
			{Content: "训练 7B 模型建议 A100（80GB 显存）或 4090（24GB，需 LoRA）。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	callsBefore := len(exec.calls)
	reply, err := eng.Chat(context.Background(), "训练7B模型该选什么卡", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "A100")
	// Knowledge tools must NOT call the API executor
	assert.Equal(t, callsBefore, len(exec.calls), "Knowledge tool should not trigger API calls")
}

// ── Scenario 8: "nvidia-smi报错" → DiagnoseGPU — 无卡模式 ──────────────

func TestScenario_GPU_NoCardMode(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "my-gpu", "State": "Running", "GPU": float64(0), "Tag": "4090"},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseGPU", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "您的实例以无卡模式启动，未挂载 GPU。请关机后以正常模式重新开机。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "nvidia-smi检测不到显卡", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "无卡模式")
	// Should conclude at step 1, not reach GPU monitor
	callStr := strings.Join(exec.calls, ",")
	assert.NotContains(t, callStr, "GetCompShareInstanceMonitor")
}

// ── Scenario 9: "nvidia-smi报错" → DiagnoseGPU — GPU 正常工作（驱动问题）──

func TestScenario_GPU_DriverMismatch(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "State": "Running", "GPU": float64(1), "Tag": "4090", "Name": "gpu"},
				},
			},
			"GetCompShareInstanceMonitor": {
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"Metrics": []any{
								map[string]any{"MetricKey": "cloudwatch_gpu_util", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(35.0)}}}}},
								map[string]any{"MetricKey": "cloudwatch_gpu_memory_usage", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(20.0)}}}}},
							},
						},
					},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseGPU", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "GPU 硬件正常工作，nvidia-smi 报错可能是驱动版本不匹配。建议执行 ldconfig 或重启。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "nvidia-smi报错了", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "驱动")
	// Should reach step 2 (GPU monitor)
	assert.Contains(t, exec.calls, "GetCompShareInstanceMonitor")
}

// ── Scenario 10: "nvidia-smi报错" → DiagnoseGPU — 全部正常 → 兜底 ────────

func TestScenario_GPU_Fallback(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "State": "Running", "GPU": float64(1), "Tag": "4090", "Name": "ok"},
				},
			},
			"GetCompShareInstanceMonitor": {
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"Metrics": []any{
								map[string]any{"MetricKey": "cloudwatch_gpu_util", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(0.0)}}}}},
								map[string]any{"MetricKey": "cloudwatch_gpu_memory_usage", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(0.0)}}}}},
							},
						},
					},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseGPU", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "GPU 已分配但无活动，可能是驱动未加载。建议重启实例或换用官方镜像。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "nvidia-smi 检测不到卡", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "重启")
}

// ── Scenario 11: "为什么扣了这么多钱" → DiagnoseBilling ──────────────────

func TestScenario_Billing_RunningAndStopped(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(2),
				"UHostSet": []any{
					map[string]any{
						"UHostId": "uhost-1", "Name": "train", "GpuType": "A100", "GPU": float64(1),
						"State": "Running", "ChargeType": "Dynamic",
						"InstancePrice": float64(8.2), "DiskPrice": float64(0.04),
						"CompShareImagePrice": float64(0.30),
					},
					map[string]any{
						"UHostId": "uhost-2", "Name": "old", "GpuType": "4090", "GPU": float64(1),
						"State": "Stopped", "ChargeType": "Dynamic",
						"InstancePrice": float64(0), "DiskPrice": float64(0.04),
						"CompShareImagePrice": float64(0.30),
					},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseBilling", map[string]any{})}},
			{Content: "您有 2 台实例，运行中 A100 收费含实例费+磁盘费+镜像费，关机的 4090 仍产生磁盘和镜像保留费用。建议释放不用的关机实例。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "为什么扣了这么多钱", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "镜像")

	// Verify diagnosis chain result contains meaningful billing breakdown
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var diagResult map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &diagResult)
	assert.NoError(t, err)
	assert.Equal(t, true, diagResult["success"])
	conclusion, _ := diagResult["conclusion"].(string)
	assert.Contains(t, conclusion, "2 个实例")
	assert.Contains(t, conclusion, "uhost-1")
	assert.Contains(t, conclusion, "关机")
	assert.Contains(t, conclusion, "镜像费", "paid image cost must appear in billing conclusion")
	assert.Contains(t, conclusion, "0.30", "image price amount must appear")
	assert.Contains(t, conclusion, "镜像保留费用", "stopped instance warning must mention image cost")
	suggestion, _ := diagResult["suggestion"].(string)
	assert.Contains(t, suggestion, "释放")
	assert.Contains(t, suggestion, "镜像", "suggestion must mention image cost for stopped paid-image instances")
	assert.NotContains(t, suggestion, "GetCompShareInstancePrice", "should not expose internal API names to users")
}

// ── Scenario 12: "为什么扣了这么多钱" → DiagnoseBilling — 无实例 ──────────

func TestScenario_Billing_NoInstances(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(0),
				"UHostSet":   []any{},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseBilling", map[string]any{})}},
			{Content: "未找到任何实例。如果仍在扣费，可能是未释放的云盘。请到控制台查看。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "我没有实例了为什么还在扣钱", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "云盘")

	// Verify diagnosis chain result is meaningful, not just routing
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var diagResult map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &diagResult)
	assert.NoError(t, err)
	assert.Equal(t, true, diagResult["success"])
	conclusion, _ := diagResult["conclusion"].(string)
	assert.Contains(t, conclusion, "未找到任何实例")
	assert.Contains(t, conclusion, "控制台")
}

// ═══════════════════════════════════════════════════════════════════════════
// W5 Evaluation — 15 additional integration scenarios
// ═══════════════════════════════════════════════════════════════════════════

// mockExecutorWithErrors supports returning errors for specific actions.
type mockExecutorWithErrors struct {
	results map[string]map[string]any
	errors  map[string]error
	calls   []string
}

func (m *mockExecutorWithErrors) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	if err, ok := m.errors[action]; ok {
		return nil, err
	}
	if r, ok := m.results[action]; ok {
		return r, nil
	}
	return map[string]any{"Action": action, "RetCode": 0}, nil
}

// ── Scenario 13: API tool returns error → engine feeds error back to LLM ──

func TestScenario_ToolExecutionError(t *testing.T) {
	exec := &mockExecutorWithErrors{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
		},
		errors: map[string]error{
			"GetCompShareInstanceUserPrice": fmt.Errorf("API timeout: connection refused"),
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("GetCompShareInstanceUserPrice", map[string]any{
				"Zone": "cn-wlcb-a", "GpuType": "4090", "Gpu": float64(1), "Cpu": float64(16), "Memory": float64(65536),
			})}},
			{Content: "抱歉，查询价格时出现网络错误，请稍后再试。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	var events []StepEvent
	reply, err := eng.Chat(context.Background(), "4090多少钱", func(e StepEvent) {
		events = append(events, e)
	})

	assert.NoError(t, err)
	assert.Contains(t, reply, "网络错误")

	// Should have an error event
	hasError := false
	for _, ev := range events {
		if ev.Type == StepError && strings.Contains(ev.Message, "API 调用失败") {
			hasError = true
		}
	}
	assert.True(t, hasError, "API error should produce StepError event")

	// Error message should be fed back to LLM
	lastMsgs := mock.calls[1].Messages
	toolMsg := lastMsgs[len(lastMsgs)-1]
	assert.Contains(t, toolMsg.Content, "API 调用失败")
}

// ── Scenario 14: FormatToolResult truncation for oversized response ────────

func TestScenario_FormatToolResultTruncation(t *testing.T) {
	// Build a large UHostSet (50 instances) that exceeds 4000 runes
	largeSet := make([]any, 50)
	for i := 0; i < 50; i++ {
		largeSet[i] = map[string]any{
			"UHostId": fmt.Sprintf("uhost-%03d", i), "Name": fmt.Sprintf("instance-%03d-with-long-name-padding", i),
			"State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			"IP": "10.0.0.1", "CreateTime": float64(1713000000), "ExpireTime": float64(0),
		}
	}

	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(50), "UHostSet": largeSet},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DescribeCompShareInstance", map[string]any{})}},
			{Content: "您有50个实例"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "查全部实例", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "50")

	// Tool result should be valid JSON (not truncated mid-string)
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	var parsed map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &parsed)
	assert.NoError(t, err, "truncated tool result must be valid JSON: %s", toolMsg.Content[:100])

	// Should contain truncation notice
	assert.Contains(t, toolMsg.Content, "截取前")
}

// ── Scenario 15: Workflow step failure — capacity check fails ──────────────

func TestScenario_WorkflowCapacityInsufficient(t *testing.T) {
	// Workflow engine stops on executor errors. Capacity check failure = executor error.
	exec := &mockExecutorWithErrors{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
			"DescribeCompShareImages":  {"ImageSet": []any{map[string]any{"CompShareImageId": "img-pt", "CompShareImageName": "PyTorch 2.1"}}},
		},
		errors: map[string]error{
			"CheckCompShareResourceCapacity": fmt.Errorf("ResourceNotEnough: H20 库存不足"),
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{"GpuType": "H20"})}},
			{Content: "抱歉，H20 库存不足，建议换用 A100 或稍后再试。"},
		},
	}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我开一台H20", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "库存不足")

	// Should NOT have reached CreateCompShareInstance
	for _, c := range exec.calls {
		assert.NotEqual(t, "CreateCompShareInstance", c, "should not attempt creation when capacity check errors")
	}
}

// ── Scenario 16: Workflow confirm step cancelled ───────────────────────────

func TestScenario_WorkflowConfirmCancelled(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
			"DescribeCompShareImages":  {"ImageSet": []any{map[string]any{"CompShareImageId": "img-pt", "CompShareImageName": "PyTorch"}}},
			"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "ResourceEnough": true}}},
			"GetCompShareInstanceUserPrice":      {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{"GpuType": "4090"})}},
			{Content: "好的，已取消创建。"},
		},
	}

	// User rejects at confirm step
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我创建一台4090", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "取消")

	// Should NOT have called CreateCompShareInstance
	for _, c := range exec.calls {
		assert.NotEqual(t, "CreateCompShareInstance", c, "should not create when user cancels")
	}
}

// ── Scenario 17: SSH diagnosis — port not open → early exit at step 2 ─────

func TestScenario_SSH_PortNotOpen(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "State": "Running", "Tag": "4090", "GPU": float64(1), "Name": "gpu"},
				},
			},
			"DescribeCompShareSoftwarePort": {
				"SoftwarePort": []any{
					// SSH port missing — only JupyterLab listed
					map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseSSH", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "该实例未发现 SSH 服务，建议使用 JupyterLab 替代或选择包含 SSH 的镜像。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "SSH连不上", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "SSH")

	// Should stop at step 2 (port check), NOT reach monitor
	callStr := strings.Join(exec.calls, ",")
	assert.Contains(t, callStr, "DescribeCompShareSoftwarePort")
	assert.NotContains(t, callStr, "GetCompShareInstanceMonitor", "should not reach monitor when port is not open")
}

// ── Scenario 18: Multi-turn context — query then operate ──────────────────

func TestScenario_MultiTurnContext(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-mt1", "State": "Running", "GpuType": "4090", "GPU": float64(1), "Name": "my-gpu", "ChargeType": "Dynamic"},
				},
			},
			"StopCompShareInstance": {"RetCode": 0},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			// Turn 1: query instances
			{ToolCalls: []openai.ToolCall{tc("DescribeCompShareInstance", map[string]any{})}},
			{Content: "您有 1 个实例 my-gpu (uhost-mt1)，4090，运行中。"},
			// Turn 2: user says "关掉它" referencing the instance from turn 1
			{ToolCalls: []openai.ToolCall{tc("StopInstanceWorkflow", map[string]any{"UHostId": "uhost-mt1"})}},
			{Content: "已关机 my-gpu。注意磁盘费用仍会产生。"},
		},
	}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	// Turn 1
	reply1, err := eng.Chat(context.Background(), "我有什么实例", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply1, "uhost-mt1")

	// Turn 2 — relies on conversation context from turn 1
	reply2, err := eng.Chat(context.Background(), "关掉它", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply2, "关机")

	// LLM call for turn 2 should include turn 1 messages in history
	turn2Msgs := mock.calls[2].Messages // call index 2 = third LLM call
	assert.True(t, len(turn2Msgs) >= 4, "turn 2 should have history from turn 1")
}

// ── Scenario 19: New user empty context + correct suggestions ─────────────

func TestScenario_NewUserEmptyContext(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
		},
	}

	eng := NewWithDeps(&mockLLM{}, exec, nil)
	suggestions, err := eng.Init(context.Background())

	assert.NoError(t, err)
	assert.NotEmpty(t, suggestions)

	// New user suggestions should include onboarding content
	suggTexts := make([]string, len(suggestions))
	for i, s := range suggestions {
		suggTexts[i] = s.Text
	}
	allText := strings.Join(suggTexts, " ")
	// Should suggest one of: intro, pricing, or getting started
	assert.True(t,
		strings.Contains(allText, "入门") || strings.Contains(allText, "价格") || strings.Contains(allText, "推荐") || strings.Contains(allText, "GPU"),
		"new user suggestions should include onboarding: got %v", suggTexts)

	// System prompt should say "没有实例"
	assert.Contains(t, eng.messages[0].Content, "没有实例")
}

// ── Scenario 20: Filter params on external API call in flow ───────────────

func TestScenario_FilterParamsInFlow(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
		},
	}

	// LLM injects extra parameters (Region, ProjectId) not in schema
	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DescribeCompShareInstance", map[string]any{
				"UHostIds": []any{"uhost-xxx"}, "Region": "cn-bj2", "ProjectId": "proj-evil",
			})}},
			{Content: "查询完成"},
		},
	}

	onStep, events := collectSteps()
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	_, err := eng.Chat(context.Background(), "查实例", onStep)
	assert.NoError(t, err)

	// Verify injected params were stripped
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DescribeCompShareInstance" {
			assert.NotContains(t, ev.Args, "Region", "Region should be filtered out")
			assert.NotContains(t, ev.Args, "ProjectId", "ProjectId should be filtered out")
			assert.Contains(t, ev.Args, "UHostIds", "UHostIds should be preserved")
		}
	}
}

// ── Scenario 21: Diagnosis engine error propagation ───────────────────────

func TestScenario_DiagnosisAPIError(t *testing.T) {
	exec := &mockExecutorWithErrors{
		results: map[string]map[string]any{},
		errors: map[string]error{
			"DescribeCompShareInstance": fmt.Errorf("MongoDB connection refused"),
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseSSH", map[string]any{"UHostId": "uhost-1"})}},
			{Content: "诊断过程中遇到系统错误，请稍后重试。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	var events []StepEvent
	reply, err := eng.Chat(context.Background(), "SSH连不上", func(e StepEvent) {
		events = append(events, e)
	})
	assert.NoError(t, err)
	assert.Contains(t, reply, "错误")

	// The error should be propagated back to LLM
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
}

// ── Scenario 22: Workflow with executor error on create step ──────────────

func TestScenario_WorkflowCreateAPIError(t *testing.T) {
	exec := &mockExecutorWithErrors{
		results: map[string]map[string]any{
			"DescribeCompShareInstance":      {"TotalCount": float64(0), "UHostSet": []any{}},
			"DescribeCompShareImages":        {"ImageSet": []any{map[string]any{"CompShareImageId": "img-pt", "CompShareImageName": "PyTorch"}}},
			"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "ResourceEnough": true}}},
			"GetCompShareInstanceUserPrice":      {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}},
		},
		errors: map[string]error{
			"CreateCompShareInstance": fmt.Errorf("insufficient balance"),
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{"GpuType": "4090"})}},
			{Content: "创建失败：余额不足，请充值后重试。"},
		},
	}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我开一台4090", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "余额不足")

	// Workflow result fed to LLM should indicate failure
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	var result map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &result)
	assert.NoError(t, err)
	assert.Equal(t, false, result["success"])
}

// ── Scenario 23: StartInstanceWorkflow full success ──────────────────────

func TestScenario_StartInstanceWorkflow(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet":   []any{map[string]any{"UHostId": "uhost-start1", "State": "Stopped", "GpuType": "3080Ti", "GPU": float64(1), "Name": "test"}},
			},
			"StartCompShareInstance": {"RetCode": 0},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("StartInstanceWorkflow", map[string]any{"UHostId": "uhost-start1"})}},
			{Content: "已开机 test (uhost-start1)。"},
		},
	}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "把 test 开起来", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "开机")
	assert.Contains(t, exec.calls, "StartCompShareInstance")
}

// ── Scenario 24: Billing diagnosis — single instance ─────────────────────

func TestScenario_Billing_SingleInstance(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{
						"UHostId": "uhost-bill1", "Name": "train", "GpuType": "4090", "GPU": float64(1),
						"State": "Running", "ChargeType": "Dynamic",
						"InstancePrice": float64(1.58), "DiskPrice": float64(0.04),
					},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseBilling", map[string]any{"UHostId": "uhost-bill1"})}},
			{Content: "4090 按量运行中 ¥1.58/时 + 磁盘 ¥0.04/时。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "uhost-bill1 一小时多少钱", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "1.58")
}

// ── Scenario 25: GPU diagnosis — instance not running → early exit ────────

func TestScenario_GPU_InstanceStopped(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-gpu1", "State": "Stopped", "GPU": float64(1), "Tag": "4090", "Name": "gpu-box"},
				},
			},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DiagnoseGPU", map[string]any{"UHostId": "uhost-gpu1"})}},
			{Content: "实例已关机，无法检测 GPU。请先开机。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "nvidia-smi 报错", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")

	// Should not reach GPU monitor
	callStr := strings.Join(exec.calls, ",")
	assert.NotContains(t, callStr, "GetCompShareInstanceMonitor")
}

// ── Scenario 26: Knowledge tool in full flow — no API calls ──────────────

func TestScenario_KnowledgeTool_GPUSpecs_NoAPI(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("GetGPUSpecs", map[string]any{"GpuType": "H20"})}},
			{Content: "H20 显存 96GB，适合大模型推理。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	callsBefore := len(exec.calls) // after Init
	reply, err := eng.Chat(context.Background(), "H20是什么配置", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "96")

	// No new API calls should happen for knowledge tools
	assert.Equal(t, callsBefore, len(exec.calls), "knowledge tool should not trigger additional API calls")
}

// ── Scenario 27: Active user context + appropriate suggestions ────────────

func TestScenario_ActiveUserContext(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-active1", "State": "Running", "GpuType": "4090", "GPU": float64(1), "Name": "train", "ChargeType": "Dynamic"},
				},
			},
		},
	}

	eng := NewWithDeps(&mockLLM{}, exec, nil)
	suggestions, err := eng.Init(context.Background())

	assert.NoError(t, err)
	assert.NotEmpty(t, suggestions)

	// Active user suggestions should be action-oriented
	suggTexts := make([]string, len(suggestions))
	for i, s := range suggestions {
		suggTexts[i] = s.Text
	}
	allText := strings.Join(suggTexts, " ")
	assert.True(t,
		strings.Contains(allText, "状态") || strings.Contains(allText, "关机") || strings.Contains(allText, "花了"),
		"active user suggestions should be action-oriented: got %v", suggTexts)

	// System prompt should contain the running instance
	assert.Contains(t, eng.messages[0].Content, "uhost-active1")
	assert.Contains(t, eng.messages[0].Content, "运行中")
}

// ── Tool registration verification ────────────────────────────────────────

func TestScenario_AllToolsRegistered(t *testing.T) {
	// Diagnosis tools (6)
	assert.True(t, diagnosis.IsDiagnosisTool("DiagnoseSSH"))
	assert.True(t, diagnosis.IsDiagnosisTool("DiagnoseInitFailure"))
	assert.True(t, diagnosis.IsDiagnosisTool("DiagnoseGPU"))
	assert.True(t, diagnosis.IsDiagnosisTool("DiagnoseBilling"))
	assert.True(t, diagnosis.IsDiagnosisTool("DiagnosePortOrFirewall"))
	assert.True(t, diagnosis.IsDiagnosisTool("DiagnoseImageIssue"))
	assert.False(t, diagnosis.IsDiagnosisTool("NonExistent"))

	// Workflow tools (6)
	assert.True(t, workflow.IsWorkflowTool("CreateInstanceWorkflow"))
	assert.True(t, workflow.IsWorkflowTool("StopInstanceWorkflow"))
	assert.True(t, workflow.IsWorkflowTool("StartInstanceWorkflow"))
	assert.True(t, workflow.IsWorkflowTool("RebootInstanceWorkflow"))
	assert.True(t, workflow.IsWorkflowTool("RenameInstanceWorkflow"))
	assert.True(t, workflow.IsWorkflowTool("ResetPasswordWorkflow"))
	assert.False(t, workflow.IsWorkflowTool("NonExistent"))
}

// ── Scenario 29: Reboot workflow — success ───────────────────────────────

func TestScenario_RebootInstance_Success(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{map[string]any{"UHostId": "uhost-r1", "State": "Running", "Name": "gpu-1", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"}},
			},
			"RebootCompShareInstance": {"RetCode": 0},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("RebootInstanceWorkflow", map[string]any{"UHostId": "uhost-r1"})}},
		{Content: "已重启 gpu-1。"},
	}}
	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "重启一下 uhost-r1", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "重启")
	assert.Contains(t, exec.calls, "RebootCompShareInstance")
}

// ── Scenario 30: Reboot workflow — not running ───────────────────────────

func TestScenario_RebootInstance_NotRunning(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{map[string]any{"UHostId": "uhost-r2", "State": "Stopped", "Name": "gpu-2"}},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("RebootInstanceWorkflow", map[string]any{"UHostId": "uhost-r2"})}},
		{Content: "实例当前是关机状态，无法重启。"},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "重启实例", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")
	// RebootCompShareInstance should NOT be called
	callStr := strings.Join(exec.calls, ",")
	assert.NotContains(t, callStr, "RebootCompShareInstance")
}

// ── Scenario 31: Rename workflow ──────────────────────────────────────────

func TestScenario_RenameInstance(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance":     {"UHostSet": []any{map[string]any{"UHostId": "uhost-rn1", "State": "Running", "Name": "old-name", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"}}},
			"ModifyCompShareInstanceName": {"UHostId": "uhost-rn1", "RetCode": 0},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("RenameInstanceWorkflow", map[string]any{"UHostId": "uhost-rn1", "Name": "new-name"})}},
		{Content: "已改名为 new-name。"},
	}}
	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "改名叫 new-name", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "new-name")
	assert.Contains(t, exec.calls, "ModifyCompShareInstanceName")
}

// ── Scenario 32: ResetPassword workflow (with sanitization) ──────────────

func TestScenario_ResetPassword(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance":          {"UHostSet": []any{map[string]any{"UHostId": "uhost-pw1", "State": "Stopped", "InstanceType": "Normal", "Name": "vm-1", "GpuType": "A100", "GPU": float64(1), "ChargeType": "Month"}}},
			"ResetCompShareInstancePassword": {"UHostId": "uhost-pw1", "RetCode": 0},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("ResetPasswordWorkflow", map[string]any{"UHostId": "uhost-pw1", "Password": "NewPass123!"})}},
		{Content: "密码已重置。"},
	}}

	var events []StepEvent
	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool {
		// Verify password is masked in confirm args
		if pw, ok := args["Password"]; ok {
			assert.Equal(t, "[已设置,不显示]", pw)
		}
		return true
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "重置密码", func(e StepEvent) {
		events = append(events, e)
	})
	assert.NoError(t, err)
	assert.Contains(t, reply, "重置")

	// Verify password in event args is sanitized
	for _, ev := range events {
		if ev.Args != nil {
			if pw, ok := ev.Args["Password"]; ok {
				assert.NotEqual(t, "NewPass123!", pw, "password should be sanitized in events")
			}
		}
	}
}

// ── Scenario 33: Create instance with community image ────────────────────

func TestScenario_CreateInstance_CommunityImage(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCommunityImages":          {"CompshareImageGroup": []any{map[string]any{"ImageName": "ComfyUI", "Data": []any{map[string]any{"CompShareImageId": "cimg-001", "Name": "ComfyUI v1.0"}}}}},
			"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}},
			}},
			"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}}},
			"GetCompShareInstanceUserPrice":      {"PriceDetails": []any{map[string]any{"Price": 1.58}}},
			"CreateCompShareInstance":        {"UHostIds": []any{"uhost-new"}},
			"DescribeCompShareInstance":      {"UHostSet": []any{map[string]any{"UHostId": "uhost-new", "State": "Running"}}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{"GpuType": "4090", "ImageSource": "community", "ImageName": "ComfyUI"})}},
		{Content: "已用 ComfyUI 社区镜像创建实例。"},
	}}
	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "用ComfyUI社区镜像开一台4090", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "ComfyUI")
	assert.Contains(t, exec.calls, "DescribeCommunityImages")
}

// ── Scenario 33b: Create instance with platform App image (PyTorch) ──────

func TestScenario_CreateInstance_PlatformAppImage(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"TotalCount": float64(0), "UHostSet": []any{}},
			"DescribeCompShareImages": {"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04", "ImageType": "System"},
				map[string]any{"CompShareImageId": "img-pytorch", "Name": "PyTorch 2.1 CUDA 12.1", "ImageType": "App"},
			}},
			"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "MachineSizes": []any{
					map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}},
				}},
			}},
			"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}}},
			"GetCompShareInstanceUserPrice":            {"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}},
			"CreateCompShareInstance":                  {"UHostIds": []any{"uhost-pt1"}},
		},
	}

	// Capture confirm args to verify the image shown in confirmation card
	var confirmImage any
	confirmFn := func(action string, args map[string]any) bool {
		confirmImage = args["image"]
		return true
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		// LLM passes ImageName="PyTorch" for platform path
		{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{
			"GpuType": "4090", "ImageName": "PyTorch",
		})}},
		{Content: "已用 PyTorch 2.1 CUDA 12.1 镜像创建 4090 实例 uhost-pt1。"},
	}}
	eng := NewWithDeps(mock, exec, confirmFn)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我开一台4090跑PyTorch", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "PyTorch")

	// Verify DescribeCompShareImages was called (not community)
	assert.Contains(t, exec.calls, "DescribeCompShareImages")
	assert.NotContains(t, exec.calls, "DescribeCommunityImages")

	// KEY ASSERTION: The confirmation card must show PyTorch, NOT Ubuntu.
	// This is the end-to-end proof that name matching selected the correct image.
	assert.Equal(t, "PyTorch 2.1 CUDA 12.1", confirmImage,
		"confirm card should show PyTorch App image, not bare Ubuntu")

	// The workflow result fed to LLM should show success
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var wfResult map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &wfResult)
	assert.NoError(t, err)
	assert.Equal(t, true, wfResult["success"])
}

// ── Scenario 34: Port/firewall — service not found ───────────────────────

func TestScenario_PortFirewall_ServiceNotFound(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance":     {"UHostSet": []any{map[string]any{"UHostId": "uhost-pf1", "State": "Running"}}},
			"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{map[string]any{"Software": "SSH", "Port": float64(22)}}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("DiagnosePortOrFirewall", map[string]any{"UHostId": "uhost-pf1", "Service": "redis"})}},
		{Content: "平台端口列表中未找到 redis 服务。"},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "redis端口不通", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "redis")
}

// ── Scenario 35: Port/firewall — instance not running ────────────────────

func TestScenario_PortFirewall_NotRunning(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-pf2", "State": "Stopped"}}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("DiagnosePortOrFirewall", map[string]any{"UHostId": "uhost-pf2"})}},
		{Content: "实例未运行，需先开机。"},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "JupyterLab打不开", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "开机")
	callStr := strings.Join(exec.calls, ",")
	assert.NotContains(t, callStr, "DescribeCompShareSoftwarePort")
}

// ── Scenario 36: Image issue — install fail ──────────────────────────────

func TestScenario_ImageIssue_InstallFail(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-img1", "State": "Install Fail", "CompShareImageName": "SD WebUI"}}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("DiagnoseImageIssue", map[string]any{"UHostId": "uhost-img1"})}},
		{Content: "初始化失败，可能是镜像问题。建议换用官方镜像。"},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "镜像初始化失败了", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "镜像")
}

// ── Scenario 37: Image issue — community image ──────────────────────────

func TestScenario_ImageIssue_CommunityImage(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-img2", "State": "Running", "CompShareImageType": "Community", "CompShareImageName": "Dify v0.8"}}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("DiagnoseImageIssue", map[string]any{"UHostId": "uhost-img2"})}},
		{Content: "社区镜像可能存在兼容性问题，建议联系镜像作者。"},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "社区镜像用不了", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "社区镜像")
}

// ── Scenario 38: Security — L2 block ─────────────────────────────────────

func TestScenario_SecurityBlock_L2(t *testing.T) {
	exec := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("TerminateCompShareInstance", map[string]any{"UHostId": "uhost-del1"})}},
		{Content: "抱歉，删除实例是破坏性操作，已拒绝执行。请到控制台手动操作。"},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	var blocked bool
	reply, err := eng.Chat(context.Background(), "帮我删除这台实例", func(e StepEvent) {
		if e.Type == StepBlocked {
			blocked = true
		}
	})
	assert.NoError(t, err)
	assert.True(t, blocked, "L2 operation should be blocked")
	assert.Contains(t, reply, "控制台")
	// TerminateCompShareInstance should NOT be executed
	assert.NotContains(t, exec.calls, "TerminateCompShareInstance")
}

// ── Scenario 39: Sanitize — JupyterToken ─────────────────────────────────

func TestScenario_Sanitize_JupyterToken(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{map[string]any{"UHostId": "uhost-jt1", "State": "Running", "Name": "test"}},
			},
			"DescribeCompShareJupyterToken": {
				"DataSet": []any{map[string]any{"JupyterToken": "secret-jwt-token-12345"}},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("DescribeCompShareJupyterToken", map[string]any{"UHostIds": []any{"uhost-jt1"}})}},
		{Content: "已获取 Jupyter Token，请通过安全通道查看。"},
	}}

	var displayContent string
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "查一下jupyter token", func(e StepEvent) {
		if e.Display != "" {
			displayContent = e.Display
		}
	})
	assert.NoError(t, err)

	// LLM should NOT see the real token (sanitized in tool result fed back)
	// The second LLM call receives the sanitized tool result
	assert.True(t, len(mock.calls) >= 2, "should have at least 2 LLM calls")
	lastReq := mock.calls[len(mock.calls)-1]
	for _, msg := range lastReq.Messages {
		if msg.Role == "tool" {
			assert.NotContains(t, msg.Content, "secret-jwt-token-12345", "real token should be sanitized in LLM context")
			assert.Contains(t, msg.Content, "已获取", "sanitized placeholder should be present")
		}
	}

	// CLI Display should contain the real token
	assert.Contains(t, displayContent, "secret-jwt-token-12345", "Display should show real token for CLI")

	_ = reply
}

// ── Scenario 40: Sanitize — Password ─────────────────────────────────────

func TestScenario_Sanitize_Password(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance":          {"UHostSet": []any{map[string]any{"UHostId": "uhost-pw2", "State": "Stopped", "InstanceType": "Normal", "Name": "vm-2", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"}}},
			"ResetCompShareInstancePassword": {"UHostId": "uhost-pw2", "RetCode": 0},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("ResetPasswordWorkflow", map[string]any{"UHostId": "uhost-pw2", "Password": "Abcd1234!"})}},
		{Content: "密码已重置成功。"},
	}}

	var events []StepEvent
	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	_, err := eng.Chat(context.Background(), "重置密码为 Abcd1234!", func(e StepEvent) {
		events = append(events, e)
	})
	assert.NoError(t, err)

	// Check that no event leaks the raw password
	for _, ev := range events {
		if ev.Args != nil {
			if pw, ok := ev.Args["Password"]; ok {
				pwStr, _ := pw.(string)
				assert.NotEqual(t, "Abcd1234!", pwStr, "raw password should not appear in events")
			}
		}
	}
}

// ── Scenario 41-44: Multi-instance disambiguation ────────────────────────
// NOTE: These tests verify ENGINE PIPELINE behavior — that no workflow is
// triggered when the LLM returns a text-only clarification response.
// They do NOT verify that the real model will actually ask for clarification;
// that is covered by offline eval cases cl_01..cl_04 which run against real LLMs.
func TestScenario_Disambiguate_StopMultipleInstances(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(3),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "GpuType": "4090", "GPU": float64(1)},
					map[string]any{"UHostId": "uhost-2", "Name": "train-b", "State": "Running", "GpuType": "A100", "GPU": float64(1)},
					map[string]any{"UHostId": "uhost-3", "Name": "dev", "State": "Stopped", "GpuType": "3090", "GPU": float64(1)},
				},
			},
		},
	}

	// LLM correctly asks for clarification instead of calling workflow
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您有多台实例，请问要关闭哪台？\n1. train-a (uhost-1) — 运行中\n2. train-b (uhost-2) — 运行中"},
	}}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "关机吧", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "哪台")
	// No workflow should be called
	assert.NotContains(t, exec.calls, "StopCompShareInstance")
}

// "重启实例" with 2 running instances — should ask which one
func TestScenario_Disambiguate_RebootMultipleInstances(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(2),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "gpu-a", "State": "Running", "GpuType": "4090", "GPU": float64(1)},
					map[string]any{"UHostId": "uhost-2", "Name": "gpu-b", "State": "Running", "GpuType": "4090", "GPU": float64(1)},
				},
			},
		},
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您有2台运行中的实例，要重启哪台？"},
	}}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "重启实例", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "哪台")
	assert.NotContains(t, exec.calls, "RebootCompShareInstance")
}

// "帮我重置密码" with 2 instances — should ask which one
func TestScenario_Disambiguate_ResetPasswordMultiple(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(2),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "prod", "State": "Running", "InstanceType": "Container"},
					map[string]any{"UHostId": "uhost-2", "Name": "dev", "State": "Stopped", "InstanceType": "Normal"},
				},
			},
		},
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您有2台实例，要重置哪台的密码？\n- prod (uhost-1)\n- dev (uhost-2)"},
	}}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我重置密码", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "哪台")
	assert.NotContains(t, exec.calls, "ResetCompShareInstancePassword")
}

// Explicit UHostId with multiple instances — should execute directly, no disambiguation
func TestScenario_Disambiguate_ExplicitIdBypass(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(3),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"},
					map[string]any{"UHostId": "uhost-2", "Name": "train-b", "State": "Running", "GpuType": "A100", "GPU": float64(1), "ChargeType": "Dynamic"},
					map[string]any{"UHostId": "uhost-3", "Name": "dev", "State": "Stopped", "GpuType": "3090", "GPU": float64(1), "ChargeType": "Month"},
				},
			},
			"RebootCompShareInstance": {"RetCode": 0},
		},
	}

	// LLM correctly identifies the target and calls workflow directly
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("RebootInstanceWorkflow", map[string]any{"UHostId": "uhost-2"})}},
		{Content: "已重启 train-b (uhost-2)。"},
	}}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "重启一下 uhost-2", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "重启")
	assert.Contains(t, exec.calls, "RebootCompShareInstance")
}

// ── Scenario 45: Engine-level UHostId guard ───────────────────────────────
// When LLM calls an instance-operation workflow without UHostId,
// the engine should block it before entering the workflow.
func TestScenario_EngineGuard_MissingUHostId(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(2),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "a", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"},
					map[string]any{"UHostId": "uhost-2", "Name": "b", "State": "Running", "GpuType": "A100", "GPU": float64(1), "ChargeType": "Dynamic"},
				},
			},
		},
	}

	// LLM incorrectly calls StopInstanceWorkflow without UHostId
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("StopInstanceWorkflow", map[string]any{})}},
		{Content: "请告诉我要关闭哪台实例。"},
	}}

	var events []StepEvent
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	// Reset calls after Init (Init calls DescribeCompShareInstance for context)
	exec.calls = nil

	reply, err := eng.Chat(context.Background(), "关机", func(e StepEvent) {
		events = append(events, e)
	})
	assert.NoError(t, err)

	// Engine should block the workflow before it even queries the API
	assert.NotContains(t, exec.calls, "DescribeCompShareInstance",
		"workflow should not reach the API when UHostId is missing")

	// Should have a StepBlocked event
	hasBlocked := false
	for _, ev := range events {
		if ev.Type == StepBlocked && ev.Action == "StopInstanceWorkflow" {
			hasBlocked = true
		}
	}
	assert.True(t, hasBlocked, "should emit StepBlocked event")

	// LLM should still produce a reply
	assert.NotEmpty(t, reply)
}

// Engine guard should NOT block CreateInstanceWorkflow (it doesn't need UHostId)
func TestScenario_EngineGuard_CreateInstanceBypasses(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareImages": {"ImageSet": []any{map[string]any{"CompShareImageId": "img-1", "CompShareImageName": "PyTorch 2.1"}}},
			"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{map[string]any{
				"Name": "4090",
				"MachineSizes": []any{map[string]any{
					"Gpu": float64(1),
					"Collection": []any{map[string]any{
						"Cpu":    float64(16),
						"Memory": []any{float64(64)},
					}},
				}},
			}}},
			"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{"Gpu": float64(1), "GpuType": "4090", "ResourceEnough": true, "Cpu": float64(16), "Mem": float64(64)}}},
			"GetCompShareInstanceUserPrice":  {"PriceDetails": []any{map[string]any{"ChargeType": "Dynamic", "Price": float64(3.5)}}},
			"CreateCompShareInstance":        {"UHostIds": []any{"uhost-new"}},
			"DescribeCompShareInstance":      {"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-new", "State": "Install"}}},
		},
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("CreateInstanceWorkflow", map[string]any{
			"GpuType": "4090", "Zone": "cn-wlcb-01", "CompShareImageId": "img-1", "ChargeType": "Dynamic",
		})}},
		{Content: "实例已创建。"},
	}}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	// Reset calls after Init
	exec.calls = nil

	_, err := eng.Chat(context.Background(), "开一台4090", func(e StepEvent) {})
	assert.NoError(t, err)

	// CreateInstanceWorkflow should proceed normally (no UHostId guard)
	assert.Contains(t, exec.calls, "CreateCompShareInstance")
}

// ── Scenario: SetStopScheduler — happy path ──────────────────────────────
func TestScenario_SetStopScheduler_Success(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{
						"UHostId": "uhost-xxx", "Name": "my-gpu",
						"State": "Running", "Zone": "cn-bj2-04",
						"GpuType": "4090", "GPU": float64(1),
						"ChargeType": "Dynamic",
					},
				},
			},
			"UpdateCompShareStopScheduler": {"RetCode": 0},
		},
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("SetStopSchedulerWorkflow", map[string]any{
			"UHostId": "uhost-xxx", "AfterMinutes": float64(60),
		})}},
		{Content: "已为 my-gpu 设置 1 小时后自动关机。"},
	}}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "1小时后自动关机", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")
	// Reset calls after Init to isolate workflow calls
	// (Init calls DescribeCompShareInstance for context)
	assert.Contains(t, exec.calls, "UpdateCompShareStopScheduler")
}

// ── Scenario: CancelStopScheduler — happy path ──────────────────────────
func TestScenario_CancelStopScheduler_Success(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{
					map[string]any{
						"UHostId": "uhost-xxx", "Name": "my-gpu",
						"State": "Running", "Zone": "cn-bj2-04",
						"GpuType": "4090", "GPU": float64(1),
						"ChargeType": "Dynamic",
					},
				},
			},
			"DeleteCompShareStopScheduler": {"RetCode": 0},
		},
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{tc("CancelStopSchedulerWorkflow", map[string]any{
			"UHostId": "uhost-xxx",
		})}},
		{Content: "已取消 my-gpu 的定时关机。"},
	}}

	eng := NewWithDeps(mock, exec, func(a string, args map[string]any) bool { return true })
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "取消定时关机", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "取消")
	assert.Contains(t, exec.calls, "DeleteCompShareStopScheduler")
}

// ── Scenario: Scheduler disambiguation — multiple instances ──────────────
func TestScenario_Disambiguate_SchedulerMultipleInstances(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(2),
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"},
					map[string]any{"UHostId": "uhost-2", "Name": "train-b", "State": "Running", "GpuType": "A100", "GPU": float64(1), "ChargeType": "Dynamic"},
				},
			},
		},
	}

	// LLM correctly asks for clarification instead of calling workflow
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您有多台实例，请问要为哪台设置定时关机？\n1. train-a (uhost-1)\n2. train-b (uhost-2)"},
	}}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我设个定时关机", func(e StepEvent) {})
	assert.NoError(t, err)
	assert.Contains(t, reply, "哪")
	assert.NotContains(t, exec.calls, "UpdateCompShareStopScheduler")
}
