package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// platformImageQueryArgs runs the given platform 查询镜像 step's BuildArgs and
// returns what it would send upstream.
func platformImageQueryArgs(t *testing.T, step Step, params map[string]any) map[string]any {
	t.Helper()
	require.NotNil(t, step.BuildArgs)
	args, err := step.BuildArgs(&Context{Params: params})
	require.NoError(t, err)
	return args
}

// TestPlatformImageQueryAsksForTheWholeCatalog guards a page size that was never a
// page.
//
// No Offset is sent anywhere in the flow, so whatever the FIRST response holds is
// the entire catalog as far as the facet options, the picker and the final card are
// concerned. At Limit=20 upstream returned 40 of 72 rows and only 7 of the 36 rows
// that carry tags (live 2026-07-22) — which is why the tag facet looked almost
// empty and why images simply could not be reached from the guided flow.
//
// Upstream refuses Limit=200 outright (RetCode=230 "Params [Limit] not available"),
// so this is a ceiling as well as a fix: the assertion pins the value rather than
// asserting ">= 20", because a future "optimisation" in either direction is a bug.
func TestPlatformImageQueryAsksForTheWholeCatalog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		step   Step
		params map[string]any
	}{
		{"initial query", stepQueryImages(true), map[string]any{}},
		{"initial query, named", stepQueryImages(true), map[string]any{"ImageName": "PyTorch"}},
		{"source re-query", stepReQuerySelectedSourceImages(), map[string]any{}},
		{"source re-query, named", stepReQuerySelectedSourceImages(), map[string]any{"ImageName": "PyTorch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := platformImageQueryArgs(t, tc.step, tc.params)
			assert.Equal(t, maxPlatformImageQueryLimit, args["Limit"],
				"a truncated first page IS a truncated catalog — nothing pages past it")
		})
	}

	assert.Equal(t, 100, maxPlatformImageQueryLimit,
		"upstream refuses 200; 100 returned the full TotalCount=72 catalog")
}

// TestCompoundTagsDoNotReachTheFacetCard is the user-visible half of the tag split.
// The facet builder uses each tag string as BOTH the option value and its label, so
// an unsplit "comfyUI，ComfyUI" is what the user is asked to click — and picking the
// clean spelling then matches nothing, because membership is exact.
func TestCompoundTagsDoNotReachTheFacetCard(t *testing.T) {
	snap := deployment.NewImageCatalogSnapshot(true, deployment.ParsePlatformImageEntries(
		map[string]any{"ImageSet": []any{
			map[string]any{
				"CompShareImageId": "img-a", "Name": "PyTorch", "ImageType": "App",
				"Status": "Available", "Tags": []any{"pytorch，Pytorch"},
			},
			map[string]any{
				"CompShareImageId": "img-b", "Name": "ComfyUI", "ImageType": "App",
				"Status": "Available", "Tags": []any{"comfyUI，ComfyUI"},
			},
		}}, "platform"))

	opts := imageTagFacetOptions(snap)
	require.NotEmpty(t, opts)
	for _, o := range opts {
		assert.NotContains(t, o.Label, "，",
			"a compound upstream string must never be offered as a tag to click")
	}

	// And the clean spelling actually selects the image that carried the compound.
	assert.True(t, imageSelectionMatchesFacets(snap, "img-b", "", "ComfyUI"),
		"picking the tag shown must match the image it was derived from")
	assert.False(t, imageSelectionMatchesFacets(snap, "img-a", "", "ComfyUI"),
		"…and must not match an unrelated image")
}
