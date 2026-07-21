package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuidedImagePicker_VersionsOfOneFamilyAreDistinguishable guards the other half of
// the named-community-create fix. Running the picker is only useful if the user can
// TELL THE VERSIONS APART: ParseCommunityImageEntries overwrites every version row's
// Name with the family (group) name, so a picker labelled by Name alone renders N
// identical "InfiniteTalk" choices. The live failure was exactly that — the card
// offered ~10 versions that all looked the same, so "at least you can choose" was not
// true in practice; picking v26.0201 was a blind guess. The label must carry the
// version, and the submitted value must stay the concrete image id.
func TestGuidedImagePicker_VersionsOfOneFamilyAreDistinguishable(t *testing.T) {
	images := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "InfiniteTalk", "Data": []any{
			// Both rows carry the family name — this is the real upstream shape.
			map[string]any{"CompShareImageId": "compshareImage-v260201", "Name": "InfiniteTalk", "VersionName": "v26.0201"},
			map[string]any{"CompShareImageId": "compshareImage-v251130", "Name": "InfiniteTalk", "VersionName": "v25.1130"},
		}},
	}}
	_, opts := guidedImageFormOptions(map[string]any{"ImageSource": "community"}, images, "")
	require.Len(t, opts, 2, "both versions must be offered")

	labels := map[string]string{} // label -> id
	for _, o := range opts {
		require.NotContains(t, labels, o.Label,
			"two versions of one family rendered the SAME label %q — the user cannot tell them apart: %+v", o.Label, opts)
		labels[o.Label] = o.Value
	}
	// The version, not just the family, must be visible.
	var withVersion int
	for label := range labels {
		if strings.Contains(label, "v26.0201") || strings.Contains(label, "v25.1130") {
			withVersion++
		}
	}
	assert.Equal(t, 2, withVersion, "each label must carry its version: %v", labels)
	// The card still submits the concrete image id, never the display name.
	assert.Equal(t, "compshareImage-v260201", labels["InfiniteTalk · v26.0201"])
	assert.Equal(t, "compshareImage-v251130", labels["InfiniteTalk · v25.1130"])
}

// TestGuidedGPU_ConcreteImageConstrainsGPUList pins WHY resolving a concrete image id
// matters beyond the image card itself: the live failure offered every GPU (a 2080 was
// selectable for an InfiniteTalk create, which then failed at the end). The GPU list is
// narrowed by currentImageSupportedGPUs, which yields a supported set ONLY once a
// concrete image id exists — with just a name it returns nil and nothing is disabled.
// So running the picker is what re-arms the GPU constraint. This test proves both legs
// so a regression in either is caught.
func TestGuidedGPU_ConcreteImageConstrainsGPUList(t *testing.T) {
	images := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "InfiniteTalk", "Data": []any{
			map[string]any{
				"CompShareImageId":  "compshareImage-v260201",
				"Name":              "InfiniteTalk",
				"VersionName":       "v26.0201",
				"SupportedGpuTypes": []any{"4090"},
			},
		}},
	}}
	catalog := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090", "Status": "Normal", "Zone": "cn-wlcb-01"},
		map[string]any{"Name": "2080Ti", "Status": "Normal", "Zone": "cn-wlcb-01"},
	}}

	// Leg 1 — concrete id resolved (what the picker now produces): the image's
	// supported set is known, so an unsupported GPU is offered DISABLED, not silently
	// selectable-then-failing.
	pinned := map[string]any{"ImageSource": "community", "CompShareImageId": "compshareImage-v260201"}
	supported := currentImageSupportedGPUs(pinned, images)
	require.NotEmpty(t, supported, "a concrete image id must yield its supported GPU list")
	_, opts := guidedGPUFormOptions(NewContext(pinned), catalog, supported, "", false, pinned, nil)
	byName := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byName[o.Value] = o
	}
	require.Contains(t, byName, "2080Ti")
	assert.True(t, byName["2080Ti"].Disabled, "an unsupported GPU must be disabled once the image is concrete")
	assert.False(t, byName["4090"].Disabled, "the image's supported GPU stays selectable")

	// Leg 2 — a name that resolves to exactly one row still constrains the list, so the
	// constraint is driven by RESOLUTION, not by which field was filled.
	resolvable := map[string]any{"ImageSource": "community", "ImageName": "InfiniteTalk"}
	assert.NotEmpty(t, currentImageSupportedGPUs(resolvable, images),
		"an unambiguously resolvable name must still yield the image's GPU constraint")

	// Leg 3 — community browsing with neither an id nor a name (the documented early
	// return in currentImageSupportedGPUs): no image is pinned, so NOTHING constrains
	// the GPU list and every card stays enabled. This is the shape of the live 2080
	// report — a create where no concrete image was ever pinned offered every GPU and
	// only failed at the end. It is why the picker must pin an id BEFORE the GPU step.
	browsing := map[string]any{"ImageSource": "community"}
	require.Empty(t, currentImageSupportedGPUs(browsing, images),
		"community browsing pins no image, so no GPU constraint is known")
	_, wideOpts := guidedGPUFormOptions(NewContext(browsing), catalog, nil, "", false, browsing, nil)
	require.NotEmpty(t, wideOpts)
	for _, o := range wideOpts {
		assert.False(t, o.Disabled,
			"with no pinned image every GPU stays enabled (%s) — the gap the picker closes", o.Value)
	}
}

// TestCommunityNameMissBrowsesInsteadOfDeadEnding guards the live regression where
// "用最强AI数字人 InfiniteTalk为我创建一台机器" answered "未找到可选社区镜像，请换一个镜像
// 来源或稍后再试". stepQueryImages passes ImageName through as the upstream FuzzySearch,
// so a wording the platform does not match returns ZERO rows and the picker has nothing
// to offer — the image is on the platform, only the phrasing missed. The rescue must run
// exactly then, and must browse rather than re-apply the name that just failed.
func TestCommunityNameMissBrowsesInsteadOfDeadEnding(t *testing.T) {
	step := stepBrowseCommunityWhenNameMatchedNothing()
	named := func(result map[string]any) *Context {
		c := NewContext(map[string]any{"ImageSource": "community", "ImageName": "最强AI数字人 InfiniteTalk"})
		c.StepResults["查询镜像"] = result
		return c
	}
	oneRow := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "InfiniteTalk", "Data": []any{
			map[string]any{"CompShareImageId": "compshareImage-hit", "Name": "InfiniteTalk", "VersionName": "v26.0201"},
		}},
	}}

	// The regression: the named search came back empty → rescue must run.
	skip, err := step.SkipIf(named(map[string]any{"CompshareImageGroup": []any{}}))
	require.NoError(t, err)
	require.False(t, skip, "an empty named community search must fall back to browsing, not dead-end")
	args, err := step.BuildArgs(named(map[string]any{"CompshareImageGroup": []any{}}))
	require.NoError(t, err)
	assert.NotContains(t, args, "FuzzySearch",
		"the rescue must browse — re-applying the name that just missed would return empty again")

	// A narrowing that actually worked is useful and must be left alone.
	skip, err = step.SkipIf(named(oneRow))
	require.NoError(t, err)
	assert.True(t, skip, "a non-empty narrowed catalog must not be widened")

	// Platform source, and community browsing with no name, are both none of its business.
	platform := NewContext(map[string]any{"ImageSource": "platform", "ImageName": "Ubuntu"})
	platform.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{}}
	skip, err = step.SkipIf(platform)
	require.NoError(t, err)
	assert.True(t, skip, "platform creates resolve their image in the final form")

	browsing := NewContext(map[string]any{"ImageSource": "community"})
	browsing.StepResults["查询镜像"] = map[string]any{"CompshareImageGroup": []any{}}
	skip, err = step.SkipIf(browsing)
	require.NoError(t, err)
	assert.True(t, skip, "a nameless community create is already browsing the whole catalog")
}

// TestGuidedCreateWiresCommunityNameMissRescue is the anti-orphan gate: the rescue must
// sit IN the guided flow, after the source re-query and before the picker that consumes
// the catalog it repairs.
func TestGuidedCreateWiresCommunityNameMissRescue(t *testing.T) {
	steps := CreateInstanceGuidedDef().Steps
	// 查询镜像 is deliberately reused as a step name (each occurrence overwrites the
	// previous result), so identify the rescue by its position among them.
	var queryPositions []int
	pickerAt := -1
	for i, s := range steps {
		switch s.Name {
		case "查询镜像":
			queryPositions = append(queryPositions, i)
		case "选择镜像":
			pickerAt = i
		}
	}
	require.GreaterOrEqual(t, len(queryPositions), 3,
		"the guided flow must carry the initial query, the source re-query AND the name-miss rescue")
	require.NotEqual(t, -1, pickerAt)
	assert.Less(t, queryPositions[len(queryPositions)-1], pickerAt,
		"the rescue must run before the picker that consumes the catalog")
}

// TestGuidedImagePicker_MissingVersionDegradesToName holds the honest-absence line: an
// upstream row with no version label must show the plain family name, never a
// fabricated or empty version suffix.
func TestGuidedImagePicker_MissingVersionDegradesToName(t *testing.T) {
	images := map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "LiveTalking", "Data": []any{
			map[string]any{"CompShareImageId": "compshareImage-solo", "Name": "LiveTalking"},
		}},
	}}
	_, opts := guidedImageFormOptions(map[string]any{"ImageSource": "community"}, images, "")
	require.Len(t, opts, 1)
	assert.Equal(t, "LiveTalking", opts[0].Label)
	assert.Equal(t, "compshareImage-solo", opts[0].Value)
}

// TestShouldSkipGuidedImageStep_CommunityNamedRunsPicker guards the fix for a named
// community create that failed with "社区镜像未返回有效的镜像 ID". A community NAME is not
// a concrete CompShareImageId (and is usually ambiguous across versions), so the
// concrete-image picker must still run to resolve one. Skipping it on the name (the
// old behavior) left the image unresolved: create failed at materializeCreateDraft,
// and — with no concrete image id — the GPU list was never constrained by
// SupportedGpuTypes and the capacity precheck (guidedCapacityArgs) was skipped, so
// every card/spec showed as selectable regardless of the image (e.g. 2080Ti offered
// for an InfiniteTalk create). Platform names still skip (resolved in the final
// form); an already-concrete CompShareImageId still skips.
func TestShouldSkipGuidedImageStep_CommunityNamedRunsPicker(t *testing.T) {
	cases := []struct {
		name     string
		params   map[string]any
		wantSkip bool
	}{
		{"community + only a name → picker runs to resolve a concrete id",
			map[string]any{"ImageSource": "community", "ImageName": "最强AI数字人 InfiniteTalk"}, false},
		{"community + concrete CompShareImageId → skip (already resolved)",
			map[string]any{"ImageSource": "community", "CompShareImageId": "cimg-abc"}, true},
		{"community + no name → picker runs to browse",
			map[string]any{"ImageSource": "community"}, false},
		{"platform + a name → skip (resolved in the final form)",
			map[string]any{"ImageSource": "platform", "ImageName": "Ubuntu-nvidia 22.04"}, true},
		{"platform + nothing → skip",
			map[string]any{"ImageSource": "platform"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wfCtx := NewContext(tc.params)
			skip, err := shouldSkipGuidedImageStep(wfCtx)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSkip, skip)
		})
	}
}
