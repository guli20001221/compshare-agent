package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixtures
//
// WHY these tests exist: the editable confirm form must (1) only ever offer
// server-generated values (whitelist — user edits cannot inject create params),
// (2) never claim stock/price for combinations that were never checked, and
// (3) re-run the stock+price gates and RE-CONFIRM after every edit (方案 A).
// A test that passes while any of those invariants break is wrong.
// ---------------------------------------------------------------------------

// formCatalogFixture: 4090 sellable in two zones (24G VRAM), A800 in one,
// P40 present but sold out (must never be offered).
func formCatalogFixture() map[string]any {
	size := func(gpu, cpu, mem float64) []any {
		return []any{map[string]any{"Gpu": gpu, "Collection": []any{
			map[string]any{"Cpu": cpu, "Memory": []any{mem}},
		}}}
	}
	return map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
			"MachineSizes": size(1, 16, 64), "GraphicsMemory": map[string]any{"Value": float64(24)}},
		map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal",
			"MachineSizes": size(1, 16, 64)},
		map[string]any{"Name": "A800", "Zone": "cn-wlcb-01", "Status": "Normal",
			"MachineSizes": size(1, 32, 128), "GraphicsMemory": map[string]any{"Value": float64(80)}},
		map[string]any{"Name": "P40", "Zone": "cn-wlcb-01", "Status": "Soldout",
			"MachineSizes": size(1, 8, 32)},
	}}
}

// formImagesFixture: img-001 declares no GPU constraint, img-002 supports
// 4090+A800, img-003 is V100S-only (must be filtered for a 4090 create),
// img-004 is 4090-only.
func formImagesFixture() map[string]any {
	return map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12"},
		map[string]any{"CompShareImageId": "img-002", "Name": "PyTorch 2.4",
			"SupportedGpuTypes": []any{"4090", "A800"}},
		map[string]any{"CompShareImageId": "img-003", "Name": "V100 专用镜像",
			"SupportedGpuTypes": []any{"V100S"}},
		map[string]any{"CompShareImageId": "img-004", "Name": "ComfyUI",
			"SupportedGpuTypes": []any{"4090"}},
	}}
}

// formMockExecutor seeds all create-workflow calls with stock available for
// both the 4090 (16C/64G) and A800 (32C/128G) specs.
func formMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages":                 formImagesFixture(),
		"DescribeAvailableCompShareInstanceTypes": formCatalogFixture(),
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			map[string]any{"Gpu": float64(1), "Cpu": float64(32), "Mem": float64(128), "ResourceEnough": true},
		}},
		"GetCompShareInstanceUserPrice": {"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Price": 1.58},
		}},
		"CreateCompShareInstance": {"UHostIds": []any{"uhost-new001"}},
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-new001", "State": "Running"},
		}},
	}}
}

func formWfCtx(t *testing.T, params map[string]any) *Context {
	t.Helper()
	wfCtx := NewContext(params)
	wfCtx.StepResults["查询镜像"] = formImagesFixture()
	wfCtx.StepResults["查询可用配比"] = formCatalogFixture()
	wfCtx.StepResults["查询价格"] = map[string]any{"PriceDetails": []any{
		map[string]any{"ChargeType": "Postpay", "Price": 1.58},
	}}
	return wfCtx
}

func fieldByKey(t *testing.T, form *ConfirmForm, key string) *ConfirmFormField {
	t.Helper()
	f := form.Field(key)
	require.NotNil(t, f, "form must contain field %s", key)
	return f
}

func optionValues(f *ConfirmFormField) []string {
	out := make([]string, len(f.Options))
	for i, o := range f.Options {
		out[i] = o.Value
	}
	return out
}

// ---------------------------------------------------------------------------
// Form assembly
// ---------------------------------------------------------------------------

func TestBuildCreateConfirmForm_OptionsAreServerWhitelists(t *testing.T) {
	form, err := buildCreateConfirmForm(formWfCtx(t, map[string]any{"GpuType": "4090"}))
	require.NoError(t, err)
	require.NotNil(t, form)
	assert.Equal(t, 1, form.Version)

	// GPU: current first; sold-out P40 never offered (没货红线 — the form must
	// not present a card the catalog says is unsellable).
	gpu := fieldByKey(t, form, "GpuType")
	assert.Equal(t, "4090", gpu.Value)
	assert.True(t, gpu.Editable)
	assert.Equal(t, []string{"4090", "A800"}, optionValues(gpu))
	assert.Contains(t, gpu.Options[0].Label, "24G", "catalog VRAM should reach the option label")
	assert.NotContains(t, optionValues(gpu), "P40")

	// Zone: both zones carrying the current GPU, current first.
	zone := fieldByKey(t, form, "Zone")
	assert.Equal(t, "cn-wlcb-01", zone.Value)
	assert.Equal(t, []string{"cn-wlcb-01", "cn-sh2-02"}, optionValues(zone))

	// Image: current selection first, then GPU-compatible recommendations only
	// (the V100S-only image must be filtered), capped at 3.
	img := fieldByKey(t, form, "ImageId")
	assert.Equal(t, "img-001", img.Value)
	assert.Equal(t, []string{"img-001", "img-002", "img-004"}, optionValues(img))
	assert.NotContains(t, optionValues(img), "img-003")

	// ChargeType: always offered; values match the upstream billing contract.
	ct := fieldByKey(t, form, "ChargeType")
	assert.Equal(t, "Postpay", ct.Value)
	assert.Equal(t, []string{"Postpay", "Day", "Month"}, optionValues(ct))
}

func TestBuildCreateConfirmForm_OmitsSingleOptionFields(t *testing.T) {
	// A800 lives in one zone only → no real zone choice → field omitted (the
	// read-only Summary already shows the value; an uneditable select is noise).
	form, err := buildCreateConfirmForm(formWfCtx(t, map[string]any{"GpuType": "A800"}))
	require.NoError(t, err)
	assert.Nil(t, form.Field("Zone"))
	assert.NotNil(t, form.Field("GpuType"))
}

func TestBuildCreateConfirmForm_ImageConstraintFiltersGPUs(t *testing.T) {
	// With the 4090-only image selected, the GPU field must not offer A800 —
	// offering an incompatible GPU would let one click create a broken combo.
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "ImageName": "ComfyUI"})
	form, err := buildCreateConfirmForm(wfCtx)
	require.NoError(t, err)
	// 4090 is the only compatible+sellable type → single option → field omitted.
	assert.Nil(t, form.Field("GpuType"))
}

// ---------------------------------------------------------------------------
// Override application
// ---------------------------------------------------------------------------

func TestApplyCreateOverrides_GpuSwitchDropsStalePins(t *testing.T) {
	// A user-pinned CPU/Memory (and the auto-resolved zone) belong to the OLD
	// GPU. Keeping them would dead-end the revalidate on a spec mismatch.
	wfCtx := formWfCtx(t, map[string]any{
		"GpuType": "4090", "Cpu": float64(16), "Memory": float64(65536), "Zone": "cn-sh2-02",
	})
	require.NoError(t, applyCreateOverrides(wfCtx, map[string]string{"GpuType": "A800"}))
	assert.Equal(t, "A800", wfCtx.Params["GpuType"])
	assert.NotContains(t, wfCtx.Params, "Cpu")
	assert.NotContains(t, wfCtx.Params, "Memory")
	assert.NotContains(t, wfCtx.Params, "Zone")
}

func TestApplyCreateOverrides_SimultaneousZonePickHonored(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090"})
	require.NoError(t, applyCreateOverrides(wfCtx, map[string]string{
		"GpuType": "A800", "Zone": "cn-wlcb-01",
	}))
	assert.Equal(t, "cn-wlcb-01", wfCtx.Params["Zone"], "an explicit zone picked in the same edit must survive the GPU-switch cleanup")
}

func TestApplyCreateOverrides_ImageThreadsIDAndName(t *testing.T) {
	// Threading CompShareImageId is what makes capacity/price/create all act
	// on the EDITED image (pickImageId prefers it); ImageName keeps the
	// refreshed card display truthful.
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090"})
	require.NoError(t, applyCreateOverrides(wfCtx, map[string]string{"ImageId": "img-002"}))
	assert.Equal(t, "img-002", wfCtx.Params["CompShareImageId"])
	assert.Equal(t, "PyTorch 2.4", wfCtx.Params["ImageName"])
}

func TestApplyCreateOverrides_UnknownKeyRejected(t *testing.T) {
	// Defense in depth: even if a key slipped past form validation, the apply
	// layer must refuse anything outside the form's field set (no Disks/GPU
	// count injection into CreateCompShareInstance).
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090"})
	assert.Error(t, applyCreateOverrides(wfCtx, map[string]string{"Disks": "999"}))
}

func TestValidateOverrides_WhitelistOnly(t *testing.T) {
	form := &ConfirmForm{Version: 1, Fields: []ConfirmFormField{
		{Key: "GpuType", Type: "select", Value: "4090", Editable: true,
			Options: []ConfirmFormOption{{Value: "4090"}, {Value: "A800"}}},
		{Key: "Locked", Type: "select", Value: "x", Editable: false,
			Options: []ConfirmFormOption{{Value: "x"}, {Value: "y"}}},
	}}
	assert.NoError(t, form.ValidateOverrides(map[string]string{"GpuType": "A800"}))
	assert.Error(t, form.ValidateOverrides(map[string]string{"GpuType": "H100"}), "value outside offered options")
	assert.Error(t, form.ValidateOverrides(map[string]string{"Locked": "y"}), "non-editable field")
	assert.Error(t, form.ValidateOverrides(map[string]string{"Nope": "v"}), "unknown field")
}

// ---------------------------------------------------------------------------
// Edit → revalidate → re-confirm loop (方案 A)
// ---------------------------------------------------------------------------

func TestCreateInstance_FormEditRevalidatesAndReconfirms(t *testing.T) {
	executor := formMockExecutor()
	var rounds []map[string]any // confirm args per round
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(action string, args map[string]any, form *ConfirmForm) ConfirmResolution {
		rounds = append(rounds, args)
		if len(rounds) == 1 {
			// Round 1: user switches 4090 → A800 in the form.
			require.NotNil(t, form.Field("GpuType"))
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}}
		}
		// Round 2: refreshed card must already reflect the edited GPU with its
		// re-resolved spec — confirming a stale card would defeat 方案 A.
		assert.Equal(t, "A800", rounds[1]["GpuType"])
		assert.Equal(t, float64(32), rounds[1]["CPU"])
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, rounds, 2, "an edit must trigger a second confirm round")

	// Stock + price must have been re-checked for the EDITED combination
	// before anything was created, and the create must use the edited GPU.
	var capacityGPUs, createGPUs []string
	capacityCalls, priceCalls := 0, 0
	for _, c := range executor.calls {
		switch c.action {
		case "CheckCompShareResourceCapacity":
			capacityCalls++
			gt, _ := c.args["GpuType"].(string)
			capacityGPUs = append(capacityGPUs, gt)
		case "GetCompShareInstanceUserPrice":
			priceCalls++
		case "CreateCompShareInstance":
			gt, _ := c.args["GpuType"].(string)
			createGPUs = append(createGPUs, gt)
		}
	}
	assert.Equal(t, 2, capacityCalls, "capacity must re-run after the edit")
	assert.Equal(t, 2, priceCalls, "price must re-run after the edit")
	assert.Equal(t, []string{"4090", "A800"}, capacityGPUs)
	assert.Equal(t, []string{"A800"}, createGPUs, "create must use the edited, revalidated GPU")
}

func TestCreateInstance_FormDenyCancels(t *testing.T) {
	executor := formMockExecutor()
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(string, map[string]any, *ConfirmForm) ConfirmResolution {
		return ConfirmResolution{Confirmed: false}
	})
	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "用户取消了操作", result.Message)
	for _, c := range executor.calls {
		assert.NotEqual(t, "CreateCompShareInstance", c.action, "deny must never create")
	}
}

func TestCreateInstance_FormEditCapStopsLoop(t *testing.T) {
	executor := formMockExecutor()
	calls := 0
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		calls++
		// Flip between the two GPUs forever — a hostile/buggy client must not
		// be able to spin the workflow.
		next := "A800"
		if calls%2 == 0 {
			next = "4090"
		}
		return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": next}}
	})
	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "修改次数过多")
	assert.Equal(t, maxConfirmEdits+1, calls)
	for _, c := range executor.calls {
		assert.NotEqual(t, "CreateCompShareInstance", c.action)
	}
}

// funcExecutor lets a test vary results by call args (the static mockExecutor
// returns one result per action).
type funcExecutor struct {
	base *mockExecutor
	fn   func(action string, args map[string]any) (map[string]any, bool)
}

func (f *funcExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	f.base.calls = append(f.base.calls, executorCall{action, args})
	if r, ok := f.fn(action, args); ok {
		return r, nil
	}
	if r, ok := f.base.results[action]; ok {
		return r, nil
	}
	return map[string]any{"RetCode": 0}, nil
}

func TestCreateInstance_FormEditSoldOutFailsGrounded(t *testing.T) {
	// The user edits to a GPU that the re-run stock check reports sold out:
	// the workflow must stop with the grounded 售罄 message — NOT create, NOT
	// fall back to the original GPU silently.
	base := formMockExecutor()
	executor := &funcExecutor{base: base, fn: func(action string, args map[string]any) (map[string]any, bool) {
		if action == "CheckCompShareResourceCapacity" {
			if gt, _ := args["GpuType"].(string); gt == "A800" {
				return map[string]any{"Specs": []any{
					map[string]any{"Gpu": float64(1), "Cpu": float64(32), "Mem": float64(128), "ResourceEnough": false},
				}}, true
			}
		}
		return nil, false
	}}
	eng := NewEngine(executor, nil, nil)
	confirms := 0
	eng.SetConfirmEditsFn(func(string, map[string]any, *ConfirmForm) ConfirmResolution {
		confirms++
		return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}}
	})
	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "检查库存", result.StoppedAt)
	assert.Contains(t, result.Message, "售罄")
	assert.Equal(t, 1, confirms, "sold-out revalidate must stop before a second confirm")
	for _, c := range base.calls {
		assert.NotEqual(t, "CreateCompShareInstance", c.action)
	}
}

func TestConfirmEditsFn_WithoutBuildForm_UsesBooleanGate(t *testing.T) {
	// A StepConfirm with no BuildForm must stay on the legacy boolean gate even
	// when a ConfirmEditsFunc is wired — the form path is opt-in per step.
	executor := &mockExecutor{}
	boolCalled, editsCalled := false, false
	eng := NewEngine(executor, func(string, map[string]any) bool { boolCalled = true; return true }, nil)
	eng.SetConfirmEditsFn(func(string, map[string]any, *ConfirmForm) ConfirmResolution {
		editsCalled = true
		return ConfirmResolution{Confirmed: true}
	})
	def := &Definition{Name: "X", Steps: []Step{{
		Name: "confirm", Type: StepConfirm,
		BuildArgs: func(*Context) (map[string]any, error) { return map[string]any{}, nil },
	}}}
	result, err := eng.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.True(t, boolCalled, "legacy boolean confirm must be used")
	assert.False(t, editsCalled, "form gate must not fire without a step form")
}
