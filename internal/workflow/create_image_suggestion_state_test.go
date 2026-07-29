package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suggestedImageCtx builds a guided-create context whose image the engine would have
// classified as an Agent SUGGESTION — the user named no image, the Agent proposed a
// concrete id — set the way the engine threads it, on ReferenceData.
func suggestedImageCtx(params map[string]any) *Context {
	wfCtx := NewContext(params)
	wfCtx.referenceData.ImageSelection = ImageSelectionSuggested
	return wfCtx
}

// TestAnAgentSuggestedImageStillRunsThePicker is the reported bug: "在上海二A创建一台
// 5090实例" named no image, the Agent pinned a concrete id, and CompShareImageId != ""
// alone skipped the only card that let the user choose. The suggestion must PRESELECT
// on the picker, never seal itself unseen.
//
// The pair is the non-vacuous half: the SAME id, only the provenance differs. If the
// skip logic ignored provenance (the old CompShareImageId != "" test) both would skip
// and this assertion pair could not tell them apart.
func TestAnAgentSuggestedImageStillRunsThePicker(t *testing.T) {
	suggested := suggestedImageCtx(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
	})
	skip, err := shouldSkipGuidedImageStep(suggested)
	require.NoError(t, err)
	assert.False(t, skip, "an Agent SUGGESTION must not skip the picker — the user never chose it")

	pinned := NewContext(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
	})
	pinned.referenceData.ImageSelection = ImageSelectionUserPinned
	skip, err = shouldSkipGuidedImageStep(pinned)
	require.NoError(t, err)
	assert.True(t, skip, "the SAME id, user-pinned, needs no picker — provenance is the only difference")
}

// TestTheQueryFetchesTheWholeCatalogForASuggestion proves the other half: a picker
// that shows is still a fake choice if the query was narrowed to the suggested id,
// because upstream then returns ONE row. For a suggestion the query must NOT carry
// CompShareImageId (the whole catalog, suggestion preselected from within it); for a
// user-settled id it must (the picker is skipped, the by-id row feeds capacity/price).
func TestTheQueryFetchesTheWholeCatalogForASuggestion(t *testing.T) {
	step := stepQueryImages(true)

	suggested := suggestedImageCtx(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
	})
	args, err := step.BuildArgs(suggested)
	require.NoError(t, err)
	assert.NotContains(t, args, "CompShareImageId",
		"a suggestion must query the whole catalog so the picker can offer alternatives, not one row")
	assert.Equal(t, maxPlatformImageQueryLimit, args["Limit"])

	pinned := NewContext(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
	})
	pinned.referenceData.ImageSelection = ImageSelectionUserPinned
	args, err = step.BuildArgs(pinned)
	require.NoError(t, err)
	assert.Equal(t, "compshareImage-agentpick", args["CompShareImageId"],
		"a user-settled id is queried directly; the picker it feeds is skipped")
}

// TestABareNameSuggestionIsBrowsedNotDeadEnded guards the pure bug shape end to end at
// the query: the Agent suggested only an id (no ImageName), so the query must fall all
// the way through to the whole platform catalog — no id filter AND no name filter.
func TestABareNameSuggestionIsBrowsedNotDeadEnded(t *testing.T) {
	step := stepQueryImages(true)
	suggested := suggestedImageCtx(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
		// The Agent may also have inferred a name; it is still only a suggestion, so
		// it must not narrow the browse either.
		"ImageName": "PyTorch",
	})
	args, err := step.BuildArgs(suggested)
	require.NoError(t, err)
	assert.NotContains(t, args, "CompShareImageId")
	assert.NotContains(t, args, "Name",
		"an Agent-inferred name is a suggestion too — it must not narrow the catalog the user browses")
}

// TestPickingOnTheCardLocksTheSuggestionIn covers the transition. The engine-set state
// stays Suggested across re-runs (a later-card edit re-runs the flow), so a card pick
// needs its own signal to settle the image — otherwise the re-run would see Suggested
// and re-open the picker as if the pick were a fresh suggestion.
func TestPickingOnTheCardLocksTheSuggestionIn(t *testing.T) {
	wfCtx := suggestedImageCtx(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
	})
	skip, err := shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	require.False(t, skip, "before the pick the suggestion shows the picker")

	require.NoError(t, applyGuidedImageOverrides(wfCtx, map[string]string{"ImageId": "compshareImage-userpick"}))
	assert.Equal(t, "compshareImage-userpick", paramStr(wfCtx.Params, "CompShareImageId", ""),
		"the pick replaces the suggestion")

	skip, err = shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "a card pick settles the image; the still-Suggested re-run must not re-ask")

	// An edit that invalidates the image clears the pick AND its lock, so the picker
	// returns for a clean re-browse of the new source.
	require.NoError(t, applyGuidedImageSourceOverrides(wfCtx, map[string]string{"ImageSource": "community"}))
	assert.Empty(t, paramStr(wfCtx.Params, "CompShareImageId", ""), "a source change clears the pinned image")
	assert.False(t, paramBool(wfCtx.Params, "GuidedImageLocked", false), "…and clears the lock it set")
	skip, err = shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "changing the source clears the pick, so the picker must run again")
}

// TestASuggestionSkipsTheAxesItAlreadyAnswersButNotThePicker splits what used to
// be one flag into the two questions it was conflating.
//
// The cards ONCE all showed for a suggestion, on the reasoning that a suggestion
// is not settlement. That is right about the image and wrong about the axes: after
// 「推荐一个做数字人的镜像」→「用该镜像开一台」, the assistant's named community image
// already answers 平台还是社区 and 想跑哪一类, and re-asking them (measured on the
// real stack 2026-07-29, the flow re-opened at step 1) makes the user re-derive
// facts the suggestion fixed. The picker is the one card that asks about the image
// itself, so it stays — TestAnAgentSuggestedImageStillRunsThePicker is its gate,
// and the original bug ("an Agent-pinned id seals unseen") remains closed.
func TestASuggestionSkipsTheAxesItAlreadyAnswersButNotThePicker(t *testing.T) {
	suggested := suggestedImageCtx(map[string]any{
		"ImageSource":      "community",
		"CompShareImageId": "compshareImage-agentpick",
	})

	for _, tc := range []struct {
		name string
		skip func(*Context) (bool, error)
	}{
		{"source", shouldSkipGuidedImageSourceStep},
		{"facets", shouldSkipGuidedImageFacetsStep},
		{"tag", shouldSkipGuidedImageTagStep},
	} {
		skip, err := tc.skip(suggested)
		require.NoError(t, err)
		assert.True(t, skip, "the %s card asks something the suggested image already answers", tc.name)
	}

	skipPicker, err := shouldSkipGuidedImageStep(suggested)
	require.NoError(t, err)
	assert.False(t, skipPicker, "the image itself is still the user's to confirm")
}

// The skip is gated on the proposal NAMING the source, not on the default. A
// community image carried under a defaulted "platform" would send the picker to
// the wrong catalog, where the suggested id does not exist — the user would face
// a browse with nothing preselected and no way to see why.
func TestASuggestionWithoutANamedSourceStillAsks(t *testing.T) {
	suggested := suggestedImageCtx(map[string]any{
		"CompShareImageId": "compshareImage-agentpick",
	})

	skip, err := shouldSkipGuidedImageSourceStep(suggested)
	require.NoError(t, err)
	assert.False(t, skip, "no source in the request means the source is a default, not an answer")
}

// A user-pinned image skips the browse cards, unchanged by any of the above.
func TestAUserPinnedImageSkipsTheBrowseCards(t *testing.T) {
	pinned := NewContext(map[string]any{
		"ImageSource":      "platform",
		"CompShareImageId": "compshareImage-agentpick",
	})
	pinned.referenceData.ImageSelection = ImageSelectionUserPinned

	skipSource, err := shouldSkipGuidedImageSourceStep(pinned)
	require.NoError(t, err)
	assert.True(t, skipSource, "a user-pinned image needs no source browsing")
}
