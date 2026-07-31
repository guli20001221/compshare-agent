package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func frameworkFallbackImages() map[string]any {
	return map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId": "torch-old",
			"Name":             "cuda124_torch280_py311",
			"ImageType":        "App",
			"Status":           "Available",
			"Container":        "True",
			"Tags":             []any{"pytorch，Pytorch"},
			"PubTime":          float64(100),
			"SupportedGpuTypes": []any{
				"4090",
			},
		},
		map[string]any{
			"CompShareImageId": "torch-new",
			"Name":             "cuda128_torch291_py312",
			"ImageType":        "App",
			"Status":           "Available",
			"Container":        "True",
			"Tags":             []any{"pytorch，Pytorch"},
			"PubTime":          float64(200),
			"SupportedGpuTypes": []any{
				"4090",
			},
		},
		map[string]any{
			"CompShareImageId": "tensorflow",
			"Name":             "cuda128_tf220_py312",
			"ImageType":        "App",
			"Status":           "Available",
			"Container":        "True",
			"Tags":             []any{"tensorflow，TensorFlow"},
			"PubTime":          float64(300),
		},
	}}
}

func frameworkFallbackContext(question string) *Context {
	wfCtx := NewContext(map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-bj2-03",
	})
	wfCtx.referenceData = ReferenceData{
		ZoneCatalog:     zoneCatalogFixture(),
		ImageIntentText: question,
		ImageSelection:  ImageSelectionUnset,
	}
	wfCtx.StepResults["查询镜像"] = frameworkFallbackImages()
	return wfCtx
}

func emptyCommunityImageCatalog() map[string]any {
	return map[string]any{"CompshareImageGroup": []any{}}
}

func communityComfyCatalog() map[string]any {
	return map[string]any{"CompshareImageGroup": []any{
		map[string]any{
			"ImageName": "ComfyUI 全模型整合包",
			"Data": []any{
				map[string]any{
					"CompShareImageId": "community-comfy-new",
					"Name":             "v2.0",
					"Status":           "Available",
					"Container":        "True",
				},
				map[string]any{
					"CompShareImageId": "community-comfy-old",
					"Name":             "v1.0",
					"Status":           "Available",
					"Container":        "True",
				},
			},
		},
		map[string]any{
			"ImageName": "Stable Diffusion WebUI",
			"Data": []any{
				map[string]any{
					"CompShareImageId": "community-sd-webui",
					"Name":             "v1.9",
					"Status":           "Available",
					"Container":        "True",
				},
			},
		},
	}}
}

func addPlatformComfyImage(wfCtx *Context) {
	set := wfCtx.StepResults["查询镜像"]["ImageSet"].([]any)
	wfCtx.StepResults["查询镜像"]["ImageSet"] = append(set, map[string]any{
		"CompShareImageId": "platform-comfy",
		"Name":             "ComfyUI 基础镜像",
		"ImageType":        "App",
		"Status":           "Available",
		"Container":        "True",
		"Tags":             []any{"comfyUI，ComfyUI"},
	})
}

func freezeImageCatalogIntent(t *testing.T, wfCtx *Context) imageCatalogIntentSeed {
	t.Helper()
	seed, ok := deriveImageCatalogIntentSeed(wfCtx)
	require.True(t, ok)
	wfCtx.StepResults[imageCatalogIntentStepName] = encodeImageCatalogIntentSeed(seed)
	return seed
}

func TestMissingProposalImageNameUsesLiteralLiveFrameworkInPicker(t *testing.T) {
	wfCtx := frameworkFallbackContext("在华北一C用最新pytorch为我创建一台4090")
	wfCtx.StepResults[alternateImageCatalogStepName] = emptyCommunityImageCatalog()

	skipSource, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	require.True(t, skipSource, "a literal live catalog tag already settles the platform/source axis")

	skipFacets, err := shouldSkipGuidedImageFacetsStep(wfCtx)
	require.NoError(t, err)
	require.True(t, skipFacets, "the catalog tag narrows the images without asking the user to classify them again")

	skipTag, err := shouldSkipGuidedImageTagStep(wfCtx)
	require.NoError(t, err)
	require.True(t, skipTag)

	skipPicker, err := shouldSkipGuidedImageStep(wfCtx)
	require.NoError(t, err)
	require.False(t, skipPicker, "the fallback ranks a recommendation; it never authorizes a concrete image")

	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	imageField := fieldByKey(t, form, "ImageId")
	require.Len(t, imageField.Options, 2, "only the catalog rows carrying the PyTorch fact belong in this picker")
	assert.Equal(t, "torch-new", imageField.Value, "the ordinary framework version ladder puts the latest first")
	assert.Equal(t, []string{"torch-new", "torch-old"}, optionValues(imageField))
	assert.NotContains(t, wfCtx.Params, "ImageName",
		"catalog recovery must not pretend the Agent supplied a name")
	assert.NotContains(t, wfCtx.Params, "CompShareImageId",
		"the recommendation remains unselected until the picker is submitted")

	require.NoError(t, applyGuidedImageOverrides(wfCtx, map[string]string{"ImageId": imageField.Value}))
	assert.Equal(t, "torch-new", wfCtx.Params["CompShareImageId"])
	assert.Equal(t, "cuda128_torch291_py312", wfCtx.Params["ImageName"])
	assert.Equal(t, true, wfCtx.Params["GuidedImageLocked"])
}

func TestAgentSuggestedFreeTextImageNameStillUsesLiteralCatalogFact(t *testing.T) {
	wfCtx := frameworkFallbackContext("在华北一C用最新pytorch为我创建一台4090")
	wfCtx.StepResults[alternateImageCatalogStepName] = emptyCommunityImageCatalog()
	imageSet := wfCtx.StepResults["查询镜像"]["ImageSet"].([]any)
	wfCtx.StepResults["查询镜像"]["ImageSet"] = append(imageSet, map[string]any{
		"CompShareImageId": "torch-name-old",
		"Name":             "pytorch_2.5.0_Py3.12",
		"ImageType":        "App",
		"Status":           "Available",
		"Container":        "True",
		"Tags":             []any{"pytorch，Pytorch"},
		"PubTime":          float64(50),
		"SupportedGpuTypes": []any{
			"4090",
		},
	})
	wfCtx.Params["ImageName"] = "最新pytorch"
	wfCtx.InitialParams["ImageName"] = "最新pytorch"
	wfCtx.referenceData.ImageSelection = ImageSelectionSuggested

	request, ok := currentTurnImageCatalogRequest(wfCtx)
	require.True(t, ok, "an Agent-supplied phrase is not a concrete selection")
	assert.Equal(t, "pytorch", request.Tag)

	skipSource, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	require.True(t, skipSource)

	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	imageField := fieldByKey(t, form, "ImageId")
	assert.Equal(t, "torch-new", imageField.Value)
	assert.Equal(t, []string{"torch-new", "torch-old", "torch-name-old"}, optionValues(imageField),
		"the Agent's wording must not give an older display-name match a second vote")
}

func TestExactUserPinnedFrameworkNameMayUseItsLiteralCatalogFact(t *testing.T) {
	wfCtx := frameworkFallbackContext("用 PyTorch 为我创建一台4090")
	wfCtx.Params["ImageName"] = "PyTorch"
	wfCtx.InitialParams["ImageName"] = "PyTorch"
	wfCtx.referenceData.ImageSelection = ImageSelectionUserPinned

	_, ok := currentTurnImageCatalogRequest(wfCtx)
	require.True(t, ok, "the user's exact framework name is the same catalog fact, not a broadened replacement")
}

func TestUserPinnedSpecificNameIsNotBroadenedByIncidentalFrameworkWord(t *testing.T) {
	wfCtx := frameworkFallbackContext("用 Acme PyTorch Workbench 为我创建一台4090")
	wfCtx.Params["ImageName"] = "Acme PyTorch Workbench"
	wfCtx.InitialParams["ImageName"] = "Acme PyTorch Workbench"
	wfCtx.referenceData.ImageSelection = ImageSelectionUserPinned

	_, ok := currentTurnImageCatalogRequest(wfCtx)
	assert.False(t, ok, "a specific user-pinned name must not become every image carrying the framework tag")
}

func TestGuidedRunShowsFrameworkPickerAsFirstCardWhenProposalOmittedImageName(t *testing.T) {
	executor := formMockExecutor()
	executor.results["DescribeCompShareImages"] = frameworkFallbackImages()
	executor.results["DescribeCommunityImages"] = emptyCommunityImageCatalog()

	var firstForm *ConfirmForm
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		firstForm = form
		return ConfirmResolution{Confirmed: false}
	})

	result, err := eng.runCreateTest(
		CreateInstanceGuidedDef(),
		map[string]any{"GpuType": "4090", "Zone": "cn-bj2-03"},
		WithReferenceData(ReferenceData{
			ZoneCatalog:     createZoneCatalog(),
			ImageIntentText: "在华北一C用最新pytorch为我创建一台4090",
		}),
	)

	require.NoError(t, err)
	require.False(t, result.Success, "the test client cancels the first card")
	require.NotNil(t, firstForm)
	assert.Nil(t, firstForm.Field("ImageSource"), "the source card must not reappear")
	imageField := fieldByKey(t, firstForm, "ImageId")
	assert.Equal(t, "torch-new", imageField.Value)
	assert.Equal(t, []string{"torch-new", "torch-old"}, optionValues(imageField))
	assert.Equal(t, 1, firstForm.Step.Index, "the concrete image picker is the first visible step")
}

func TestMissingProposalImageNameWithoutCatalogFrameworkStillShowsSource(t *testing.T) {
	wfCtx := frameworkFallbackContext("在华北一C为我创建一台4090")

	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "GPU and zone text alone must not be reinterpreted as an image preference")
}

func TestMissingProposalImageNameWithTwoFrameworksDoesNotGuess(t *testing.T) {
	wfCtx := frameworkFallbackContext("PyTorch 或 TensorFlow 都可以，帮我创建一台4090")

	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "two literal catalog frameworks require a user choice")
}

func TestFrameworkFallbackNeverOverridesCommunityBrowse(t *testing.T) {
	wfCtx := frameworkFallbackContext("用最新 PyTorch 为我创建一台4090")
	wfCtx.Params["ImageSource"] = "community"
	wfCtx.InitialParams["ImageSource"] = "community"

	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "community browsing remains explicit and is not replaced by a platform-framework fallback")
}

func TestComfyUIInBothCatalogsKeepsSourceChoiceVisible(t *testing.T) {
	wfCtx := frameworkFallbackContext("用 ComfyUI 为我创建一台4090")
	addPlatformComfyImage(wfCtx)
	seed := freezeImageCatalogIntent(t, wfCtx)
	require.Equal(t, "platform", seed.InitialSource)
	require.Greater(t, seed.InitialMatches, 0)
	wfCtx.StepResults[alternateImageCatalogStepName] = communityComfyCatalog()

	alternateMatches, checked := alternateImageCatalogMatchCount(wfCtx, seed)
	require.True(t, checked)
	require.Greater(t, alternateMatches, 0)

	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "a name present in both live catalogs must not silently inherit the platform default")

	form, err := buildGuidedImageSourceForm(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, "platform", fieldByKey(t, form, "ImageSource").Value,
		"the current Agent/default source may remain the recommendation, but the other live source stays visible")
}

func TestComfyUISourceChoiceCarriesIntentIntoCommunityPicker(t *testing.T) {
	wfCtx := frameworkFallbackContext("用 ComfyUI 为我创建一台4090")
	addPlatformComfyImage(wfCtx)
	freezeImageCatalogIntent(t, wfCtx)
	wfCtx.StepResults[alternateImageCatalogStepName] = communityComfyCatalog()

	require.NoError(t, applyGuidedImageSourceOverrides(wfCtx, map[string]string{"ImageSource": "community"}))
	// Simulate stepReQuerySelectedSourceImages replacing the authoritative catalog.
	wfCtx.StepResults["查询镜像"] = communityComfyCatalog()

	request, ok := currentTurnImageCatalogRequest(wfCtx)
	require.True(t, ok)
	assert.Equal(t, "community", request.Source)
	assert.True(t, strings.EqualFold("comfyui", request.Name))

	form, err := buildGuidedImageForm(wfCtx)
	require.NoError(t, err)
	imageField := fieldByKey(t, form, "ImageId")
	assert.Equal(t, []string{"community-comfy-new", "community-comfy-old"}, optionValues(imageField))
	assert.NotContains(t, wfCtx.Params, "ImageName",
		"carrying the intent across sources must not fabricate a selected image name")
	assert.NotContains(t, wfCtx.Params, "CompShareImageId",
		"the concrete community image still belongs to the user's picker submission")
}

func TestUserPinnedBareComfyUINameDoesNotSettleSourceWhenBothCatalogsMatch(t *testing.T) {
	wfCtx := frameworkFallbackContext("用 ComfyUI 为我创建一台4090")
	addPlatformComfyImage(wfCtx)
	wfCtx.Params["ImageName"] = "ComfyUI"
	wfCtx.InitialParams["ImageName"] = "ComfyUI"
	wfCtx.referenceData.ImageSelection = ImageSelectionUserPinned
	freezeImageCatalogIntent(t, wfCtx)
	wfCtx.StepResults[alternateImageCatalogStepName] = communityComfyCatalog()

	require.True(t, imageUserSettled(wfCtx), "the user did settle the image phrase")
	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "settling a cross-catalog name is not the same as settling its source")
}

func TestExplicitImageSourceNeedsNoAlternateCatalogProbe(t *testing.T) {
	wfCtx := frameworkFallbackContext("用平台镜像 ComfyUI 为我创建一台4090")
	addPlatformComfyImage(wfCtx)
	wfCtx.referenceData.ImageSourceUserPinned = true
	freezeImageCatalogIntent(t, wfCtx)

	probe := stepQueryAlternateImageCatalog()
	skipProbe, err := probe.SkipIf(wfCtx)
	require.NoError(t, err)
	assert.True(t, skipProbe)

	skipSource, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skipSource, "an explicit source remains authoritative")
}

func TestAlternateCatalogFailureDoesNotProvePlatformUnique(t *testing.T) {
	wfCtx := frameworkFallbackContext("用最新 PyTorch 为我创建一台4090")
	freezeImageCatalogIntent(t, wfCtx)
	// No alternate StepResult represents an optional probe failure/unavailability.
	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip, "unknown alternate state must keep the source choice visible")
}

func TestCommunityFuzzyZeroForFreeTextKeepsSourceChoiceVisible(t *testing.T) {
	wfCtx := frameworkFallbackContext("用最强 ComfyUI 工作流整合包为我创建一台4090")
	addPlatformComfyImage(wfCtx)
	wfCtx.Params["ImageName"] = "最强 ComfyUI 工作流整合包"
	wfCtx.InitialParams["ImageName"] = "最强 ComfyUI 工作流整合包"
	wfCtx.referenceData.ImageSelection = ImageSelectionUserPinned
	seed := freezeImageCatalogIntent(t, wfCtx)
	require.Equal(t, "最强 ComfyUI 工作流整合包", seed.Query,
		"a specific user/Agent phrase must not be broadened into a framework")
	require.False(t, seed.Structured)
	require.Greater(t, seed.InitialMatches, 0,
		"the platform catalog relates the phrase through the shared ComfyUI token")

	// This is the real upstream failure mode: whole-string FuzzySearch returned
	// zero even though a differently-worded ComfyUI family exists.
	wfCtx.StepResults[alternateImageCatalogStepName] = emptyCommunityImageCatalog()
	matches, checked := alternateImageCatalogMatchCount(wfCtx, seed)
	assert.Zero(t, matches)
	assert.False(t, checked, "one wording miss is unknown, never a directory-level zero")

	skipSource, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skipSource)
}

func TestStructuredCatalogFactCanSettlePlatformSourceAfterLiteralCommunityMiss(t *testing.T) {
	wfCtx := frameworkFallbackContext("在华北一C用最新pytorch为我创建一台4090")
	seed := freezeImageCatalogIntent(t, wfCtx)
	require.True(t, seed.Structured)
	require.Greater(t, seed.InitialMatches, 0)
	wfCtx.StepResults[alternateImageCatalogStepName] = emptyCommunityImageCatalog()

	matches, checked := alternateImageCatalogMatchCount(wfCtx, seed)
	require.True(t, checked)
	assert.Zero(t, matches)
	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "a literal live framework fact keeps the direct platform flow")
}

func TestSDWebUINameCollisionIsDetectedWithoutAliasTable(t *testing.T) {
	wfCtx := frameworkFallbackContext("用 SD-webUI 为我创建一台4090")
	set := wfCtx.StepResults["查询镜像"]["ImageSet"].([]any)
	wfCtx.StepResults["查询镜像"]["ImageSet"] = append(set, map[string]any{
		"CompShareImageId": "platform-sd-webui",
		"Name":             "SD-WebUI 基础镜像",
		"ImageType":        "App",
		"Status":           "Available",
		"Container":        "True",
	})
	wfCtx.Params["ImageName"] = "SD-webUI"
	wfCtx.InitialParams["ImageName"] = "SD-webUI"
	wfCtx.referenceData.ImageSelection = ImageSelectionUserPinned
	seed := freezeImageCatalogIntent(t, wfCtx)
	wfCtx.StepResults[alternateImageCatalogStepName] = communityComfyCatalog()

	matches, checked := alternateImageCatalogMatchCount(wfCtx, seed)
	require.True(t, checked)
	require.Greater(t, matches, 0,
		"generic name overlap should relate SD-webUI to the live family Stable Diffusion WebUI")

	skip, err := shouldSkipGuidedImageSourceStep(wfCtx)
	require.NoError(t, err)
	assert.False(t, skip)
}
