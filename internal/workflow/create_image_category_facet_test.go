package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// communityRow builds one community version row carrying the given tags.
func communityRow(id, name string, tags ...any) map[string]any {
	return map[string]any{
		"GroupId": "grp-" + id, "ImageName": name,
		"Data": []any{map[string]any{
			"CompShareImageId": id, "Name": name, "Status": "Available",
			"ImageType": "Community", "Tags": tags,
		}},
	}
}

func categoryTestCatalog() map[string]any {
	return map[string]any{"CompshareImageGroup": []any{
		communityRow("img-talk", "InfiniteTalk", "数字人", "视频生成"),
		communityRow("img-wan", "Wan2.2", "Wan"),
		communityRow("img-tts", "IndexTTS", "IndexTTS"),
		communityRow("img-plain", "SomeTool", "PyTorch"), // unclassified
	}}
}

func categoryTestTaxonomy() map[string]any {
	return map[string]any{
		"TagIndex": []any{"AIGC热门", "图像/视频生成", "语音/TTS生成", "LLM"},
		"TagsMap": map[string]any{
			"AIGC热门":   []any{"数字人"},
			"图像/视频生成":  []any{"Wan", "视频生成"},
			"语音/TTS生成": []any{"IndexTTS"},
			"LLM":      []any{"DeepSeek"}, // no image in this catalog
		},
	}
}

func categoryWfCtx(t *testing.T, params map[string]any) *Context {
	t.Helper()
	wfCtx := &Context{Params: params, StepResults: map[string]map[string]any{
		"查询镜像":   categoryTestCatalog(),
		"查询镜像分类": categoryTestTaxonomy(),
	}}
	return wfCtx
}

// TestCategoryFacetOffersOnlyCategoriesThatHaveImages keeps the card from offering a
// filter that can only produce an empty picker. LLM is a real platform category with
// no image in this catalog, so it must not appear.
func TestCategoryFacetOffersOnlyCategoriesThatHaveImages(t *testing.T) {
	wfCtx := categoryWfCtx(t, map[string]any{"ImageSource": "community"})
	opts := imageCategoryFacetOptions(createImageTaxonomy(wfCtx), createImageCandidates(wfCtx))
	require.NotEmpty(t, opts)

	var values []string
	for _, o := range opts {
		values = append(values, o.Value)
	}
	assert.Equal(t, []string{"", "AIGC热门", "图像/视频生成", "语音/TTS生成"}, values,
		"platform order, empty-head first, and no category without an image")

	byValue := map[string]ConfirmFormOption{}
	for _, o := range opts {
		byValue[o.Value] = o
	}
	assert.Equal(t, "1 个镜像系列", byValue["AIGC热门"].Note)
	assert.Equal(t, "2 个镜像系列", byValue["图像/视频生成"].Note,
		"InfiniteTalk (视频生成) and Wan both land here; the count is what the user will see")
}

// TestCategoryFacetReplacesTheRawTagList is the point of the change. The flat facet
// listed whatever tag strings this page of the catalog happened to carry; showing
// both in one submit would also let the user pick a contradictory pair, because this
// card submits once and the tag options cannot narrow to the chosen 用途.
func TestCategoryFacetReplacesTheRawTagList(t *testing.T) {
	wfCtx := categoryWfCtx(t, map[string]any{"ImageSource": "community"})
	form, err := buildGuidedImageFacetsForm(wfCtx)
	require.NoError(t, err)

	assert.NotNil(t, form.Field("ImageCategory"), "the 用途 facet must be offered")
	assert.Nil(t, form.Field("ImageTag"),
		"the raw tag facet is the same axis at a finer resolution; two of them in one submit can contradict")
}

// TestWithoutTheTaxonomyTheTagFacetStillWorks is the degradation contract. A failed
// DescribeCompShareImageTags must still leave the user a way to narrow — the
// classification is an improvement on the filter, never a precondition for having
// one. The raw-tag facet now lives on its own card, so the fallback is that the tag
// CARD becomes reachable, not that the 用途 card grows a second field.
func TestWithoutTheTaxonomyTheTagFacetStillWorks(t *testing.T) {
	wfCtx := &Context{
		Params:      map[string]any{"ImageSource": "community"},
		StepResults: map[string]map[string]any{"查询镜像": categoryTestCatalog()},
	}
	form, err := buildGuidedImageFacetsForm(wfCtx)
	require.NoError(t, err)
	assert.Nil(t, form.Field("ImageCategory"), "no classification, no 用途 facet")

	skip, err := shouldSkipGuidedImageTagStep(wfCtx)
	require.NoError(t, err)
	require.False(t, skip, "with no classification the raw-tag card must be reached")

	tagForm, err := buildGuidedImageTagForm(wfCtx)
	require.NoError(t, err)
	assert.NotNil(t, tagForm.Field("ImageTag"),
		"the previous raw-tag facet must survive as the fallback")
}

// TestTheTaxonomySupersedesTheTagCard is the other half: when the platform DID
// classify this catalog, the finer raw-tag card must not also be asked. Two
// resolutions of one axis, asked twice, is how the user reaches a contradictory
// pair — and the tag card exists only because the type card cannot cover it.
func TestTheTaxonomySupersedesTheTagCard(t *testing.T) {
	wfCtx := categoryWfCtx(t, map[string]any{"ImageSource": "community"})
	skip, err := shouldSkipGuidedImageTagStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "用途 already asked this question at a stabler resolution")
}

// TestPickingACategoryNarrowsTheImageList proves the facet actually filters rather
// than only rendering. An unclassified image is excluded by an active category, and
// no category at all excludes nothing.
func TestPickingACategoryNarrowsTheImageList(t *testing.T) {
	ids := func(params map[string]any) []string {
		wfCtx := categoryWfCtx(t, params)
		_, opts, _ := guidedImageFormOptions(wfCtx.Params, wfCtx.Result("查询镜像"), "", createImageTaxonomy(wfCtx), false)
		var out []string
		for _, o := range opts {
			out = append(out, o.Value)
		}
		return out
	}

	all := ids(map[string]any{"ImageSource": "community"})
	assert.Len(t, all, 4, "premise: every image is offered when nothing is filtered")

	aigc := ids(map[string]any{"ImageSource": "community", "ImageCategory": "AIGC热门"})
	assert.Equal(t, []string{"img-talk"}, aigc)

	video := ids(map[string]any{"ImageSource": "community", "ImageCategory": "图像/视频生成"})
	assert.ElementsMatch(t, []string{"img-talk", "img-wan"}, video,
		"an image with tags in two categories appears under both")
}

// TestAnUnreadableCategoryDoesNotEmptyThePicker guards the direction of the failure.
// Excluding every image on the strength of a classification we could not read turns a
// degraded read into a dead end; absence of evidence is not evidence of a mismatch.
func TestAnUnreadableCategoryDoesNotEmptyThePicker(t *testing.T) {
	wfCtx := &Context{
		Params: map[string]any{"ImageSource": "community", "ImageCategory": "AIGC热门"},
		// 查询镜像分类 absent: the fetch failed.
		StepResults: map[string]map[string]any{"查询镜像": categoryTestCatalog()},
	}
	_, opts, _ := guidedImageFormOptions(wfCtx.Params, wfCtx.Result("查询镜像"), "", createImageTaxonomy(wfCtx), false)
	assert.Len(t, opts, 4,
		"a category we cannot resolve must not filter; the user still gets a usable picker")
}

// TestSwitchingSourceDropsTheCategory — the platform catalog barely intersects the
// classification (only ComfyUI of its tags is a member) while community rows are
// fully classified, so a category carried across a source switch silently matches
// nothing.
func TestSwitchingSourceDropsTheCategory(t *testing.T) {
	wfCtx := &Context{Params: map[string]any{
		"ImageSource": "community", "ImageCategory": "AIGC热门",
	}}
	require.NoError(t, applyGuidedImageSourceOverrides(wfCtx, map[string]string{"ImageSource": "platform"}))
	assert.NotContains(t, wfCtx.Params, "ImageCategory",
		"a category derived from the previous source's catalog must not survive the switch")

	// A same-source re-confirm keeps it.
	wfCtx = &Context{Params: map[string]any{
		"ImageSource": "community", "ImageCategory": "AIGC热门",
	}}
	require.NoError(t, applyGuidedImageSourceOverrides(wfCtx, map[string]string{"ImageSource": "community"}))
	assert.Equal(t, "AIGC热门", wfCtx.Params["ImageCategory"])
}

// TestTheCommunityBrowseIsWorthCategorising pins the corpus size. At 20 groups the
// classification had almost nothing to classify; upstream tag filtering cannot narrow
// it for us (DescribeCommunityImages declares Tag []string but never parses the dotted
// Tag.N form, so every value returns the identical unfiltered result — measured
// 2026-07-22), so the fetch itself has to carry the categories.
func TestTheCommunityBrowseIsWorthCategorising(t *testing.T) {
	args := communityImageBrowseArgs("")
	assert.Equal(t, maxGuidedCommunityImageQueryLimit, args["Limit"])
	assert.Equal(t, 100, maxGuidedCommunityImageQueryLimit,
		"one page of 100 spans all 7 categories; 20 spanned almost none")
	assert.NotContains(t, args, "Tag",
		"upstream ignores Tag on this action; sending it would look like a filter and be none")
}

// unusedTaxonomyRef keeps the deployment import honest if the assertions above are
// ever reduced.
var _ = deployment.ParseImageTaxonomy
