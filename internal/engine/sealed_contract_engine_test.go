package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	reply := eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow",
		map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(context.Background())))

	assert.Equal(t, "4090", cardGpu, "the confirm card must show the resolved GPU, not the one named in lastUserMsg")
	assert.NotContains(t, reply, "5090", "lastUserMsg must not reach the confirmed/executed contract")
}

// TestExecuteWorkflow_FailureNarrationUsesSealedNotPreEditParams pins P4
// acceptance #9: after a confirm-form edit changes the GPU, a sold-out is narrated
// from the edited (sealed) params, not the stale pre-edit args. The stock-shortage
// reply lists the OTHER available GPUs — it excludes the requested one — so an edit
// to A100 that is then sold out must exclude A100 (proving the edited value drove
// the reply); if the stale 4090 drove it, A100 would appear as an alternative.
//
// The sold-out is injected at the CAPACITY GATE, not the create call. That is where
// a sold-out actually offers alternatives: a create-step sold-out comes back as
// upstream RetCode 226604, which friendlyMessageFromText turns into a generic hint
// and returns BEFORE the alternatives branch — so it never lists any. The previous
// version of this test injected the failure at the create step with a bare error
// string (no RetCode), which no production executor produces; it reached the
// alternatives branch only because of that fiction. Routing through the capacity
// gate tests the path a real sold-out takes.
//
// The edit to A100 is sold out, so revalidation fails on the edited spec and the
// second card is never shown — confirmCalls stays 1. That the reply's alternatives
// are computed from A100 (not 4090) is the whole proof: the record the reply reads
// carries the re-resolved A100 draft, which only exists because the edit was
// applied before the failure.
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

	reply := eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow",
		map[string]any{"GpuType": "4090", "ImageName": "PyTorch", "Zone": "cn-wlcb-01"}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(context.Background())))

	assert.Equal(t, 1, confirmCalls, "the edit to a sold-out GPU fails on revalidation, before any second card")
	assert.NotEqual(t, "A100", exec.createArgs["GpuType"],
		"a sold-out edit must never reach the create call at all")
	// Sealed (edited) value is A100, so A100 must NOT be offered back as an
	// alternative; if the stale 4090 had driven the reply, A100 would appear.
	assert.Contains(t, reply, "库存不足")
	require.Contains(t, reply, "当前可创建的其他机型", "the stock-shortage reply must list alternatives")
	idx := strings.Index(reply, "当前可创建的其他机型")
	assert.NotContains(t, reply[idx:], "A100",
		"alternatives must be computed from the edited (sealed) GPU, which excludes A100")
}
