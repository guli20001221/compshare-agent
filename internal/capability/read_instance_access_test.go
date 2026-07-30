package capability

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type accessReadExec struct {
	results map[string]map[string]any
	calls   []fakeReadExecCall
}

func (e *accessReadExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, fakeReadExecCall{action: action, args: args})
	return e.results[action], nil
}

func (e *accessReadExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return e.Execute(ctx, action, args)
}

func accessTarget(id string) []platform.TargetRef {
	return []platform.TargetRef{{
		Type: platform.TargetRefUHostIDUserInput, Value: id, Source: platform.SourceUserText,
	}}
}

func runInstanceAccess(t *testing.T, exec ReadExecutor, req InstanceAccessRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(instanceAccessReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{
		Executor: exec,
		Resolver: coldRegistrySnapshot(),
	})
}

func accessHost(id, kind string, extra map[string]any) map[string]any {
	host := map[string]any{
		"UHostId": id, "Name": "test-host", "State": "Running",
		"InstanceType": kind, "OsType": "Linux",
	}
	for key, value := range extra {
		host[key] = value
	}
	return host
}

func TestInstanceAccessRequestMissingFields(t *testing.T) {
	assert.Equal(t, []platform.MissingField{
		{Name: "targets", Reason: "required"},
		{Name: "access_type", Reason: "required"},
	}, InstanceAccessRequest{}.MissingFields())
	assert.Equal(t, []platform.MissingField{
		{Name: "protocol", Reason: "required"},
		{Name: "port", Reason: "required"},
	}, InstanceAccessRequest{
		Targets: accessTarget("cpod-a"), AccessType: accessTypeCustomPort,
	}.MissingFields())
	assert.Empty(t, InstanceAccessRequest{
		Targets: accessTarget("cpod-a"), AccessType: accessTypeCustomPort,
		Protocol: accessProtocolHTTP, Port: 8188,
	}.MissingFields())
}

func TestInstanceAccessSSHUsesCloudMetadataWithoutLeakingCommand(t *testing.T) {
	const command = "ssh -p 2222 root@203.0.113.9"
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("uhost-a", "UHost", map[string]any{
			"SshLoginCommand": command,
			"IPSet":           []any{map[string]any{"IP": "203.0.113.9", "Type": "International"}},
		})),
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{map[string]any{
				"UHostId": "uhost-a",
				"Metrics": []any{
					accessMonitorMetric("uhost_cpu_used", 15),
					accessMonitorMetric("cloudwatch_memory_usage", 20),
				},
			}}},
		},
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("uhost-a"), AccessType: accessTypeSSH,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "云侧配置已登记")
	assert.NotContains(t, result.Reply, command)
	assert.NotContains(t, result.Reply, "203.0.113.9")
	require.Len(t, exec.calls, 2)
	assert.Equal(t, instanceAccessDescribeAction, exec.calls[0].action)
	assert.Equal(t, "GetCompShareInstanceMonitor", exec.calls[1].action)
	require.NotNil(t, result.Envelope)
	assert.Equal(t, envelope.KindInstanceAccess, result.Envelope.Kind)
	assert.Equal(t, []string{instanceAccessDescribeAction, "GetCompShareInstanceMonitor"}, result.Envelope.SourceActions)
	assert.Equal(t, false, factValue(result.Envelope, "endpoint_reachability_checked"))
	assert.Equal(t, false, factValue(result.Envelope, "instance_process_checked"))
	assert.Equal(t, false, factValue(result.Envelope, "system_firewall_checked"))
}

func TestInstanceAccessJupyterUsesInstanceAndRealPortCatalog(t *testing.T) {
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("cpod-a", "Container", map[string]any{
			"Ports": map[string]any{
				"HttpPorts": []any{float64(8888)},
				"TcpPorts":  []any{float64(23)},
				"UdpPorts":  []any{},
			},
			"Softwares": []any{map[string]any{
				"Name": "JupyterLab", "URL": "https://secret-entry.invalid/?token=do-not-render",
			}},
		})),
		instanceAccessPortAction: {
			"SoftwarePort": []any{
				map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
				map[string]any{"Software": "ComfyUI", "Port": float64(8188)},
			},
		},
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("cpod-a"), AccessType: accessTypeJupyter,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "云侧配置已登记")
	assert.Contains(t, result.Reply, "HTTP 8888")
	assert.NotContains(t, result.Reply, "secret-entry")
	assert.NotContains(t, result.Reply, "token=")
	require.Len(t, exec.calls, 2)
	assert.Equal(t, instanceAccessDescribeAction, exec.calls[0].action)
	assert.Equal(t, instanceAccessPortAction, exec.calls[1].action)
	assert.Equal(t, []string{instanceAccessDescribeAction, instanceAccessPortAction}, result.Envelope.SourceActions)
}

func TestInstanceAccessJupyterMissingPodPortIsBlocked(t *testing.T) {
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("cpod-a", "Container", map[string]any{
			"Ports": map[string]any{"HttpPorts": []any{float64(8188)}},
		})),
		instanceAccessPortAction: {
			"SoftwarePort": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}},
		},
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("cpod-a"), AccessType: accessTypeJupyter,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "云侧存在明确阻断")
	assert.Contains(t, result.Reply, "没有登记")
	assert.Contains(t, result.Reply, "不能据此断言它们正常")
}

func TestInstanceAccessVMJupyterDoesNotTreatGlobalCatalogAsInstanceMapping(t *testing.T) {
	exec := &accessReadExec{results: map[string]map[string]any{
		// The upstream may report InstanceType=Container for a UHost created
		// from a container image. It is still not a Pod port-mapping target.
		instanceAccessDescribeAction: describeFixture(accessHost("uhost-a", "Container", map[string]any{
			"Softwares": []any{map[string]any{
				"Name": "JupyterLab", "URL": "https://entry.invalid/?token=not-a-connectivity-check",
			}},
		})),
		instanceAccessPortAction: {
			"SoftwarePort": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}},
		},
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("uhost-a"), AccessType: accessTypeJupyter,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "云侧信息不足")
	assert.Contains(t, result.Reply, "不能证明入口、平台代理或实例内服务当前可达")
	assert.NotContains(t, result.Reply, "8888")
	assert.NotContains(t, result.Reply, "entry.invalid")
	assert.NotContains(t, result.Reply, "token=")
	require.Len(t, exec.calls, 1)
	assert.Equal(t, instanceAccessDescribeAction, exec.calls[0].action)
	assert.Equal(t,
		"实例详情返回了 Jupyter 入口记录，但这不能证明入口、平台代理或实例内服务当前可达。",
		factValue(result.Envelope, "cloud_precheck_reason"))
	assert.Equal(t, false, factValue(result.Envelope, "endpoint_reachability_checked"))
}

func TestInstanceAccessCustomPortDistinguishesPodFromVM(t *testing.T) {
	t.Run("pod exact mapping", func(t *testing.T) {
		exec := &accessReadExec{results: map[string]map[string]any{
			instanceAccessDescribeAction: describeFixture(accessHost("cpod-a", "Container", map[string]any{
				"Ports": map[string]any{"TcpPorts": []any{float64(6006)}},
				"TcpForwards": []any{map[string]any{
					"InternalPort": float64(6006), "ExternalPort": float64(32001),
				}},
			})),
		}}

		result := runInstanceAccess(t, exec, InstanceAccessRequest{
			Targets: accessTarget("cpod-a"), AccessType: accessTypeCustomPort,
			Protocol: accessProtocolTCP, Port: 6006,
		})

		assert.Equal(t, platform.ReadStatusHandled, result.Status)
		assert.Contains(t, result.Reply, "云侧配置已登记")
		require.Len(t, exec.calls, 1, "custom-port checks should not depend on the global software catalog")
	})

	t.Run("vm stays unknown", func(t *testing.T) {
		exec := &accessReadExec{results: map[string]map[string]any{
			instanceAccessDescribeAction: describeFixture(accessHost("uhost-a", "UHost", nil)),
		}}

		result := runInstanceAccess(t, exec, InstanceAccessRequest{
			Targets: accessTarget("uhost-a"), AccessType: accessTypeCustomPort,
			Protocol: accessProtocolHTTP, Port: 8188,
		})

		assert.Equal(t, platform.ReadStatusHandled, result.Status)
		assert.Contains(t, result.Reply, "云侧信息不足")
		assert.Contains(t, result.Reply, "不返回实例内监听端口或系统防火墙状态")
		require.Len(t, exec.calls, 1, "VM custom-port checks should not make an unrelated catalog call")
	})
}

func TestInstanceAccessRequiresSameIDInDescribeResponse(t *testing.T) {
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("uhost-b", "UHost", map[string]any{
			"SshLoginCommand": "ssh root@example.invalid",
		})),
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("uhost-a"), AccessType: accessTypeSSH,
	})

	assert.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Contains(t, result.Reply, "没有找到指定实例")
	assert.Nil(t, result.Envelope)
}

func TestInstanceAccessJupyterDiagnosisDoesNotFetchTokenOrSoftwareURL(t *testing.T) {
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("uhost-a", "UHost", nil)),
		instanceAccessPortAction: {
			"SoftwarePort": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}},
		},
	}}

	_ = runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("uhost-a"), AccessType: accessTypeJupyter,
	})

	for _, call := range exec.calls {
		assert.NotEqual(t, "DescribeCompShareJupyterToken", call.action)
		assert.NotEqual(t, "GetSoftwareUrl", call.action)
		assert.NotEqual(t, "GetSoftwareURL", call.action)
	}
}

func TestInstanceAccessExplicitJupyterTokenUsesVerifiedInstance(t *testing.T) {
	const token = "stable-console-visible-token"
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("cpod-a", "Container", map[string]any{
			"State": "Stopped",
		})),
		instanceAccessTokenAction: {"JupyterToken": token},
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("cpod-a"), AccessType: accessTypeJupyterToken,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, instanceAccessTokenAction, result.ToolAction)
	assert.Contains(t, result.Reply, token)
	assert.Contains(t, result.Reply, "需启动后")
	assert.Equal(t, ReadPresentationRequired, result.Presentation)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, instanceAccessDescribeAction, exec.calls[0].action)
	assert.Equal(t, instanceAccessTokenAction, exec.calls[1].action)
	assert.Equal(t, []string{instanceAccessDescribeAction, instanceAccessTokenAction}, result.Envelope.SourceActions)
	assert.Equal(t, true, factValue(result.Envelope, "jupyter_token_present"))
	assert.Nil(t, factValue(result.Envelope, "jupyter_token"))
	for _, call := range exec.calls {
		assert.NotEqual(t, "GetSoftwareUrl", call.action)
		assert.NotEqual(t, "GetSoftwareURL", call.action)
	}
}

func TestInstanceAccessExplicitJupyterTokenDoesNotInventMissingValue(t *testing.T) {
	exec := &accessReadExec{results: map[string]map[string]any{
		instanceAccessDescribeAction: describeFixture(accessHost("uhost-a", "UHost", nil)),
		instanceAccessTokenAction:    {"JupyterToken": ""},
	}}

	result := runInstanceAccess(t, exec, InstanceAccessRequest{
		Targets: accessTarget("uhost-a"), AccessType: accessTypeJupyterToken,
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "没有返回有效")
	assert.NotContains(t, result.Reply, "Jupyter Token：")
}

func factValue(got *envelope.Envelope, key string) any {
	if got == nil {
		return nil
	}
	for _, fact := range got.Facts {
		if fact.Key == key {
			return fact.Value
		}
	}
	return nil
}

func accessMonitorMetric(key string, value float64) map[string]any {
	return map[string]any{
		"MetricKey": key,
		"Results": []any{map[string]any{
			"Values": []any{map[string]any{"Timestamp": float64(200), "Value": value}},
		}},
	}
}
