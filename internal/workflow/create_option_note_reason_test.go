package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The client renders a disabled option's caption as
//
//	[o.Note, o.Disabled && o.Reason].filter(Boolean).join(' · ')
//
// (frame/src/Frame/AIAssistant/components/MessageItem.jsx). Note and Reason are
// therefore two halves of one line, not two places to say the same thing: Note is
// the neutral context, Reason is why the option cannot be picked. Producers that
// wrote the reason into both printed it twice —
//
//	"4090 · 该可用区不支持独占购买方式 · 该可用区不支持独占购买方式"
//
// which is what a user reported. Three of the five option producers did it (the
// GPU card, the zone card, and the image picker's GPU mismatch); the GPU-count and
// CPU/memory cards were already correct.
//
// assertNoteAndReasonDoNotRepeat is deliberately about the RENDERED string rather
// than about equality, because the image picker's pair was not equal — it was
// "所选镜像不支持当前 GPU" and "镜像不支持当前 GPU", which reads as a duplicate but
// compares as different.
// renderedOptionCaption reproduces the client's join, so a test can assert what
// the USER reads instead of which backend field happens to hold it. Assertions
// pinned to one field went red when the reason moved from Note to Reason even
// though the caption was unchanged — and would have stayed green if the reason had
// been dropped from both.
func renderedOptionCaption(o ConfirmFormOption) string {
	parts := []string{o.Note}
	if o.Disabled {
		parts = append(parts, o.Reason)
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

func assertNoteAndReasonDoNotRepeat(t *testing.T, card string, opts []ConfirmFormOption) {
	t.Helper()
	checked := 0
	for _, o := range opts {
		if !o.Disabled || o.Reason == "" {
			continue
		}
		checked++
		assert.NotContains(t, o.Note, o.Reason,
			"%s option %q: the client joins Note and Reason, so a reason in both prints twice", card, o.Value)
		// Near-duplicates too: neither half may be a substring of the other.
		if o.Note != "" {
			assert.NotContains(t, o.Reason, o.Note,
				"%s option %q: Note is contained in Reason and would read as a repeat", card, o.Value)
		}
	}
	assert.Positive(t, checked,
		"%s: no disabled option carried a reason — this assertion proved nothing", card)
}

// noteReasonCatalog offers 4090 in two zones with two card counts and two
// CPU/memory combos, plus a second model — every shape needed for each card to
// have BOTH a disabled and an enabled option. Without the second option per axis
// the sweep runs against cards that disable nothing and proves nothing.
func noteReasonCatalog() map[string]any {
	row := func(name, zone string, sizes []any, vram float64) map[string]any {
		return map[string]any{
			"Name": name, "Zone": zone, "Status": "Normal", "MachineSizes": sizes,
			"CpuPlatforms":   map[string]any{"Amd": map[string]any{}},
			"GraphicsMemory": map[string]any{"Value": vram},
			"Disks": []any{map[string]any{"BootDisk": []any{
				map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)},
			}}},
		}
	}
	return map[string]any{"AvailableInstanceTypes": []any{
		row("4090", "cn-wlcb-01", []any{
			map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(128)}},
			}},
			map[string]any{"Gpu": float64(2), "Collection": []any{
				map[string]any{"Cpu": float64(32), "Memory": []any{float64(128)}},
			}},
		}, 24),
		row("4090", "cn-sh2-02", []any{
			map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}},
		}, 24),
		row("A800", "cn-wlcb-01", []any{
			map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(32), "Memory": []any{float64(128)}},
			}},
		}, 80),
	}}
}

// noteReasonWfCtx builds a context where EVERY guided card has something to
// disable: a GPU the image does not support, a sold-out zone, a card count and a
// CPU/memory combo capacity found short.
func noteReasonWfCtx(t *testing.T) *Context {
	t.Helper()
	wfCtx := NewContext(map[string]any{
		"GpuType":          "4090",
		"CompShareImageId": "img-vm",
		"ImageName":        "Ubuntu-nvidia 22.04",
		"Zone":             "cn-wlcb-01",
		"ChargeType":       "Postpay",
	})
	wfCtx.referenceData.ZoneCatalog = createZoneCatalog()
	wfCtx.StepResults["查询可用配比"] = noteReasonCatalog()
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-vm", "Name": "Ubuntu-nvidia 22.04",
			"ImageType": "System", "Status": "Available", "Container": "False",
			"SupportedGpuTypes": []any{"4090"}},
		map[string]any{"CompShareImageId": "img-other", "Name": "A800 专用镜像",
			"ImageType": "App", "Status": "Available", "Container": "True",
			"SupportedGpuTypes": []any{"A800"}},
	}}
	// 4090 creatable in cn-wlcb-01, sold out in cn-sh2-02 — so the zone card has a
	// disabled option AND an enabled one (no stand-down).
	wfCtx.StepResults[zoneCapacityStepName] = map[string]any{batchResultsKey: []any{
		map[string]any{"Key": capacityComboKey("4090", "cn-wlcb-01"), "OK": true, "Result": map[string]any{
			"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}},
		}},
		map[string]any{"Key": capacityComboKey("4090", "cn-sh2-02"), "OK": true, "Result": map[string]any{
			"Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false}},
		}},
	}}
	// Capacity enumerated both card counts (2 is short) and both CPU/memory combos
	// (16C/128G is short), so each of those cards has one disabled and one enabled.
	wfCtx.StepResults[capacitySpecsStepName] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(128), "ResourceEnough": false},
		map[string]any{"Gpu": float64(2), "Cpu": float64(32), "Mem": float64(128), "ResourceEnough": false},
	}}
	return wfCtx
}

// TestNoGuidedCardPrintsItsReasonTwice is the sweep. It walks every option
// producer the guided create flow has, not just the two a user happened to
// report, so a fourth producer added later is covered by construction.
func TestNoGuidedCardPrintsItsReasonTwice(t *testing.T) {
	wfCtx := noteReasonWfCtx(t)
	catalog := wfCtx.Result("查询可用配比")
	inventory := wfCtx.Result("查询GPU库存")

	t.Run("GPU card", func(t *testing.T) {
		_, opts := guidedGPUFormOptions(wfCtx, catalog,
			currentImageSupportedGPUs(wfCtx.Params, wfCtx.Result("查询镜像")), "4090", false, wfCtx.Params, inventory)
		require.NotEmpty(t, opts)
		assertNoteAndReasonDoNotRepeat(t, "GPU card", opts)
	})

	t.Run("zone card", func(t *testing.T) {
		_, opts, stoodDown := guidedZoneFormOptions(wfCtx, catalog, "4090", "cn-wlcb-01", wfCtx.Params, inventory)
		require.NotEmpty(t, opts)
		require.False(t, stoodDown, "premise: this fixture leaves a selectable zone, so reasons survive as Reason")
		assertNoteAndReasonDoNotRepeat(t, "zone card", opts)
	})

	t.Run("GPU count card", func(t *testing.T) {
		_, opts := guidedGPUCountFormOptions(wfCtx, catalog, "4090", "cn-wlcb-01", 1, wfCtx.Params, inventory)
		require.NotEmpty(t, opts)
		assertNoteAndReasonDoNotRepeat(t, "GPU count card", opts)
	})

	t.Run("CPU/memory card", func(t *testing.T) {
		_, opts := guidedCpuMemoryFormOptions(wfCtx, catalog, "4090", "cn-wlcb-01", 1, wfCtx.Params, inventory)
		require.NotEmpty(t, opts)
		assertNoteAndReasonDoNotRepeat(t, "CPU/memory card", opts)
	})

	t.Run("image picker", func(t *testing.T) {
		// A800-only image against a 4090 create: the picker shows it DISABLED rather
		// than hiding it, which is the case that carried the near-duplicate pair.
		params := map[string]any{"ImageSource": "platform"}
		_, opts, _ := guidedImageFormOptions(params, wfCtx.Result("查询镜像"), "4090", nil, false)
		require.NotEmpty(t, opts)
		assertNoteAndReasonDoNotRepeat(t, "image picker", opts)
	})
}

// TestStandDownKeepsTheReasonVisible is the interaction the split creates. With
// Note and Reason disjoint, re-enabling an option drops the reason unless it is
// folded in first — the client only renders Reason while Disabled is true.
func TestStandDownKeepsTheReasonVisible(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090", "CompShareImageId": "img-vm"})
	wfCtx.referenceData.ZoneCatalog = createZoneCatalog()
	catalog := vmAndPodZoneCatalog()
	wfCtx.StepResults["查询可用配比"] = catalog
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

	_, opts, stoodDown := guidedZoneFormOptions(wfCtx, catalog, "4090", "", wfCtx.Params, nil)
	require.True(t, stoodDown)
	byZone := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byZone[o.Value] = o
		require.False(t, o.Disabled)
		require.Empty(t, o.Reason, "a re-enabled option must not keep a Reason the client will not render")
	}
	assert.Contains(t, byZone["cn-wlcb-01"].Note, "无可创建库存")
	assert.Contains(t, byZone["cn-bj2-03"].Note, "容器镜像")
	// Folded exactly once — the reason must not be doubled by the fold either.
	assert.Equal(t, 1, strings.Count(byZone["cn-bj2-03"].Note, "不是容器镜像"))
}
