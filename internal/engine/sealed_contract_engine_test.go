package engine

import (
	"context"
	"fmt"
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
	createErr  string
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
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": m.available}, nil
	case "CheckCompShareResourceCapacity":
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}}, nil
	case "GetCompShareInstanceUserPrice":
		return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}}, nil
	case "CreateCompShareInstance":
		m.createArgs = args
		if m.createErr != "" {
			return nil, fmt.Errorf("%s", m.createErr)
		}
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
		map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, noopStep)

	assert.Equal(t, "4090", cardGpu, "the confirm card must show the resolved GPU, not the one named in lastUserMsg")
	assert.NotContains(t, reply, "5090", "lastUserMsg must not reach the confirmed/executed contract")
}

// TestExecuteWorkflow_FailureNarrationUsesSealedNotPreEditParams pins P4
// acceptance #9: after a confirm-form edit changes the GPU, a create failure is
// narrated from the sealed (edited) params, not the stale pre-edit args. The
// stock-shortage reply lists the OTHER available GPUs — it excludes the
// requested one — so with the edit to A100 the alternatives must exclude A100
// (proving the sealed value drove the reply); if the stale 4090 were used, A100
// would appear instead.
func TestExecuteWorkflow_FailureNarrationUsesSealedNotPreEditParams(t *testing.T) {
	exec := &createFlowExecutor{
		images:    []any{map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch", "ImageType": "App"}},
		available: []any{availableGPU("4090", 24), availableGPU("A100", 80), availableGPU("H20", 96)},
		createErr: "A100 1 卡当前库存不足（售罄），请换一个规格或稍后再试。",
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
		map[string]any{"GpuType": "4090", "ImageName": "PyTorch", "Zone": "cn-wlcb-01"}, noopStep)

	require.GreaterOrEqual(t, confirmCalls, 2, "the GPU edit must force a re-confirm before the create")
	assert.Equal(t, "A100", exec.createArgs["GpuType"], "the write must execute the edited (sealed) GPU")
	// The failure reply's alternatives exclude the requested GPU. Sealed value is
	// A100, so A100 must NOT be offered as an alternative; the pre-edit 4090 would.
	assert.Contains(t, reply, "库存不足")
	require.Contains(t, reply, "当前可创建的其他机型", "the stock-shortage reply must list alternatives")
	idx := strings.Index(reply, "当前可创建的其他机型")
	assert.NotContains(t, reply[idx:], "A100",
		"alternatives must be computed from the sealed (edited) GPU, which excludes A100")
}
