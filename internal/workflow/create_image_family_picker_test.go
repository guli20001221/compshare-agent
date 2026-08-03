package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func familyPickerCommunityCatalog() map[string]any {
	return map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"GroupId": "group-live", "ImageName": "LiveTalking",
			"Data": []any{
				map[string]any{"CompShareImageId": "live-v2", "Name": "LiveTalking", "VersionName": "v2.2", "Status": "Available", "ImageType": "Community", "Tags": []any{"数字人"}, "SupportedGpuTypes": []any{"4090"}},
				map[string]any{"CompShareImageId": "live-v1", "Name": "LiveTalking", "VersionName": "v2.1", "Status": "Available", "ImageType": "Community", "Tags": []any{"数字人"}, "SupportedGpuTypes": []any{"4090"}},
			},
		},
		map[string]any{
			"GroupId": "group-wan", "ImageName": "Wan2.2",
			"Data": []any{
				map[string]any{"CompShareImageId": "wan-v1", "Name": "Wan2.2", "VersionName": "v1", "Status": "Available", "ImageType": "Community", "Tags": []any{"视频生成"}, "SupportedGpuTypes": []any{"4090"}},
			},
		},
	}}
}

func familyPickerTaxonomy() map[string]any {
	return map[string]any{
		"TagIndex": []any{"AIGC热门", "图像/视频生成"},
		"TagsMap": map[string]any{
			"AIGC热门":  []any{"数字人"},
			"图像/视频生成": []any{"视频生成"},
		},
	}
}

func familyPickerContext() *Context {
	return &Context{
		Params: map[string]any{"ImageSource": "community", "GpuType": "4090"},
		StepResults: map[string]map[string]any{
			"查询镜像":   familyPickerCommunityCatalog(),
			"查询镜像分类": familyPickerTaxonomy(),
		},
	}
}

func optionByLabel(t *testing.T, options []ConfirmFormOption, label string) ConfirmFormOption {
	t.Helper()
	for _, option := range options {
		if option.Label == label {
			return option
		}
	}
	t.Fatalf("option %q not found in %#v", label, options)
	return ConfirmFormOption{}
}

func TestGuidedImageFamilyPicker_CommunityChoosesFamilyBeforeConcreteVersion(t *testing.T) {
	wfCtx := familyPickerContext()

	skip, err := shouldSkipGuidedImageFamilyStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "a browse with multiple families and a multi-version family needs a family card")

	form, err := buildGuidedImageFamilyForm(wfCtx)
	require.NoError(t, err)
	familyField := form.Field("ImageFamily")
	require.NotNil(t, familyField)
	assert.Equal(t, "镜像系列", familyField.Label)
	live := optionByLabel(t, familyField.Options, "LiveTalking")
	assert.Equal(t, "2 个可选版本", live.Note)
	assert.NotContains(t, live.Label, "v2.2", "the family card must not duplicate versions as sibling cards")

	require.NoError(t, applyGuidedImageFamilyOverrides(wfCtx, map[string]string{"ImageFamily": live.Value}))
	assert.Equal(t, live.Value, paramStr(wfCtx.Params, "ImageFamily", ""))
	assert.Empty(t, paramStr(wfCtx.Params, "CompShareImageId", ""), "a multi-version family is not yet a concrete image")
	assert.False(t, paramBool(wfCtx.Params, "GuidedImageLocked", false))

	versionForm, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	assert.Contains(t, versionForm.Step.Title, "具体版本")
	versionField := versionForm.Field("ImageId")
	require.NotNil(t, versionField)
	assert.Equal(t, "版本", versionField.Label)
	assert.Equal(t, []string{"live-v2", "live-v1"}, optionValues(versionField))
	assert.Equal(t, "LiveTalking · v2.2", optionByLabel(t, versionField.Options, "LiveTalking · v2.2").Label)

	require.NoError(t, applyGuidedImageOverrides(wfCtx, map[string]string{"ImageId": "live-v1"}))
	assert.Equal(t, "live-v1", paramStr(wfCtx.Params, "CompShareImageId", ""))
	assert.True(t, paramBool(wfCtx.Params, "GuidedImageLocked", false), "only the concrete version settles the image")
}

func TestImageCategoryCountsFamiliesRatherThanCommunityVersions(t *testing.T) {
	wfCtx := familyPickerContext()
	options := imageCategoryFacetOptions(createImageTaxonomy(wfCtx), createImageCandidates(wfCtx))
	assert.Equal(t, "1 个镜像系列", optionByLabel(t, options, "AIGC热门").Note,
		"two LiveTalking versions are one user-facing image series")
}

func TestGuidedImageFamilyPicker_PlatformRowsRemainOneCardImageChoices(t *testing.T) {
	wfCtx := &Context{
		Params: map[string]any{"ImageSource": "platform", "GpuType": "4090"},
		StepResults: map[string]map[string]any{"查询镜像": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "torch", "Name": "PyTorch 2.9", "Status": "Available", "ImageType": "App"},
			map[string]any{"CompShareImageId": "ubuntu", "Name": "Ubuntu 22.04", "Status": "Available", "ImageType": "System"},
		}}},
	}

	skip, err := shouldSkipGuidedImageFamilyStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "flat platform rows are singleton families and need no extra card")

	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	assert.True(t, strings.Contains(form.Step.Title, "具体镜像"))
	assert.Equal(t, "镜像", form.Field("ImageId").Label)
}

func TestGuidedImageFamilyPicker_AgentRecommendationKeepsConcreteImageConfirmation(t *testing.T) {
	wfCtx := familyPickerContext()
	wfCtx.Params["CompShareImageId"] = "live-v2"
	wfCtx.InitialParams = map[string]any{
		"ImageSource":      "community",
		"GpuType":          "4090",
		"CompShareImageId": "live-v2",
	}
	wfCtx.referenceData.ImageSelection = ImageSelectionSuggested

	familySkip, err := shouldSkipGuidedImageFamilyStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, familySkip,
		"an Agent recommendation is already an image-level proposal, not a request to browse image families")

	imageSkip, err := shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, imageSkip,
		"the recommendation remains a user-confirmable default rather than an unseen automatic selection")

	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	assert.Contains(t, form.Step.Title, "具体镜像")
	field := form.Field("ImageId")
	require.NotNil(t, field)
	assert.Equal(t, "镜像", field.Label)
	assert.Equal(t, "live-v2", field.Value)
}

func TestGuidedImageFamilyPicker_SingletonFamilyResolvesWithoutVersionCard(t *testing.T) {
	wfCtx := familyPickerContext()
	form, err := buildGuidedImageFamilyForm(wfCtx)
	require.NoError(t, err)
	wan := optionByLabel(t, form.Field("ImageFamily").Options, "Wan2.2")

	require.NoError(t, applyGuidedImageFamilyOverrides(wfCtx, map[string]string{"ImageFamily": wan.Value}))
	assert.Equal(t, "wan-v1", paramStr(wfCtx.Params, "CompShareImageId", ""))
	assert.True(t, paramBool(wfCtx.Params, "GuidedImageLocked", false))
	skip, err := shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "a singleton family has already resolved its only concrete version")
}

func TestGuidedImageFamilySelectionIsClearedWithItsParentFilters(t *testing.T) {
	wfCtx := familyPickerContext()
	wfCtx.Params["ImageFamily"] = "community:family:group-live"
	wfCtx.Params["CompShareImageId"] = "live-v2"
	wfCtx.Params["GuidedImageLocked"] = true

	require.NoError(t, applyGuidedImageFacetsOverrides(wfCtx, map[string]string{"ImageCategory": "AIGC热门"}))
	assert.NotContains(t, wfCtx.Params, "ImageFamily")
	assert.NotContains(t, wfCtx.Params, "CompShareImageId")
	assert.NotContains(t, wfCtx.Params, "GuidedImageLocked")
}
