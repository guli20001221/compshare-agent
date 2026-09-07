package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
)

// createFlowExecutor is an arg-capturing executor for the create workflow: it
// serves the read steps (images / available types / capacity / price) and
// captures the CreateCompShareInstance args, optionally failing that call so the
// post-Run failure narration can be asserted.
type createFlowExecutor struct {
	calls      []string
	createArgs map[string]any
	// soldOutGPU makes the capacity gate report this GPU sold out, the way a real
	// 库存不足 arises: a SUCCESSFUL CheckCompShareResourceCapacity whose body says
	// the spec is unavailable. It keys on the GPU in the capacity ARGS, so an edit
	// that changes the sealed GPU changes which spec is sold out — which is what
	// lets a test prove the failure was narrated from the edited params.
	soldOutGPU string
	available  []any
	images     []any
}

func (m *createFlowExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"TotalCount": float64(0), "UHostSet": []any{}}, nil
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": m.images}, nil
	case "DescribeCompShareSupportZone":
		// The create resolves to defaultZone (cn-wlcb-01); the authoritative zone
		// catalog must carry it, or the run refuses (gate #2/#5).
		return map[string]any{"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A"},
		}}, nil
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": m.available}, nil
	case "CheckCompShareResourceCapacity":
		// The spec matches the availableGPU fixture (1 / 16C / 64GB) so the gate's
		// exact GPU/CPU/Mem match succeeds and it reaches the ResourceEnough branch
		// — a mismatched spec would take the "spec not found" path instead, which is
		// a different failure with no reason.
		gpu, _ := args["GpuType"].(string)
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64),
				"ResourceEnough": m.soldOutGPU == "" || gpu != m.soldOutGPU},
		}}, nil
	case "GetCompShareInstanceUserPrice":
		return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}}, nil
	case "CreateCompShareInstance":
		m.createArgs = args
		return map[string]any{"UHostIds": []any{"uhost-new1"}}, nil
	}
	return map[string]any{"RetCode": float64(0)}, nil
}

func availableGPU(name string, vramGB int) any {
	return map[string]any{
		"Name": name, "Zone": "cn-wlcb-01", "Status": "Normal",
		"GraphicsMemory": map[string]any{"Value": float64(vramGB)},
		"MachineSizes": []any{
			map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}},
		},
	}
}

// TestExecuteWorkflow_SealedParamsIgnoreContradictoryLastUserMsg pins P4
// acceptance #7: after resolution, the confirmed (and executed) create params
// come from the sealed contract, never a second reading of the chat text. A
// lastUserMsg that names a different GPU must not change what the user confirms.
func TestExecuteWorkflow_SealedParamsIgnoreContradictoryLastUserMsg(t *testing.T) {
	exec := &createFlowExecutor{
		images:    []any{map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch", "ImageType": "App"}},
		available: []any{availableGPU("4090", 24)},
	}
	var cardGpu any
	confirmFn := func(_ string, args map[string]any) bool {
		cardGpu = args["GpuType"]
		return false // decline — we only assert the confirmed card
	}
	eng := NewWithDeps(&mockLLM{}, exec, confirmFn)
	eng.lastUserMsg = "算了我其实要一台 5090，不要 4090" // contradictory; must not leak into the contract

	reply := eng.executeResolvedWorkflow(context.Background(),
		mustConfirmable("CreateInstanceWorkflow", map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, zoneRefData(eng.zoneCatalogSnapshot(context.Background()))), noopStep)

	assert.Equal(t, "4090", cardGpu, "the confirm card must show the resolved GPU, not the one named in lastUserMsg")
	assert.NotContains(t, reply, "5090", "lastUserMsg must not reach the confirmed/executed contract")
}

// An edit to A100 is revalidated against live inventory. If unavailable, return
// the edited spec's failure to the agent without creating or selecting substitutes.
func TestExecuteWorkflow_FailureNarrationUsesSealedNotPreEditParams(t *testing.T) {
	exec := &createFlowExecutor{
		images:     []any{map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch", "ImageType": "App"}},
		available:  []any{availableGPU("4090", 24), availableGPU("A100", 80), availableGPU("H20", 96)},
		soldOutGPU: "A100", // the EDITED GPU is the one with no stock
	}
	confirmCalls := 0
	editsFn := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		confirmCalls++
		if confirmCalls == 1 {
			return workflow.ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": "A100"}}
		}
		return workflow.ConfirmResolution{Confirmed: true}
	}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.confirmEditsFn = editsFn

	reply := eng.executeResolvedWorkflow(context.Background(),
		mustConfirmable("CreateInstanceWorkflow", map[string]any{"GpuType": "4090", "ImageName": "PyTorch", "Zone": "cn-wlcb-01"}, zoneRefData(eng.zoneCatalogSnapshot(context.Background()))), noopStep)

	assert.Equal(t, 1, confirmCalls, "the edit to a sold-out GPU fails on revalidation, before any second card")
	assert.NotEqual(t, "A100", exec.createArgs["GpuType"],
		"a sold-out edit must never reach the create call at all")
	assert.Contains(t, reply, "A100 1 卡 / 16C / 64GB 当前库存不足")
	assert.False(t, strings.HasPrefix(reply, finalReplyPrefix), "the Agent receives the failure on the edited draft")
	assert.NotContains(t, reply, "当前可创建的其他机型")
}
