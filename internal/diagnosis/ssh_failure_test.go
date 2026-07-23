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

func TestSSHChain_InstallingUsesCurrentUpstreamStateAndKeepsLegacyCompatibility(t *testing.T) {
	for _, state := range []string{"Initializing", "Install"} {
		t.Run(state, func(t *testing.T) {
			executor := &mockExecutor{results: map[string]map[string]any{
				"DescribeCompShareInstance": {
					"UHostSet": []any{
						map[string]any{"UHostId": "uhost-abc", "State": state},
					},
				},
			}}
			onStep, _ := collectEvents()

			result, err := NewEngine(executor, onStep).Run(
				context.Background(), SSHFailureChain(), map[string]any{"UHostId": "uhost-abc"},
			)

			assert.NoError(t, err)
			assert.True(t, result.Success)
			assert.Contains(t, result.Conclusion, "初始化")
			assert.NotContains(t, result.Conclusion, "2-3")
			assert.Len(t, executor.calls, 1)
		})
	}
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
	assert.Contains(t, result.Suggestion, "技术支持")
	assert.NotContains(t, result.Suggestion, "建议删除重建")
	assert.Len(t, executor.calls, 1)
}

func TestSSHChain_ResponseMustContainRequestedInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-other", "State": "Running", "OsType": "Linux", "SshLoginCommand": "ssh ubuntu@1.2.3.5"},
			},
		},
	}}

	result, err := NewEngine(executor, nil).Run(
		context.Background(), SSHFailureChain(), map[string]any{"UHostId": "uhost-abc"},
	)

	assert.NoError(t, err)
	assert.Contains(t, result.Conclusion, "未找到")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, callActions(executor.calls))
}

func TestSSHChain_IncompleteLoginCommandIsNotAnAvailableEndpoint(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running", "OsType": "Linux",
					// Current upstream can construct this when IPSet has no public IP.
					"SshLoginCommand": "ssh ubuntu@", "IPSet": []any{},
				},
			},
		},
	}}

	result, err := NewEngine(executor, nil).Run(
		context.Background(), SSHFailureChain(), map[string]any{"UHostId": "uhost-abc"},
	)

	assert.NoError(t, err)
	assert.Contains(t, result.Conclusion, "登录入口不完整")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, callActions(executor.calls))
}

func TestSSHChain_PodEndpointRequiresSSHForwardWhenStructuredFieldIsPresent(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "cpod-abc", "State": "Running", "OsType": "Linux",
					"SshLoginCommand": "ssh -p 23120 root@cpod-abc.podtcp.compshare.cn",
					"TcpForwards": []any{
						map[string]any{"ExternalHost": "cpod-abc.podtcp.compshare.cn", "ExternalPort": float64(23120), "InternalPort": float64(8888)},
					},
				},
			},
		},
	}}

	result, err := NewEngine(executor, nil).Run(
		context.Background(), SSHFailureChain(), map[string]any{"UHostId": "cpod-abc"},
	)

	assert.NoError(t, err)
	assert.Contains(t, result.Conclusion, "登录入口不完整")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, callActions(executor.calls))
}

func TestValidatedSSHEndpoint_AcceptsCurrentUpstreamShapes(t *testing.T) {
	tests := []struct {
		name string
		host map[string]any
		want sshEndpoint
	}{
		{
			name: "uhost public ip",
			host: map[string]any{
				"UHostId": "uhost-abc", "SshLoginCommand": "ssh ubuntu@1.2.3.4",
				"IPSet": []any{
					map[string]any{"Type": "Private", "IP": "10.0.0.2"},
					map[string]any{"Type": "BGP", "IP": "1.2.3.4"},
				},
			},
			want: sshEndpoint{host: "1.2.3.4", port: 22},
		},
		{
			name: "pod ssh forward",
			host: map[string]any{
				"UHostId": "cpod-abc", "SshLoginCommand": "ssh -p 23120 root@cpod-abc.podtcp.compshare.cn",
				"TcpForwards": []any{map[string]any{
					"ExternalHost": "cpod-abc.podtcp.compshare.cn", "ExternalPort": float64(23120), "InternalPort": float64(23),
				}},
			},
			want: sshEndpoint{host: "cpod-abc.podtcp.compshare.cn", port: 23120},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, ok := validatedSSHEndpoint(tt.host)
			assert.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
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

func TestSSHChain_NormalCloudChecksUseTheReportedFailureKind(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{kind: "timeout", want: "网络或端口路径"},
		{kind: "connection_refused", want: "SSH 服务是否监听"},
		{kind: "authentication_failed", want: "用户名、密码或密钥"},
		{kind: "connection_dropped", want: "ServerAliveInterval"},
		{kind: "unknown", want: "ssh -vvv"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			executor := healthySSHExecutor("uhost-abc")
			result, err := NewEngine(executor, nil).Run(context.Background(), SSHFailureChain(), map[string]any{
				"UHostId": "uhost-abc", "FailureKind": tt.kind,
			})

			assert.NoError(t, err)
			assert.Contains(t, result.Suggestion, tt.want)
			assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
		})
	}
}

func TestSSHChain_MonitorMustMatchRequestedInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-abc", "State": "Running", "OsType": "Linux",
				"SshLoginCommand": "ssh ubuntu@1.2.3.4",
			}},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-other", "Metrics": []any{
					metric("uhost_cpu_used", 99), metric("cloudwatch_memory_usage", 99),
				}},
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{
					metric("uhost_cpu_used", 15), metric("cloudwatch_memory_usage", 20),
				}},
			}},
		},
	}}

	result, err := NewEngine(executor, nil).Run(
		context.Background(), SSHFailureChain(), map[string]any{"UHostId": "uhost-abc"},
	)

	assert.NoError(t, err)
	assert.Contains(t, result.Conclusion, "未发现明确")
	assert.NotContains(t, result.Conclusion, "资源耗尽")
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
	assert.Contains(t, result.Conclusion, "风险信号")
	assert.Contains(t, result.Conclusion, "不能确认")
	assert.NotContains(t, result.Conclusion, "资源耗尽")
	assert.Contains(t, result.Suggestion, "top")
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, callActions(executor.calls))
}

// TestSSHChain_Running_HighCPU_PodMonitor: a cpod-* instance's monitor lands in
// Data.PodList (flat Cpu/Memory series), NOT Data.List. Before the pod-leg fix the
// resource check read only List, so every pod was "无法确认"; now it reads PodList and
// catches exhaustion. The Cpu points are deliberately out of chronological order to
// also prove the latest value is chosen by max Timestamp, not array position (a
// naive last-element read would pick the older 12.0 and wrongly report "normal").
func TestSSHChain_Running_HighCPU_PodMonitor(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "cpod-abc", "State": "Running", "OsType": "LINUX",
					"SshLoginCommand": "ssh -p 23120 root@host.podtcp.compshare.cn",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{}, // pod instance → the ucloud leg is empty
				"PodList": []any{
					map[string]any{
						"UHostId": "cpod-abc",
						"Metrics": map[string]any{
							"Cpu": []any{
								map[string]any{"Timestamp": float64(100), "Value": float64(12.0)}, // older
								map[string]any{"Timestamp": float64(200), "Value": float64(97.0)}, // newest → chosen
							},
							"Memory": []any{
								map[string]any{"Timestamp": float64(200), "Value": float64(50.0)},
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
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "cpod-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "风险信号") // high usage detected from the PodList leg
	assert.Contains(t, result.Conclusion, "97")   // the max-Timestamp CPU value, not the older 12
}

// TestSSHChain_Running_Normal_PodMonitor: a healthy pod must read its PodList data
// and reach the healthy fallback — NOT the "监控未返回数据无法确认" branch that the
// List-only code always hit for pods.
func TestSSHChain_Running_Normal_PodMonitor(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "cpod-ok", "State": "Running", "OsType": "LINUX",
					"SshLoginCommand": "ssh -p 23120 root@host.podtcp.compshare.cn",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{},
				"PodList": []any{
					map[string]any{
						"UHostId": "cpod-ok",
						"Metrics": map[string]any{
							"Cpu":    []any{map[string]any{"Timestamp": float64(200), "Value": float64(15.0)}},
							"Memory": []any{map[string]any{"Timestamp": float64(200), "Value": float64(40.0)}},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "cpod-ok"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	// Read the data → healthy fallback, NOT the monitor-missing "无法确认".
	assert.NotContains(t, result.Conclusion, "无法确认")
	assert.Contains(t, result.Conclusion, "未见高压")
}

// TestSSHChain_Running_DiskFull_Ucloud: a full system disk (sys_disk_used_per) blocks
// login even with CPU/memory idle. The metric rides along in the same monitor call;
// the chain now reads it and reports 磁盘写满, checked before the CPU/mem branch.
func TestSSHChain_Running_DiskFull_Ucloud(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "State": "Running", "OsType": "LINUX",
					"SshLoginCommand": "ssh -p 22 ubuntu@1.2.3.4",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{"UHostId": "uhost-abc", "Metrics": []any{
					map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Timestamp": float64(200), "Value": float64(10.0)}}},
					}},
					map[string]any{"MetricKey": "cloudwatch_memory_usage", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Timestamp": float64(200), "Value": float64(20.0)}}},
					}},
					map[string]any{"MetricKey": "cloudwatch_sys_disk_used_per", "Results": []any{
						map[string]any{"Values": []any{map[string]any{"Timestamp": float64(200), "Value": float64(98.0)}}},
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
	assert.Contains(t, result.Conclusion, "系统盘")
	assert.Contains(t, result.Conclusion, "98")
	assert.Contains(t, result.Conclusion, "风险信号")
	assert.Contains(t, result.Conclusion, "不能确认")
	assert.NotContains(t, result.Conclusion, "写满")
}

// TestSSHChain_Running_DiskFull_PodMonitor: same, but the disk metric is the flat pod
// series Metrics.SysDiskUsed under Data.PodList.
func TestSSHChain_Running_DiskFull_PodMonitor(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "cpod-full", "State": "Running", "OsType": "LINUX",
					"SshLoginCommand": "ssh -p 23120 root@host.podtcp.compshare.cn",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{},
				"PodList": []any{
					map[string]any{
						"UHostId": "cpod-full",
						"Metrics": map[string]any{
							"Cpu":         []any{map[string]any{"Timestamp": float64(200), "Value": float64(5.0)}},
							"Memory":      []any{map[string]any{"Timestamp": float64(200), "Value": float64(30.0)}},
							"SysDiskUsed": []any{map[string]any{"Timestamp": float64(200), "Value": float64(99.0)}},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := SSHFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "cpod-full"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "系统盘")
	assert.Contains(t, result.Conclusion, "99")
	assert.Contains(t, result.Conclusion, "风险信号")
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

func healthySSHExecutor(id string) *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": id, "State": "Running", "OsType": "Linux",
				"SshLoginCommand": "ssh ubuntu@1.2.3.4",
			}},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{map[string]any{
				"UHostId": id,
				"Metrics": []any{
					metric("uhost_cpu_used", 15), metric("cloudwatch_memory_usage", 20),
				},
			}}},
		},
	}}
}

func metric(key string, value float64) map[string]any {
	return map[string]any{
		"MetricKey": key,
		"Results": []any{map[string]any{
			"Values": []any{map[string]any{"Timestamp": float64(200), "Value": value}},
		}},
	}
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
