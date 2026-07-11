package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteWorkflow_GuidedCreateLocksExplicitGPU(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
			map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(94)}},
				}}}},
			map[string]any{"Name": "A800", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(32), "Memory": []any{float64(128)}},
				}}}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	var gpuOptions []string
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		if form.Step.Index == 1 {
			gpu := form.Field("GpuType")
			require.NotNil(t, gpu)
			for _, opt := range gpu.Options {
				gpuOptions = append(gpuOptions, opt.Value)
			}
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_ = eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, noopStep)

	assert.Equal(t, []string{"4090"}, gpuOptions)
}

func TestExecuteWorkflow_GuidedCreateCanonicalizesSpaced409048G(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
			map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(94)}},
				}}}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	var gpuOptions []string
	var seenGpuArgs []string
	eng.confirmEditsFn = func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		if gt, _ := args["GpuType"].(string); gt != "" {
			seenGpuArgs = append(seenGpuArgs, gt)
		}
		if gpu := form.Field("GpuType"); gpu != nil {
			for _, opt := range gpu.Options {
				gpuOptions = append(gpuOptions, opt.Value)
			}
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_ = eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090 48G"}, noopStep)

	assert.NotEmpty(t, seenGpuArgs)
	assert.Equal(t, "4090_48G", seenGpuArgs[0])
	assert.NotContains(t, seenGpuArgs, "4090 48G")
	if len(gpuOptions) > 0 {
		assert.Equal(t, []string{"4090_48G"}, gpuOptions)
	}
}

func TestExecuteWorkflow_GuidedCreateUsesExplicit409048GFromUserText(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
			map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(94)}},
				}}}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	eng.lastUserMsg = "开一台 4090 48G"
	var seenGpuArgs []string
	eng.confirmEditsFn = func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		if gt, _ := args["GpuType"].(string); gt != "" {
			seenGpuArgs = append(seenGpuArgs, gt)
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_ = eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, noopStep)

	assert.NotEmpty(t, seenGpuArgs)
	assert.Equal(t, "4090_48G", seenGpuArgs[0])
	assert.NotContains(t, seenGpuArgs, "4090")
}
