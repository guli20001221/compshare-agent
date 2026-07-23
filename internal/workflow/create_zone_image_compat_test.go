package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vmAndPodZoneCatalog offers one GPU in a VM zone and a Pod zone, which is the
// live shape (4090 sells in both) and the only one under which the image/zone
// constraint is expressible.
func vmAndPodZoneCatalog() map[string]any {
	row := func(zone string) map[string]any {
		return map[string]any{
			"Name": "4090", "Zone": zone, "Status": "Normal",
			"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}}},
			"CpuPlatforms": map[string]any{"Amd": map[string]any{}},
			"Disks": []any{map[string]any{"BootDisk": []any{
				map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)},
			}}},
			"GraphicsMemory": map[string]any{"Value": float64(24)},
		}
	}
	// cn-wlcb-01 is a VM zone and cn-bj2-03 a Pod zone in createZoneCatalog().
	return map[string]any{"AvailableInstanceTypes": []any{row("cn-wlcb-01"), row("cn-bj2-03")}}
}

func vmOnlyImageExecutor() *mockExecutor {
	ex := formMockExecutor()
	ex.results["DescribeAvailableCompShareInstanceTypes"] = vmAndPodZoneCatalog()
	ex.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-vm", "Name": "Ubuntu-nvidia 22.04",
			"ImageType": "System", "Status": "Available", "Container": "False", "Size": float64(102400)},
	}}
	return ex
}

// TestAPodZoneIsNotOfferedForAVmOnlyImage is the live failure: "创建一台4090实例"
// settled on Ubuntu-nvidia 22.04, the flow's own zone default then landed on a Pod
// zone, and the run died at the last gate with "Ubuntu-nvidia 22.04 不是容器镜像，
// 不能用于 上海二A".
//
// Nothing about that was unknowable at the time. The image is settled four cards
// before the zone card, and whether a zone is a Pod zone is a catalog fact — so the
// pair could be rejected while the user could still act on it.
//
// Scope of the claim: the card reads the create gate's own rule
// (imageContainerFitForZone), so it never disables a zone the gate would have
// accepted. The converse does NOT hold and this test does not assert it — an image
// id absent from the catalog page passes here and is refused there, and the
// combined stand-down deliberately re-enables everything when no zone survives.
// See TestCrossingGatesDoNotStrandTheZoneCard.
func TestAPodZoneIsNotOfferedForAVmOnlyImage(t *testing.T) {
	var podOption, vmOption *ConfirmFormOption
	var chosenZone string

	eng := NewEngine(vmOnlyImageExecutor(), nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if zone := form.Field("Zone"); zone != nil && zone.Editable {
			chosenZone = zone.Value
			for i := range zone.Options {
				switch zone.Options[i].Value {
				case "cn-bj2-03":
					podOption = &zone.Options[i]
				case "cn-wlcb-01":
					vmOption = &zone.Options[i]
				}
			}
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	assert.True(t, result.Success,
		"the flow must not assemble a pair it will refuse: %s", result.Message)

	require.NotNil(t, podOption, "premise: the Pod zone is a candidate for this GPU")
	require.NotNil(t, vmOption, "premise: a VM zone is also a candidate")
	assert.True(t, podOption.Disabled, "a Pod zone cannot boot a VM-only image")
	assert.Contains(t, podOption.Reason, "容器镜像",
		"the card must say WHY, in terms of the image the user picked")
	assert.False(t, vmOption.Disabled)
	assert.Equal(t, "cn-wlcb-01", chosenZone,
		"and the default must land on a zone that actually works")
}

// TestPinnedPodZoneNeverSealsAVMImage is the second live failure: "在上海二A创建
// 一台5090实例" pinned a POD zone up front and no image, and the run died at the
// last gate with "Ubuntu-nvidia 22.04 不是容器镜像，不能用于 上海二A".
//
// The zone-card gate did not catch it because a pinned zone SKIPS the zone card —
// and under the image-first order the picker had already run and sealed a VM image.
// The picker read ZoneIsPod (a param written only at the zone card) as absent =
// non-pod, so it never applied the container filter. The fix resolves the pod flag
// from the zone catalog at the picker, the same authority the create gate uses, so
// a pinned pod zone offers only container images.
//
// This drives the REAL guided flow, not the picker in isolation, so it proves the
// pinned-zone path actually reaches the fixed resolution.
func TestPinnedPodZoneNeverSealsAVMImage(t *testing.T) {
	ex := formMockExecutor()
	// 5090 sells only in a POD zone here; createZoneCatalog marks cn-bj2-03 IsPod.
	ex.results["DescribeAvailableCompShareInstanceTypes"] = map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "5090", "Zone": "cn-bj2-03", "Status": "Normal",
			"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}}},
			"CpuPlatforms":   map[string]any{"Amd": map[string]any{}},
			"GraphicsMemory": map[string]any{"Value": float64(32)},
			"Disks": []any{map[string]any{"BootDisk": []any{
				map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)},
			}}},
		},
	}}
	ex.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-vm", "Name": "Ubuntu-nvidia 22.04",
			"ImageType": "System", "Status": "Available", "Container": "False", "Size": float64(102400)},
		map[string]any{"CompShareImageId": "img-ctr", "Name": "PyTorch 2.4",
			"ImageType": "App", "Status": "Available", "Container": "True", "Size": float64(102400)},
	}}

	var pickerValues []string
	eng := NewEngine(ex, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if img := form.Field("ImageId"); img != nil && img.Editable {
			pickerValues = optionValues(img)
		}
		return ConfirmResolution{Confirmed: true}
	})

	// Pinned zone + GPU, no image — exactly "在上海二A创建一台5090实例".
	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "5090", "Zone": "cn-bj2-03"})
	require.NoError(t, err)
	assert.True(t, result.Success,
		"a pinned pod zone must seal a container image, not fail at the create gate: %s", result.Message)
	require.NotEmpty(t, pickerValues, "premise: the picker ran (no image was pinned)")
	assert.NotContains(t, pickerValues, "img-vm",
		"the picker must not even offer a VM image for a pinned pod zone")
	assert.Contains(t, pickerValues, "img-ctr")

	var created map[string]any
	for _, c := range ex.calls {
		if c.action == "CreateCompShareInstance" {
			created = c.args
		}
	}
	require.NotNil(t, created, "the create call must have been reached")
	assert.Equal(t, "img-ctr", created["CompShareImageId"], "the container image was sealed")
}

// TestAContainerImageStillReachesBothZones is the direction guard. Container
// images run in both kinds of zone, so the new gate must disable nothing for them
// — a rule that grays out zones for every image is not a fix, it is a narrower bug.
func TestAContainerImageStillReachesBothZones(t *testing.T) {
	ex := vmOnlyImageExecutor()
	ex.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-ctr", "Name": "PyTorch 2.4",
			"ImageType": "App", "Status": "Available", "Container": "True", "Size": float64(102400)},
	}}

	var disabledZones []string
	eng := NewEngine(ex, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if zone := form.Field("Zone"); zone != nil && zone.Editable {
			for _, o := range zone.Options {
				if o.Disabled {
					disabledZones = append(disabledZones, o.Value)
				}
			}
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Empty(t, disabledZones, "a container image is compatible with every zone")
}

// TestCrossingGatesDoNotStrandTheZoneCard is the combination boundary. Each rule
// used to carry its own stand-down and each one was individually satisfied here —
// capacity found a creatable zone, the image rule found a compatible zone — so
// neither stood down, and the card still went out with nothing enabled, because
// they were not the SAME zone. Per-rule escapes cannot see that; only a decision
// taken on the assembled options can.
//
// It asserts the notes survive too. Standing down is a DOWNGRADE, not a fix: one
// of the known-bad zones becomes the default and the flow keeps going. The confirm
// protocol has no back operation (ConfirmResolution is confirmed/denied plus
// overrides), so the user's real options are cancel-and-restart or continue and be
// stopped by 检查库存 / the create gate. Keeping the per-zone reasons is what makes
// that an informed choice rather than a silent one — it is not a substitute for a
// zone-step structured conflict that could send them back to the image card.
func TestCrossingGatesDoNotStrandTheZoneCard(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090", "CompShareImageId": "img-vm"})
	wfCtx.referenceData.ZoneCatalog = createZoneCatalog()
	catalog := vmAndPodZoneCatalog()
	wfCtx.StepResults["查询可用配比"] = catalog
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-vm", "Name": "Ubuntu-nvidia 22.04",
			"ImageType": "System", "Status": "Available", "Container": "False"},
	}}
	// Capacity kills the VM zone; the image kills the Pod zone. Disjoint survivors.
	wfCtx.StepResults[zoneCapacityStepName] = map[string]any{batchResultsKey: []any{
		map[string]any{"Key": capacityComboKey("4090", "cn-wlcb-01"), "OK": true, "Result": map[string]any{
			"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false}},
		}},
		map[string]any{"Key": capacityComboKey("4090", "cn-bj2-03"), "OK": true, "Result": map[string]any{
			"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}},
		}},
	}}

	selected, opts, _ := guidedZoneFormOptions(wfCtx, catalog, "4090", "", wfCtx.Params, nil)
	require.Len(t, opts, 2)
	assert.NotEmpty(t, selected,
		"two individually-satisfied rules must not cross into a card with nothing to select")

	byZone := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byZone[o.Value] = o
		assert.False(t, o.Disabled, "nothing is selectable, so nothing may be grayed out")
		assert.Empty(t, o.Reason)
	}
	assert.Contains(t, byZone["cn-wlcb-01"].Note, "无可创建库存",
		"the capacity reason must survive the stand-down as a note")
	assert.Contains(t, byZone["cn-bj2-03"].Note, "容器镜像",
		"and so must the image reason — the card still explains itself")
}

// TestAStoodDownZoneCardSaysSo guards the heading. Re-enabling the options while
// the card still reads "建议优先选择有现货的可用区" tells the user to pick a good
// zone when there is none, which reads as a rendering glitch rather than the
// conflict it is. The copy must name the conflict, state that continuing is still
// safe (later gates hold), and name the remedy the protocol actually offers —
// cancel and restart, because there is no back.
func TestAStoodDownZoneCardSaysSo(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090", "CompShareImageId": "img-vm"})
	wfCtx.referenceData.ZoneCatalog = createZoneCatalog()
	wfCtx.StepResults["查询可用配比"] = vmAndPodZoneCatalog()
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-vm", "Name": "Ubuntu-nvidia 22.04",
			"ImageType": "System", "Status": "Available", "Container": "False"},
	}}
	wfCtx.StepResults[zoneCapacityStepName] = map[string]any{batchResultsKey: []any{
		map[string]any{"Key": capacityComboKey("4090", "cn-wlcb-01"), "OK": true, "Result": map[string]any{
			"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false}},
		}},
		map[string]any{"Key": capacityComboKey("4090", "cn-bj2-03"), "OK": true, "Result": map[string]any{
			"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}},
		}},
	}}

	form, err := buildGuidedZoneForm(wfCtx)
	require.NoError(t, err)
	assert.NotContains(t, form.Step.Description, "建议优先选择有现货的可用区",
		"the normal copy recommends a choice that does not exist here")
	assert.Contains(t, form.Step.Description, "没有同时满足")
	assert.Contains(t, form.Step.Description, "取消本次创建")

	// And an ordinary card keeps the ordinary copy — otherwise this is just a
	// rewrite, not a conditional one.
	ok := NewContext(map[string]any{"GpuType": "4090", "CompShareImageId": "img-ctr"})
	ok.referenceData.ZoneCatalog = createZoneCatalog()
	ok.StepResults["查询可用配比"] = vmAndPodZoneCatalog()
	ok.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-ctr", "Name": "PyTorch", "ImageType": "App",
			"Status": "Available", "Container": "True"},
	}}
	okForm, err := buildGuidedZoneForm(ok)
	require.NoError(t, err)
	assert.Contains(t, okForm.Step.Description, "建议优先选择有现货的可用区")
}

// TestWhenNoZoneCanRunTheImageTheGateStandsDown keeps the gate a steering aid
// rather than a dead end. Graying out every option leaves a card that offers
// nothing, and the zone step turns that into "暂无可选可用区" — raised BEFORE the
// draft exists, so the failure record loses both the candidate draft and its typed
// reason. The existing capacity gate already stands down for exactly this reason;
// this one follows it, and the create gate refuses with a message that names the
// image, which is the thing the user would have to change.
func TestWhenNoZoneCanRunTheImageTheGateStandsDown(t *testing.T) {
	ex := vmOnlyImageExecutor()
	// Only the Pod zone sells this GPU, so a VM-only image has nowhere to go.
	ex.results["DescribeAvailableCompShareInstanceTypes"] = map[string]any{
		"AvailableInstanceTypes": []any{
			vmAndPodZoneCatalog()["AvailableInstanceTypes"].([]any)[1],
		},
	}

	sawZoneCard := false
	eng := NewEngine(ex, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if zone := form.Field("Zone"); zone != nil && zone.Editable {
			sawZoneCard = true
			assert.True(t, enabledOptionExists(zone.Options, "cn-bj2-03"),
				"with nothing to steer toward, the only zone stays selectable")
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, sawZoneCard, "the card must still be shown, not collapsed into a dead end")
	require.False(t, result.Success, "the create gate is what refuses here")
	assert.Contains(t, result.Message, "容器镜像",
		"and it names the image, which is what the user would have to change")
}
