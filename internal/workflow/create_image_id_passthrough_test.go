package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// TestCreateImageCatalogPrefersThisRunsQueryOverAnInjectedSnapshot is the
// protection that used to be expressed as "create must carry no CompShareImageId
// field" in actionresolver's TestCodecImageActivatedForImageBearingOps. Create now
// carries one, so the engine threads a proposal-time ImageCatalog, and the reason
// that used to be dangerous has to be defended directly.
//
// The proposal-time snapshot is taken against the source the PROPOSAL declared.
// The guided flow then lets the user switch source (选择镜像来源 → 查询镜像 re-query)
// and, when a name matched nothing, widens to the whole catalog. If the injected
// snapshot outranked those, a user who switched to community would be offered the
// platform images of a source they just left — silently, since both catalogs are
// well-formed.
func TestCreateImageCatalogPrefersThisRunsQueryOverAnInjectedSnapshot(t *testing.T) {
	injected := deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{
		{ID: "img-proposal-time", Name: "Ubuntu 22.04", Source: "platform", Status: "Available"},
	})

	wfCtx := NewContext(map[string]any{"ImageSource": "community"})
	wfCtx.referenceData.ImageCatalog = injected

	// Before the run queries anything the injected snapshot is all there is, and
	// using it is right — it is a fallback, not a competitor.
	assert.Equal(t, "img-proposal-time", createImageCatalog(wfCtx).Entries()[0].ID,
		"with no query result yet, the threaded snapshot is the only catalog there is")

	// Once this run has queried — the source re-query, or the name-miss browse
	// rescue — its result is newer AND matches the current ImageSource, so it wins.
	wfCtx.StepResults["查询镜像"] = map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "InfiniteTalk", "Data": []any{
			map[string]any{"CompShareImageId": "img-this-run", "Name": "InfiniteTalk", "VersionName": "v26.0201"},
		}},
	}}
	entries := createImageCatalog(wfCtx).Entries()
	require.NotEmpty(t, entries)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	assert.Contains(t, ids, "img-this-run",
		"the catalog this run re-queried for the chosen source must be what the flow selects from")
	assert.NotContains(t, ids, "img-proposal-time",
		"a proposal-time snapshot must not shadow a source switch or the name-miss browse rescue")
}

// TestGuidedCreateAcceptsAConcreteImageIdAndSkipsThePicker pins the passthrough
// end to end at the workflow boundary: an id that arrives in params is the image,
// no name resolution happens, and the picker that exists to RESOLVE an image does
// not ask a question that is already answered.
func TestGuidedCreateAcceptsAConcreteImageIdAndSkipsThePicker(t *testing.T) {
	params := map[string]any{
		"ImageSource":      "community",
		"CompShareImageId": "compshareImage-1mefk6bv35xn",
	}
	wfCtx := NewContext(params)
	wfCtx.referenceData.ImageSelection = ImageSelectionUserPinned

	skip, err := shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "a concrete id needs no picker")

	// pickImageId must take the id as given rather than re-deriving it from a
	// catalog that a FuzzySearch may have narrowed to nothing — that re-derivation
	// is the failure the id field exists to bypass.
	assert.Equal(t, "compshareImage-1mefk6bv35xn",
		pickImageId(params, map[string]any{"CompshareImageGroup": []any{}}),
		"an explicit id survives an empty catalog; it was verified by the resolver, not by this query")
}

// TestImageNameStillReachesUpstreamAsFuzzySearch keeps the reason the id field
// exists visible in a test rather than only in a comment. If this ever stops being
// true, the id is no longer the safer input and the tool description that tells
// the model to prefer it would be wrong.
func TestImageNameStillReachesUpstreamAsFuzzySearch(t *testing.T) {
	step := stepQueryImages(true)
	wfCtx := NewContext(map[string]any{
		"ImageSource": "community",
		"ImageName":   "最强AI数字人 InfiniteTalk",
	})
	args, err := step.BuildArgs(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, "最强AI数字人 InfiniteTalk", args["FuzzySearch"],
		"the name is an upstream fuzzy search, so a wording miss returns zero rows — which is why an id is preferred")
}
