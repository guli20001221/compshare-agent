package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file holds the image-facet acceptance gates for the ImagePurpose→facet
// convergence. They pin the invariants the lead named: an unselected tag never
// excludes an image, an absent-tag catalog shows no tag facet at all (never a silent
// default), a selected tag excludes ONLY genuine non-members, and an explicitly
// named/identified image is resolved exactly, never swapped for a similar one.

func facetGateOptionValues(opts []ConfirmFormOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Value
	}
	return out
}

// facetGateImages: one App image carrying a REAL "深度学习" catalog tag and one bare
// System image with NO tags — the honest-absence case. Chinese tags and real
// ImageType mirror the production DescribeCompShareImages shape.
func facetGateImages() map[string]any {
	return map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-dl", "Name": "PyTorch 2.9", "ImageType": "App", "Status": "Available",
			"SupportedGpuTypes": []any{"4090"}, "Tags": []any{"深度学习"}},
		map[string]any{"CompShareImageId": "img-plain", "Name": "Ubuntu 22.04", "ImageType": "System", "Status": "Available",
			"SupportedGpuTypes": []any{"4090"}},
	}}
}

// Gate #3: with NO ImageTag selected, every viable image is offered — the untagged
// image is present. A missing/empty tag facet is "no filter", never "match nothing".
func TestImageFacetsGate_NoTagSelectedExcludesNothing(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询镜像"] = facetGateImages()
	_, opts := guidedImageFormOptions(wfCtx.Params, wfCtx.Result("查询镜像"), "4090")
	assert.ElementsMatch(t, []string{"img-dl", "img-plain"}, facetGateOptionValues(opts),
		"an unselected tag facet must never exclude an image, including the untagged one")
}

// Gate #3 (facet omission): when NO candidate carries a tag, the ImageTag facet is
// OMITTED entirely — never shown with a default value that would silently filter and
// never blocking creation.
func TestImageFacetsGate_TagFacetOmittedWhenCatalogHasNoTags(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-plain", "Name": "Ubuntu 22.04", "ImageType": "System", "Status": "Available"},
		map[string]any{"CompShareImageId": "img-app", "Name": "PyTorch", "ImageType": "App", "Status": "Available"},
	}}
	form, err := buildGuidedImageFacetsForm(wfCtx)
	require.NoError(t, err)
	assert.Nil(t, form.Field("ImageTag"), "no candidate has a tag → no tag facet, never a silent default filter")
}

// Gate #2/#3 boundary: a selected tag excludes an image ONLY because that image
// GENUINELY lacks the tag (exact membership on real Tags) — absence is not a match.
func TestImageFacetsGate_TagSelectedExcludesOnlyGenuineNonMembers(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "ImageTag": "深度学习"})
	wfCtx.StepResults["查询镜像"] = facetGateImages()
	_, opts := guidedImageFormOptions(wfCtx.Params, wfCtx.Result("查询镜像"), "4090")
	assert.Equal(t, []string{"img-dl"}, facetGateOptionValues(opts),
		"only the image carrying the real tag survives; the untagged image is excluded because it genuinely lacks the tag")
}

// Gate #6: an explicit, catalog-verified CompShareImageId resolves to EXACTLY that
// image — never silently replaced by a similar one.
func TestImageFacetsGate_ExactIdNotReplacedBySimilar(t *testing.T) {
	wfCtx := formWfCtx(t, map[string]any{"GpuType": "4090", "CompShareImageId": "img-plain", "ImageName": "Ubuntu 22.04"})
	wfCtx.StepResults["查询镜像"] = facetGateImages()
	sel := resolveSelectedImage(wfCtx.Params, createImageCatalog(wfCtx))
	assert.Equal(t, "img-plain", sel.ID,
		"an explicit catalog-verified id must resolve to exactly itself, not a similar image")
	assert.Equal(t, "Ubuntu 22.04", sel.Name)
}
