package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPortFirewall_NotRunning(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Stopped"},
		}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未运行")
	assert.Contains(t, result.Suggestion, "开机")
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
	assert.Len(t, executor.calls, 1)
}

func TestPortFirewall_InstanceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
}

// Priority 1: Instance-level Softwares match — confirms the instance has the service
func TestPortFirewall_InstanceHasService(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-abc",
				"State":   "Running",
				"Softwares": []any{
					map[string]any{"Name": "JupyterLab", "URL": "http://1.2.3.4:8888?token=abc"},
					map[string]any{"Name": "SSH", "URL": "http://1.2.3.4:22"},
				},
			},
		}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{
			map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
		}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-abc",
		"Service": "jupyter",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	// Should use instance-level data, not platform catalog
	assert.Contains(t, result.Conclusion, "该实例已配置")
	assert.Contains(t, result.Conclusion, "JupyterLab")
	assert.Contains(t, result.Conclusion, "1.2.3.4:8888")
}

// Priority 2: Instance has no Softwares, falls back to platform catalog (reference)
func TestPortFirewall_NoInstanceSoftwares_FallbackToCatalog(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-abc",
				"State":   "Running",
				// No Softwares field — e.g. plain system image
			},
		}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{
			map[string]any{"Software": "JupyterLab", "Port": float64(8888)},
			map[string]any{"Software": "SSH", "Port": float64(22)},
		}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-abc",
		"Service": "jupyter",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	// Should NOT say "instance has service", should reference platform catalog
	assert.Contains(t, result.Conclusion, "未发现")
	assert.Contains(t, result.Conclusion, "平台端口目录")
	assert.Contains(t, result.Conclusion, "8888")
	assert.Contains(t, result.Suggestion, "控制台")
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
}

func TestPortFirewall_ServiceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Running"},
		}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{
			map[string]any{"Software": "SSH", "Port": float64(22)},
		}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-abc",
		"Service": "redis",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
	assert.Contains(t, result.Conclusion, "redis")
	assert.Contains(t, result.Suggestion, "控制台")
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
}

func TestPortFirewall_NoService_Fallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Running"},
		}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{
			map[string]any{"Software": "SSH", "Port": float64(22)},
		}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-abc",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "平台端口映射正常")
	assert.Contains(t, result.Suggestion, "控制台")
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
}

func TestPortFirewall_ServiceAliasNormalization(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-abc",
				"State":   "Running",
				"Softwares": []any{
					map[string]any{"Name": "FileBrowser", "URL": "http://1.2.3.4:8080"},
				},
			},
		}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{
			map[string]any{"Software": "FileBrowser", "Port": float64(8080)},
		}},
	}}
	onStep, _ := collectEvents()

	chain := PortFirewallChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-abc",
		"Service": "文件管理",
	})

	assert.NoError(t, err)
	assert.Contains(t, result.Conclusion, "该实例已配置")
	assert.Contains(t, result.Conclusion, "FileBrowser")
}
