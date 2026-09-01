package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCPUOnlyStartFailureReadsBackTheObservedResult(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		readback      map[string]any
		readbackError error
		contains      []string
	}{
		{
			name:     "target spec observed and instance is running",
			mode:     "cpu_only_2c4g",
			readback: cpuOnlyStartInstance("Running", 0, "", 2, 4096),
			contains: []string{"当前为 2核/4GB", "且已运行", "请勿重复提交"},
		},
		{
			name:     "target spec observed while instance remains stopped",
			mode:     "cpu_only_8c16g",
			readback: cpuOnlyStartInstance("Stopped", 0, "", 8, 16384),
			contains: []string{"当前为 8核/16GB", "截至本次回读仍处于已关机", "开机结果尚未确认"},
		},
		{
			name:     "observed spec differs",
			mode:     "cpu_only_2c4g",
			readback: cpuOnlyStartInstance("Stopped", 1, "H20", 16, 245760),
			contains: []string{"当前状态为已关机", "实际规格为H20 × 1 / 16核 / 240GB", "不能确认无卡改配已完整生效"},
		},
		{
			name:          "readback unavailable",
			mode:          "cpu_only_2c4g",
			readbackError: errors.New("readback unavailable"),
			contains:      []string{"随后未能读取实例当前状态", "结果不确定", "请勿重复提交"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			describeCalls := 0
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeCompShareInstance":
					describeCalls++
					if describeCalls == 1 {
						return cpuOnlyStartInstance("Stopped", 1, "H20", 16, 245760), nil
					}
					if tt.readbackError != nil {
						return nil, tt.readbackError
					}
					return tt.readback, nil
				case "StartCompShareInstance":
					return nil, tools.NewUpstreamAPIError(120, "synthetic upstream failure")
				default:
					return map[string]any{"RetCode": 0}, nil
				}
			}}
			eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
			onStep, events := collectSteps()

			reply := eng.executeResolvedWorkflow(context.Background(), mustConfirmable("StartInstanceWorkflow", map[string]any{
				"UHostId": "uhost-1", "StartMode": tt.mode,
			}, zoneRefData(nil)), onStep)

			for _, want := range tt.contains {
				assert.Contains(t, reply, want)
			}
			assert.Equal(t, []string{"DescribeCompShareInstance", "StartCompShareInstance", "DescribeCompShareInstance"}, executor.calls)
			var workflowFailure *StepEvent
			for i := range *events {
				if (*events)[i].Action == "StartInstanceWorkflow" && (*events)[i].Type == StepBlocked {
					workflowFailure = &(*events)[i]
				}
			}
			require.NotNil(t, workflowFailure)
			assert.Equal(t, "UPSTREAM_RETCODE_120", workflowFailure.ErrorCode)
		})
	}
}

func TestOrdinaryStartFailureAlsoReadsBackTheObservedResult(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return cpuOnlyStartInstance("Stopped", 1, "H20", 16, 245760), nil
		}
		return nil, tools.NewUpstreamAPIError(120, "synthetic upstream failure")
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	onStep, events := collectSteps()

	reply := eng.executeResolvedWorkflow(context.Background(), mustConfirmable("StartInstanceWorkflow", map[string]any{
		"UHostId": "uhost-1", "StartMode": "normal",
	}, zoneRefData(nil)), onStep)

	assert.Contains(t, reply, "截至本次回读仍处于已关机")
	assert.Contains(t, reply, "未观察到启动完成")
	assert.Contains(t, reply, "请勿重复提交")
	assert.Equal(t, []string{"DescribeCompShareInstance", "StartCompShareInstance", "DescribeCompShareInstance"}, executor.calls)
	var failure *StepEvent
	for i := range *events {
		if (*events)[i].Action == "StartInstanceWorkflow" && (*events)[i].Type == StepBlocked {
			failure = &(*events)[i]
		}
	}
	require.NotNil(t, failure)
	assert.Equal(t, "UPSTREAM_RETCODE_120", failure.ErrorCode)
}

func TestOrdinaryStartFailureReportsACommittedGPUReactivation(t *testing.T) {
	describeCalls := 0
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			describeCalls++
			if describeCalls == 1 {
				return cpuOnlyStartInstance("Stopped", 0, "4090", 2, 4096), nil
			}
			return cpuOnlyStartInstance("Stopped", 1, "4090", 16, 65536), nil
		case "StartCompShareInstance":
			return nil, tools.NewUpstreamAPIError(120, "synthetic upstream failure")
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })

	reply := eng.executeResolvedWorkflow(context.Background(), mustConfirmable("StartInstanceWorkflow", map[string]any{
		"UHostId": "uhost-1", "StartMode": "normal",
	}, zoneRefData(nil)), noopStep)

	assert.Contains(t, reply, "GPU 规格恢复已发生")
	assert.Contains(t, reply, "启动尚未完成")
	assert.Contains(t, reply, "请勿重复提交")
	assert.Equal(t, []string{"DescribeCompShareInstance", "StartCompShareInstance", "DescribeCompShareInstance"}, executor.calls)
}

func cpuOnlyStartInstance(state string, gpu int, gpuType string, cpu, memory int) map[string]any {
	return map[string]any{"UHostSet": []any{map[string]any{
		"UHostId": "uhost-1", "State": state, "Zone": "cn-wlcb-01", "Region": "cn-wlcb",
		"GPU": gpu, "GpuType": gpuType, "CPU": cpu, "Memory": memory,
		"ChargeType": "Postpay", "SupportWithoutGpuStart": true,
	}}}
}
