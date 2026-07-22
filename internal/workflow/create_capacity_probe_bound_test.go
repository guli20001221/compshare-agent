package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelCapacityExecutor answers CheckCompShareResourceCapacity per GPU MODEL and
// serves a catalog of arbitrary size, so a test can place a sold-out model past
// the batch bound. A single unconstrained image keeps every model in the fan-out
// (no SupportedGpuTypes narrowing), which is the broad-image case that makes the
// fan-out large in the first place.
type modelCapacityExecutor struct {
	*mockExecutor
	catalog map[string]any
	soldOut map[string]bool
	probed  map[string]bool
}

func newModelCapacityExecutor(models []string, soldOut map[string]bool) *modelCapacityExecutor {
	base := formMockExecutor()
	base.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-open", "Name": "Ubuntu 无约束", "Size": float64(102400)},
	}}
	rows := make([]any, 0, len(models))
	for _, m := range models {
		rows = append(rows, map[string]any{
			"Name":   m,
			"Zone":   "cn-wlcb-01",
			"Status": "Normal",
			"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}}},
			"CpuPlatforms": map[string]any{"Amd": map[string]any{}},
			"Disks":        []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
		})
	}
	return &modelCapacityExecutor{
		mockExecutor: base,
		catalog:      map[string]any{"AvailableInstanceTypes": rows},
		soldOut:      soldOut,
		probed:       map[string]bool{},
	}
}

func (e *modelCapacityExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeAvailableCompShareInstanceTypes":
		e.mockExecutor.calls = append(e.mockExecutor.calls, executorCall{action, args})
		return e.catalog, nil
	case "CheckCompShareResourceCapacity":
		e.mockExecutor.calls = append(e.mockExecutor.calls, executorCall{action, args})
		gpu, _ := args["GpuType"].(string)
		e.probed[gpu] = true
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": !e.soldOut[gpu]},
		}}, nil
	default:
		return e.mockExecutor.Execute(ctx, action, args)
	}
}

func runToGPUCardWith(t *testing.T, exec *modelCapacityExecutor) []ConfirmFormOption {
	t.Helper()
	var gpuOpts []ConfirmFormOption
	eng := NewEngine(exec, func(string, map[string]any) bool { return true }, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if form != nil {
			if f := form.Field("GpuType"); f != nil {
				gpuOpts = f.Options
				return ConfirmResolution{Confirmed: false}
			}
		}
		return ConfirmResolution{Confirmed: true}
	})
	_, err := eng.Run(context.Background(), CreateInstanceGuidedDef(), map[string]any{},
		func(c *Context) { c.referenceData.ZoneCatalog = createZoneCatalog() })
	require.NoError(t, err)
	return gpuOpts
}

// TestGPUCardGraysASoldOutModelPastTheCapacityProbeFanOut is the regression the
// combo probe introduced. The GPU-card gate widened the capacity fan-out from a
// single model's zones (≤4 calls) to every offered (model, zone) row — ~19 live —
// but the batch bound that decides how many of those calls are actually made was
// sized for the old, small shape. A catalog larger than the bound records the
// tail as "never asked", which both cards read as unknown = selectable. A sold-out
// model whose only probe fell in that tail is then offered as clickable — exactly
// the 没库存却能点 this whole gate exists to prevent.
//
// The bound must cover the real fan-out. This catalog offers 20 models — more than
// the ~19 the live catalog produces — with the sold-out one last, so the bound has
// to reach it. Mutation check: at the old bound (12) the last model's probe is
// never made and it stays selectable, failing this test.
func TestGPUCardGraysASoldOutModelPastTheCapacityProbeFanOut(t *testing.T) {
	const soldOutModel = "gpu19"
	models := make([]string, 0, 20)
	for i := range 20 {
		models = append(models, fmt.Sprintf("gpu%02d", i))
	}
	// gpu19 is last in catalog order, so its single (model, zone) probe is the very
	// last call the fan-out would make — the first to be dropped by too small a bound.
	require.Equal(t, soldOutModel, models[len(models)-1])

	exec := newModelCapacityExecutor(models, map[string]bool{soldOutModel: true})
	opts := runToGPUCardWith(t, exec)
	require.NotEmpty(t, opts, "the GPU card must be offered")

	byValue := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byValue[o.Value] = o
	}

	require.True(t, exec.probed[soldOutModel],
		"the sold-out model must actually be probed — an un-probed model is the bug, "+
			"not the gate: the bound dropped its capacity call")

	soldOut, ok := byValue[soldOutModel]
	require.True(t, ok, "the sold-out model must still be listed on the card")
	assert.True(t, soldOut.Disabled,
		"a model upstream says is not creatable in any zone must be unpickable, even when "+
			"its probe sits at the far end of a large fan-out")
	assert.NotEmpty(t, soldOut.Reason, "a disabled option must say why")

	// The escape hatch is intact: the creatable models the same large fan-out
	// covered stay selectable, so raising the bound did not gray anything wrongly.
	first := byValue["gpu00"]
	assert.False(t, first.Disabled, "a creatable model stays selectable")
}
