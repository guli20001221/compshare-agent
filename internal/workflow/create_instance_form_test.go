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
		map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
			"MachineSizes": size(1, 16, 94), "GraphicsMemory": map[string]any{"Value": float64(48)}},
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
	assert.Equal(t, []string{"4090", "4090_48G", "A800"}, optionValues(gpu))
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

func TestBuildCreateConfirmForm_ZoneOptionsUseDisplayNames(t *testing.T) {
	// params["ZoneDescribes"] (threaded from the engine's live support-zone
	// catalog) labels zone options with the console's Chinese names so the user
	// recognizes "华北一C" instead of the opaque "cn-bj2-03".
	form, err := buildCreateConfirmForm(formWfCtx(t, map[string]any{
		"GpuType": "4090",
		"ZoneDescribes": map[string]string{
			"cn-wlcb-01": "华北二A",
			"cn-sh2-02":  "上海二B",
		},
	}))
	require.NoError(t, err)
	zone := fieldByKey(t, form, "Zone")
	// Values stay the zone ids — the create API speaks zone ids, not names.
	assert.Equal(t, []string{"cn-wlcb-01", "cn-sh2-02"}, optionValues(zone))
	// Labels carry the display name + id so the dropdown is human-readable.
	assert.Equal(t, "华北二A (cn-wlcb-01)", zone.Options[0].Label)
	assert.Equal(t, "上海二B (cn-sh2-02)", zone.Options[1].Label)
}

func TestBuildCreateConfirmForm_ZoneLabelsFallBackToIdWithoutDescribes(t *testing.T) {
	// No ZoneDescribes (manual/CLI path or catalog unavailable) → labels are the
	// bare zone ids, byte-identical to the pre-change behavior.
	form, err := buildCreateConfirmForm(formWfCtx(t, map[string]any{"GpuType": "4090"}))
	require.NoError(t, err)
	zone := fieldByKey(t, form, "Zone")
	assert.Equal(t, "cn-wlcb-01", zone.Options[0].Label)
	assert.Equal(t, "cn-sh2-02", zone.Options[1].Label)
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

func TestValidateOverrides_DisabledOptionRejected(t *testing.T) {
	form := &ConfirmForm{Version: 2, Fields: []ConfirmFormField{
		{Key: "GpuType", Type: "select", Value: "4090", Editable: true,
			Options: []ConfirmFormOption{
				{Value: "4090", Label: "4090"},
				{Value: "P40", Label: "P40", Disabled: true},
			}},
	}}
	assert.NoError(t, form.ValidateOverrides(map[string]string{"GpuType": "4090"}))
	assert.Error(t, form.ValidateOverrides(map[string]string{"GpuType": "P40"}), "disabled options must not be selectable")
}

// ---------------------------------------------------------------------------
// Guided create flow (Figma-style step cards)
// ---------------------------------------------------------------------------

func TestCreateInstanceGuided_FormStepsContinueAndCreateSelectedSpec(t *testing.T) {
	executor := formMockExecutor()
	var seenSteps []int

	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, args map[string]any, form *ConfirmForm) ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		seenSteps = append(seenSteps, form.Step.Index)

		// Every guided card carries 引导信息 (guidance text). The frontend renders
		// step.Description under the title; dropping it would leave a bare card.
		assert.NotEmpty(t, form.Step.Description,
			"guided step %d must carry guidance text", form.Step.Index)

		switch form.Step.Index {
		case 1:
			assert.Equal(t, 5, form.Step.Total)
			assert.Equal(t, "确认选择", form.Step.PrimaryLabel)
			assert.Equal(t, "跳过", form.Step.SecondaryLabel)
			assert.True(t, form.Step.Skippable)
			gpu := fieldByKey(t, form, "GpuType")
			assert.Equal(t, "cards", gpu.Render)
			assert.Equal(t, []string{"4090", "4090_48G", "A800", "P40"}, optionValues(gpu))
			assert.Contains(t, gpu.Options[0].Label, "24G")
			assert.False(t, gpu.Options[0].Disabled)
			assert.True(t, gpu.Options[3].Disabled)
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}}
		case 2:
			zone := fieldByKey(t, form, "Zone")
			assert.Equal(t, "cards", zone.Render)
			assert.Equal(t, []string{"cn-wlcb-01"}, optionValues(zone))
			return ConfirmResolution{Confirmed: true}
		case 3:
			gpuCount := fieldByKey(t, form, "Gpu")
			assert.Equal(t, "cards", gpuCount.Render)
			assert.Equal(t, []string{"1"}, optionValues(gpuCount))
			return ConfirmResolution{Confirmed: true}
		case 4:
			spec := fieldByKey(t, form, "CpuMemory")
			assert.Equal(t, "cards", spec.Render)
			assert.Contains(t, optionValues(spec), "cn-wlcb-01|1|32|131072")
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"CpuMemory": "cn-wlcb-01|1|32|131072"}}
		case 5:
			assert.Equal(t, "A800", args["GpuType"])
			assert.Equal(t, float64(1), args["Gpu"])
			assert.Equal(t, float64(32), args["CPU"])
			assert.Equal(t, float64(131072), args["Memory"])
			assert.Equal(t, "cn-wlcb-01", args["Zone"])
			assert.Equal(t, "确认部署", form.Step.PrimaryLabel)
			assert.True(t, form.Step.Final)
			return ConfirmResolution{Confirmed: true}
		default:
			t.Fatalf("unexpected guided form step %d", form.Step.Index)
			return ConfirmResolution{}
		}
	})

	result, err := eng.Run(context.Background(), CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, seenSteps)

	var capacityCalls, priceCalls int
	var created map[string]any
	for _, c := range executor.calls {
		switch c.action {
		case "CheckCompShareResourceCapacity":
			capacityCalls++
		case "GetCompShareInstanceUserPrice":
			priceCalls++
		case "CreateCompShareInstance":
			created = c.args
		}
	}
	assert.Equal(t, 1, capacityCalls, "GPU/spec selection steps should continue; stock is checked after the full spec is chosen")
	assert.Equal(t, 1, priceCalls, "price is checked after the full spec is chosen")
	require.NotNil(t, created)
	assert.Equal(t, "A800", created["GpuType"])
	assert.Equal(t, float64(1), created["GPU"])
	assert.Equal(t, float64(32), created["CPU"])
	assert.Equal(t, float64(131072), created["Memory"])
	assert.Equal(t, "cn-wlcb-01", created["Zone"])
}

func TestCreateInstanceGuided_ExplicitGPUOffersIntentFamily(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "GuidedGpuLocked": true})
	form, err := buildGuidedGPUForm(wfCtx)
	require.NoError(t, err)
	require.NotNil(t, form.Step)
	assert.Equal(t, 1, form.Step.Index)
	assert.Equal(t, 5, form.Step.Total)
	gpu := fieldByKey(t, form, "GpuType")
	assert.Equal(t, []string{"4090", "4090_48G"}, optionValues(gpu))
}

func TestCreateInstanceGuided_ExplicitGPUVariantStaysExact(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090_48G", "GuidedGpuLocked": true})
	form, err := buildGuidedGPUForm(wfCtx)
	require.NoError(t, err)
	gpu := fieldByKey(t, form, "GpuType")
	assert.Equal(t, []string{"4090_48G"}, optionValues(gpu))
}

// A model-driven deploy (deploy_model sets GuidedRecommended, not GuidedGpuLocked)
// keeps every GPU on the card but must SHOW the matcher's pick as a recommendation:
// recommendation-aware title + a 推荐 badge on the matching option. Encodes WHY —
// "用户有明确需求的时候，卡片中也应该是相应的推荐的配置".
func TestCreateInstanceGuided_RecommendedGPUIsVisible(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "A800", "GuidedRecommended": true})
	form, err := buildGuidedGPUForm(wfCtx)
	require.NoError(t, err)
	require.NotNil(t, form.Step)
	assert.Contains(t, form.Step.Title, "推荐")
	assert.NotEmpty(t, form.Step.Description)
	gpu := fieldByKey(t, form, "GpuType")
	// unlocked: every option still offered, the recommended one hoisted first + badged.
	assert.Equal(t, []string{"A800", "4090", "4090_48G", "P40"}, optionValues(gpu))
	for _, o := range gpu.Options {
		if o.Value == "A800" {
			assert.Contains(t, o.Note, "推荐", "recommended option must carry a 推荐 badge")
		}
	}
}

func TestGuidedSpecOverrideSetsFullCreateParams(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "Zone": "cn-sh2-02", "Gpu": float64(1)})
	form, err := buildGuidedCpuMemoryForm(wfCtx)
	require.NoError(t, err)
	spec := fieldByKey(t, form, "CpuMemory")
	assert.Equal(t, "cards", spec.Render)
	assert.Contains(t, optionValues(spec), "cn-sh2-02|1|16|65536")

	require.NoError(t, applyGuidedCpuMemoryOverrides(wfCtx, map[string]string{"CpuMemory": "cn-sh2-02|1|16|65536"}))
	assert.Equal(t, "cn-sh2-02", wfCtx.Params["Zone"])
	assert.Equal(t, float64(1), wfCtx.Params["Gpu"])
	assert.Equal(t, float64(16), wfCtx.Params["Cpu"])
	assert.Equal(t, float64(65536), wfCtx.Params["Memory"])
}

func TestCreateInstanceGuided_FinalEditRevalidatesPriceBeforeCreate(t *testing.T) {
	executor := formMockExecutor()
	finalRounds := 0

	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		switch form.Step.Index {
		case 1, 2, 3, 4:
			return ConfirmResolution{Confirmed: true}
		case 5:
			finalRounds++
			if finalRounds == 1 {
				return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"ImageId": "img-002"}}
			}
			return ConfirmResolution{Confirmed: true}
		default:
			t.Fatalf("unexpected guided form step %d", form.Step.Index)
			return ConfirmResolution{}
		}
	})

	result, err := eng.Run(context.Background(), CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, 2, finalRounds, "final image edit must refresh the final confirm card")

	capacityCalls, priceCalls := 0, 0
	var created map[string]any
	for _, c := range executor.calls {
		switch c.action {
		case "CheckCompShareResourceCapacity":
			capacityCalls++
		case "GetCompShareInstanceUserPrice":
			priceCalls++
		case "CreateCompShareInstance":
			created = c.args
		}
	}
	assert.Equal(t, 2, capacityCalls)
	assert.Equal(t, 2, priceCalls)
	require.NotNil(t, created)
	assert.Equal(t, "img-002", created["CompShareImageId"])
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
