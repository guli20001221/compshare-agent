package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveTaxonomyPayload mirrors the real DescribeCompShareImageTags response shape
// and a representative slice of its content (measured 2026-07-22: 7 categories,
// 47 tags).
func liveTaxonomyPayload() map[string]any {
	return map[string]any{
		"TagIndex": []any{"AIGC热门", "图像/视频生成", "语音/TTS生成", "LLM", "空分类"},
		"TagsMap": map[string]any{
			"AIGC热门":   []any{"数字人", "视频超分", "OCR识别"},
			"图像/视频生成":  []any{"ComfyUI", "Wan", "Qwen-Image", "Flux"},
			"语音/TTS生成": []any{"IndexTTS", "CosyVoice", "语音合成"},
			"LLM":      []any{"DeepSeek", "Qwen", "推理框架"},
			"空分类":      []any{},
		},
	}
}

// TestTaxonomyKeepsThePlatformsOwnOrder pins that the categories are presented in
// the order the platform ships them (TagIndex), not map iteration order — the card
// is rebuilt on every render and a shuffling list is unusable.
func TestTaxonomyKeepsThePlatformsOwnOrder(t *testing.T) {
	tax := ParseImageTaxonomy(liveTaxonomyPayload())
	require.True(t, tax.Available())
	assert.Equal(t, []string{"AIGC热门", "图像/视频生成", "语音/TTS生成", "LLM"}, tax.Categories(),
		"TagIndex order is the platform's display order; the empty category is dropped")
}

// TestEmptyCategoriesAreNotOffered keeps a filter from existing that can only ever
// produce an empty list.
func TestEmptyCategoriesAreNotOffered(t *testing.T) {
	tax := ParseImageTaxonomy(liveTaxonomyPayload())
	assert.NotContains(t, tax.Categories(), "空分类")
	assert.Empty(t, tax.CategoryOf("空分类"))
}

// TestCategoryLookupIgnoresCase covers a real inconsistency in the live catalog:
// community rows carry BOTH "Qwen-Image" (2 rows) and "Qwen-image" (27 rows) while
// the taxonomy lists "Qwen-Image". An exact match would drop the 27 rows out of
// 图像/视频生成 and the category count would silently under-report.
func TestCategoryLookupIgnoresCase(t *testing.T) {
	tax := ParseImageTaxonomy(liveTaxonomyPayload())
	assert.Equal(t, "图像/视频生成", tax.CategoryOf("Qwen-Image"))
	assert.Equal(t, "图像/视频生成", tax.CategoryOf("Qwen-image"),
		"the catalog is not internally consistent about casing; the lookup must be")
	assert.Equal(t, "图像/视频生成", tax.CategoryOf("  qwen-image  "))
}

// TestUnclassifiedTagsReturnNoCategory guards the honest-absence rule. Most PLATFORM
// image tags are framework names the platform never classified (live: of 10 distinct
// platform tags only ComfyUI is a taxonomy member), and all 9 platform System images
// carry no tags at all. "" must mean "not classified", and callers must not read it
// as a category.
func TestUnclassifiedTagsReturnNoCategory(t *testing.T) {
	tax := ParseImageTaxonomy(liveTaxonomyPayload())
	for _, tag := range []string{"PyTorch", "Miniconda3", "深度学习", "", "   "} {
		assert.Empty(t, tax.CategoryOf(tag), "%q is not part of the platform classification", tag)
	}
	assert.Nil(t, tax.CategoriesOf([]string{"PyTorch", "Miniconda3"}))
	assert.Nil(t, tax.CategoriesOf(nil))
}

// TestAnImageCanSitInSeveralCategories — a 数字人 image also tagged 视频生成 belongs to
// both, so the lookup must not silently pick one.
func TestAnImageCanSitInSeveralCategories(t *testing.T) {
	tax := ParseImageTaxonomy(liveTaxonomyPayload())
	got := tax.CategoriesOf([]string{"Wan", "数字人", "PyTorch"})
	assert.Equal(t, []string{"AIGC热门", "图像/视频生成"}, got,
		"both real categories, in platform order, with the unclassified tag ignored")
}

// TestAnUnreadableTaxonomyIsUnavailableNotEmpty is the degradation contract. A failed
// or malformed fetch must leave every image unclassified so the caller falls back to
// the previous facet — never produce a partial grouping that silently hides images.
func TestAnUnreadableTaxonomyIsUnavailableNotEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"nil payload", nil},
		{"empty payload", map[string]any{}},
		{"no TagsMap", map[string]any{"TagIndex": []any{"AIGC热门"}}},
		{"all categories empty", map[string]any{
			"TagIndex": []any{"A"}, "TagsMap": map[string]any{"A": []any{}}}},
		{"garbage", map[string]any{"TagsMap": "not a map"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tax := ParseImageTaxonomy(tc.payload)
			assert.False(t, tax.Available())
			assert.Nil(t, tax.Categories())
			assert.Empty(t, tax.CategoryOf("数字人"))
		})
	}

	var nilTax *ImageTaxonomy
	assert.False(t, nilTax.Available(), "a nil taxonomy must behave as unavailable, not panic")
	assert.Empty(t, nilTax.CategoryOf("数字人"))
	assert.Nil(t, nilTax.CategoriesOf([]string{"数字人"}))
}

// TestTagIndexAbsenceStillClassifies — the order is nice to have, the classification
// is the point. Losing TagIndex must not throw the grouping away.
func TestTagIndexAbsenceStillClassifies(t *testing.T) {
	tax := ParseImageTaxonomy(map[string]any{
		"TagsMap": map[string]any{"LLM": []any{"DeepSeek"}},
	})
	require.True(t, tax.Available())
	assert.Equal(t, "LLM", tax.CategoryOf("DeepSeek"))
}
