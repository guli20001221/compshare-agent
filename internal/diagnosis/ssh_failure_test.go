package diagnosis

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSSHChain_Stopped(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Stopped"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "关机")
	assert.Contains(t, result.Suggestion, "开机")
	assertDiagnosisSuggestionDoesNotPresentMutatingCommands(t, result.Suggestion)
	assert.Equal(t, "检查实例状态", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestSSHChain_Installing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Install"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化")
	assert.Len(t, executor.calls, 1)
}

func TestSSHChain_InstallFail(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Install Fail"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化失败")
	assert.Contains(t, result.Suggestion, "删除重建")
	assert.Len(t, executor.calls, 1)
}

func TestSSHChain_RunningWithLoginCommand_AllNormalFallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running", "OsType": "Linux",
					"SshLoginCommand": "ssh -p 23 root@1.2.3.4",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{
					map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Value": float64(30)}}},
					}},
					map[string]any{"MetricKey": "cloudwatch_memory_usage", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Value": float64(40)}}},
					}},
				}},
			}},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未发现明确")
	assert.Contains(t, result.Suggestion, "systemctl status ssh --no-pager")
	assert.Contains(t, result.Suggestion, "ss -lntp")
	assertDiagnosisSuggestionDoesNotPresentMutatingCommands(t, result.Suggestion)
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, callActions(executor.calls))
}

func TestSSHChain_RunningMissingLoginCommand_ConcludesBeforeMonitor(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running", "OsType": "Linux"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未返回 SSH 登录命令")
	assert.Contains(t, result.Suggestion, "控制台")
	assert.Contains(t, result.Suggestion, "systemctl status ssh --no-pager")
	assert.Equal(t, "检查实例状态", result.StoppedAt)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, callActions(executor.calls))
}

func TestSSHChain_WindowsUsesRDP(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-win", "State": "Running", "OsType": "Windows"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-win"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "Windows")
	assert.Contains(t, result.Suggestion, "RDP")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, callActions(executor.calls))
}

func TestSSHChain_Running_HighCPU(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running", "OsType": "Linux",
					"SshLoginCommand": "ssh -p 23 root@1.2.3.4",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "uhost_cpu_used",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(98.5)},
										},
									},
								},
							},
							map[string]any{
								"MetricKey": "cloudwatch_memory_usage",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(45.0)},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "资源")
	assert.Contains(t, result.Suggestion, "重启")
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, callActions(executor.calls))
}

func TestSSHChain_Running_MonitorMissingIsInconclusive(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running", "OsType": "Linux",
					"SshLoginCommand": "ssh -p 23 root@1.2.3.4",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{}},
			}},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "监控未返回 CPU/内存数据")
	assert.NotContains(t, result.Conclusion, "资源使用正常")
	assert.Contains(t, result.Suggestion, "free -h")
	assertDiagnosisSuggestionDoesNotPresentMutatingCommands(t, result.Suggestion)
	assert.Equal(t, "检查资源使用", result.StoppedAt)
}

func TestSSHChain_InstanceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
}

func assertDiagnosisSuggestionDoesNotPresentMutatingCommands(t *testing.T, suggestion string) {
	t.Helper()
	lower := strings.ToLower(suggestion)
	for _, forbidden := range []string{
		"startinstanceworkflow",
		"stopinstanceworkflow",
		"resetpasswordworkflow",
		"sudo apt",
		"apt install",
		"systemctl restart",
		"systemctl enable",
		"/start.d/",
		"tee /",
		" > /",
		"rm -",
		"mkfs",
	} {
		assert.NotContains(t, lower, forbidden)
	}
}

func assertReadOnlyDiagnosisSuggestion(t *testing.T, suggestion string) {
	t.Helper()
	assertDiagnosisSuggestionDoesNotPresentMutatingCommands(t, suggestion)
}

func callActions(calls []executorCall) []string {
	actions := make([]string, len(calls))
	for i, call := range calls {
		actions[i] = call.action
	}
	return actions
}
