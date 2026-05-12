package diagnosis

import (
	"context"
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
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
	assert.Equal(t, "检查实例状态", result.StoppedAt)
	assert.Len(t, executor.calls, 1) // only DescribeCompShareInstance called
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

func TestSSHChain_Running_InstanceHasSSH(t *testing.T) {
	// Instance-level Softwares includes SSH → step 2 Continue (priority 1 hit)
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running",
					"Softwares": []any{
						map[string]any{"Name": "SSH", "URL": "ssh://root@1.2.3.4:22"},
						map[string]any{"Name": "JupyterLab", "URL": "http://1.2.3.4:8888"},
					},
				},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "SSH", "Port": float64(22)},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{
					map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Value": float64(30)}}},
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
	// All 3 steps checked, fallback triggered
	assert.Len(t, executor.calls, 3)
	assert.Len(t, result.Steps, 3)
	assert.Contains(t, result.Conclusion, "未发现")
}

func TestSSHChain_Running_NoSoftwares_CatalogHasSSH(t *testing.T) {
	// Instance has no Softwares field (e.g. VM), but platform catalog has SSH
	// → step 2 Continue via priority 2 (catalog reference), proceed to step 3
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "SSH", "Port": float64(22)},
				map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{
					map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Value": float64(20)}}},
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
	assert.Len(t, executor.calls, 3) // all 3 steps
	assert.Contains(t, result.Conclusion, "未发现")
}

func TestSSHChain_Running_SoftwaresWithoutSSH_CatalogHasSSH(t *testing.T) {
	// Instance has Softwares (JupyterLab) but NOT SSH, platform catalog has SSH
	// → Priority 1 miss, Priority 2 hit → Continue to step 3
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running",
					"Softwares": []any{
						map[string]any{"Name": "JupyterLab", "URL": "http://1.2.3.4:8888"},
					},
				},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "SSH", "Port": float64(22)},
				map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{
					map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Value": float64(25)}}},
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
	assert.Len(t, executor.calls, 3) // all 3 steps, not stopped at step 2
	assert.Contains(t, result.Conclusion, "未发现")
}

func TestSSHChain_Running_NeitherHasSSH(t *testing.T) {
	// Neither instance Softwares nor platform catalog has SSH → conclude
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
			},
		},
		"DescribeCompShareSoftwarePort": {
			"SoftwarePort": []any{
				map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
				// No SSH in catalog either
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未发现 SSH 服务")
	assert.Contains(t, result.Suggestion, "JupyterLab")
	assert.Equal(t, "检查 SSH 端口", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
}

func TestSSHChain_Running_HighCPU(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
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
	assert.Len(t, executor.calls, 3)
}

func TestSSHChain_Running_AllNormal_Fallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Running"},
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
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "uhost_cpu_used",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(35.0)},
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
	assert.Contains(t, result.Conclusion, "未发现")
	assert.Contains(t, result.Suggestion, "控制台")
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
	assert.NotContains(t, result.Conclusion, "SSH 端口已开放", "fallback should not claim SSH port is open")
	assert.Len(t, executor.calls, 3) // all 3 steps checked
	assert.Len(t, result.Steps, 3)
	for _, s := range result.Steps {
		assert.Equal(t, "checked", s.Status)
	}
}

func TestSSHChain_InstanceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{}, // empty
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

func assertReadOnlyDiagnosisSuggestion(t *testing.T, suggestion string) {
	t.Helper()
	for _, forbidden := range []string{
		"StartInstanceWorkflow",
		"StopInstanceWorkflow",
		"ResetPasswordWorkflow",
		"systemctl",
		"ldconfig",
		"/start.d/",
		"终端执行",
		"手动启动",
		"登录命令",
	} {
		assert.NotContains(t, suggestion, forbidden)
	}
}
