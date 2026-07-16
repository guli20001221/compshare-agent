package capability

import (
	"context"
	"maps"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeReadExecCall struct {
	action string
	args   map[string]any
}

// fakeReadExec is a single-result executor stub mirroring the legacy
// mockHandlerExecutor: it records every call and returns the same map for
// Describe, GetPrice and DescribeCompShareSupportZone. Support-zone parsing
// yields no zones from the pricing fixture, so no placement args are added —
// exactly the legacy behaviour the parity assertions below lock in.
type fakeReadExec struct {
	result map[string]any
	calls  []fakeReadExecCall
}

func (f *fakeReadExec) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, fakeReadExecCall{action: action, args: args})
	return f.result, nil
}

func (f *fakeReadExec) ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return f.Execute(ctx, action, args)
}

func runPricing(t *testing.T, exec ReadExecutor, req PricingRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(pricingReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec})
}

func pricingFixture(extra map[string]any) map[string]any {
	result := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "4090",
				"Zone": "cn-wlcb-01",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{
								"Cpu":    float64(16),
								"Memory": []any{float64(64)},
							},
						},
					},
				},
			},
		},
	}
	maps.Copy(result, extra)
	return result
}

// TestPricingRequestMissingFields locks the validation contract: an empty
// gpu_type is a structured missing field, not a Chinese substring.
func TestPricingRequestMissingFields(t *testing.T) {
	require.Equal(t, []platform.MissingField{{Name: "gpu_type", Reason: "required"}}, PricingRequest{}.MissingFields())
	require.Nil(t, PricingRequest{GPUType: "4090"}.MissingFields())
}

// TestPricingHandle_PassesMemoryAsMBToAPI is the typed-path parity twin of the
// legacy TestHandlePricingQuery_PassesMemoryAsMBToAPI: the GB→MB boundary
// conversion, the omitted Zone (RetCode=230 guard) and the 1-GPU default must
// survive the migration off intent.Slots.
func TestPricingHandle_PassesMemoryAsMBToAPI(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{"Postpay": float64(1.69)})}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090", Kind: platform.PriceKindAccount})

	assert.Equal(t, "GetCompShareInstanceUserPrice", result.ToolAction)
	assert.Equal(t, platform.ReadStatusHandled, result.Status)
	require.GreaterOrEqual(t, len(exec.calls), 2, "expected Describe + GetPrice")
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", exec.calls[0].action)

	var priceCall *fakeReadExecCall
	for i := range exec.calls {
		if exec.calls[i].action == "GetCompShareInstanceUserPrice" {
			priceCall = &exec.calls[i]
			break
		}
	}
	require.NotNil(t, priceCall, "GetCompShareInstanceUserPrice never invoked")
	assert.Equal(t, 64*1024, priceCall.args["Memory"], "Memory must be MB (64GB*1024), not GB")
	assert.Equal(t, 16, priceCall.args["CPU"])
	assert.Equal(t, 1, priceCall.args["GPU"])
	assert.Equal(t, "4090", priceCall.args["GpuType"])
	_, hasZone := priceCall.args["Zone"]
	assert.False(t, hasZone, "Zone must be omitted from the price call (RetCode=230 guard)")
}

// TestPricingHandle_UserPriceReply mirrors TestHandlePricingQuery_UserPriceUses
// UserPriceTool: the account-price PriceDetails shape renders the discounted
// price under the account-price header.
func TestPricingHandle_UserPriceReply(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(map[string]any{
		"PriceDetails":         []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(1.58)}},
		"ListPriceDetails":     []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(2.10)}},
		"OriginalPriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": float64(1.98)}},
	})}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090", Kind: platform.PriceKindAccount})

	assert.Equal(t, "GetCompShareInstanceUserPrice", result.ToolAction)
	assert.Contains(t, result.Reply, "当前账号价格")
	assert.Contains(t, result.Reply, "¥1.58")
}

// TestPricingHandle_UnmatchedGPUClarifies proves the clarify branch: a named but
// unavailable GPU yields a structured clarification listing the real models,
// not a fabricated price.
func TestPricingHandle_UnmatchedGPUClarifies(t *testing.T) {
	exec := &fakeReadExec{result: pricingFixture(nil)}

	result := runPricing(t, exec, PricingRequest{GPUType: "H999"})

	assert.True(t, result.NeedsClarification)
	assert.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "请告诉我您想查的 GPU 型号")
	assert.Contains(t, result.Reply, "4090")
}

// TestPricingHandle_NoInventory covers the stage-1-empty terminal.
func TestPricingHandle_NoInventory(t *testing.T) {
	exec := &fakeReadExec{result: map[string]any{"AvailableInstanceTypes": []any{}}}

	result := runPricing(t, exec, PricingRequest{GPUType: "4090"})

	assert.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, noInstanceTypesReply, result.Reply)
}
