package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The facet cards count image FAMILIES, so that a category holding six versions of
// one series reads as one thing to choose rather than six. That number needs a noun,
// and the noun is not a constant: a source that publishes no family relation gives
// one singleton family per image, where the family count IS the image count and
// "系列" would name a hierarchy the catalog does not have — and promise a family card
// that shouldSkipGuidedImageFamilyStep will skip.
//
// Both directions are pinned here because each alone is satisfiable by a constant.
// Every OTHER facet-count assertion in this package runs on a flat fixture, so a
// hardcoded "个镜像" passes all of them; only the grouped half below fails. And a
// hardcoded "个镜像系列" passes the grouped half, which is how it shipped.

func TestAFlatCatalogCountsImagesAndSaysSo(t *testing.T) {
	// candidateWfCtx pins ImageSource=platform. Platform rows carry no group, so
	// FamilyKey falls back to the image id and every row is its own family.
	wfCtx := candidateWfCtx(map[string]any{})
	set := createImageCandidates(wfCtx)
	entries := candidateEntries(set.snap, set.base)
	require.NotEmpty(t, entries)
	require.False(t, imageCandidatesGroupIntoFamilies(entries),
		"premise: no platform row shares a family with another")

	opts := imageTypeFacetOptions(set)
	require.NotEmpty(t, opts)
	for _, opt := range opts {
		if opt.Value == "" {
			continue // 全部类型
		}
		assert.NotContains(t, opt.Note, "系列",
			"a flat catalog has no series; the count is a plain image count")
		assert.Contains(t, opt.Note, "个镜像")
	}

	form, err := buildGuidedImageFacetsForm(wfCtx)
	require.NoError(t, err)
	assert.NotContains(t, form.Step.Description, "镜像系列",
		"the next card on a flat source is the concrete-image picker, not a family picker")
}

func TestAGroupedCatalogCountsFamiliesAndSaysSo(t *testing.T) {
	// familyPickerCommunityCatalog: LiveTalking has two versions, Wan2.2 has one —
	// three rows, two families.
	wfCtx := familyPickerContext()
	set := createImageCandidates(wfCtx)
	entries := candidateEntries(set.snap, set.base)
	require.Len(t, entries, 3, "premise: three concrete rows")
	require.True(t, imageCandidatesGroupIntoFamilies(entries),
		"premise: two of them are versions of one family")

	tagOpts := imageTagFacetOptions(set)
	digitalHuman := optionByLabel(t, tagOpts, "数字人")
	// 1, not 2: both LiveTalking versions carry 数字人 and they are one choice. The
	// number and the noun are the same fact, which is why they are asserted together
	// — "2 个镜像系列" and "1 个镜像" are each half right and wholly wrong.
	assert.Equal(t, "1 个镜像系列", digitalHuman.Note)

	categoryOpts := imageCategoryFacetOptions(createImageTaxonomy(wfCtx), set)
	aigc := optionByLabel(t, categoryOpts, "AIGC热门")
	assert.Equal(t, "1 个镜像系列", aigc.Note)

	form, err := buildGuidedImageFacetsForm(wfCtx)
	require.NoError(t, err)
	assert.Contains(t, form.Step.Description, "镜像系列")
}
