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

	_ = eng.executeResolvedWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(context.Background())))

	assert.Equal(t, []string{"4090"}, gpuOptions)
}

// DELETED: TestExecuteWorkflow_GuidedCreateCanonicalizesSpaced409048G.
//
// It drove executeWorkflow with a raw {"GpuType": "4090 48G"} and asserted the
// confirm card showed "4090_48G". That canonicalization used to happen inside
// executeWorkflow (engine.go, knowledge.CanonicalGPUType) — i.e. AFTER the
// resolver had accepted the value and on the same side of the wire as the seal.
// It now happens in the resolver, before ReadyForConfirmation, against the live
// machine-type catalog.
//
// So the test's INPUT is no longer producible: GpuType reaches
// executeResolvedWorkflow only via Request* -> Resolver, which means it is
// already canonical. The
// test would have been asserting a hand-built state production cannot reach —
// the same green-but-unreachable shape deleted in 77c9f5e9.
//
// The contract itself is NOT dropped. actionresolver's
// TestResolveCanonicalizesGpuTypeBeforeConfirmation drives the identical input
// ("4090 48G") to the identical expectation ("4090_48G") and additionally pins
// what this test could not: that the confirm card and the executed arguments
// carry the same string.

func TestExecuteWorkflow_GuidedCreateDoesNotOverrideResolvedGPUFromUserText(t *testing.T) {
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

	_ = eng.executeResolvedWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(context.Background())))

	assert.NotEmpty(t, seenGpuArgs)
	assert.Equal(t, "4090", seenGpuArgs[0])
	assert.NotContains(t, seenGpuArgs, "4090_48G")
}
