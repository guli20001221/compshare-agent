package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func customImageCreateFixture(id string) map[string]any {
	return map[string]any{
		"TotalCount": float64(1),
		"ImageSet": []any{map[string]any{
			"CompShareImageId":  id,
			"Name":              "我的自制训练环境",
			"ImageType":         "Custom",
			"Status":            "Available",
			"Container":         "False",
			"SupportedGpuTypes": []any{"4090"},
			"Size":              float64(102400),
		}},
	}
}

func TestCustomImageSourceUsesTenantScopedCatalogQueries(t *testing.T) {
	params := map[string]any{
		"ImageSource":      imageSourceCustom,
		"CompShareImageId": "compshareImage-custom-001",
	}
	wfCtx := NewContext(params)

	initial := stepQueryImages(true)
	assert.Equal(t, "DescribeCompShareCustomImages", initial.ToolFunc(wfCtx))
	args, err := initial.BuildArgs(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, maxCustomImageQueryLimit, args["Limit"])
	assert.NotContains(t, args, "CompShareImageId",
		"custom IDs are verified through the tenant-scoped catalog snapshot, never a point query")

	requery := stepReQuerySelectedSourceImages()
	assert.Equal(t, "DescribeCompShareCustomImages", requery.ToolFunc(wfCtx))
	args, err = requery.BuildArgs(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, maxCustomImageQueryLimit, args["Limit"])
	assert.NotContains(t, args, "CompShareImageId")

	// Self-made images are an account inventory, not a third public search result
	// inferred from a user phrase. A user can select the source on the source card,
	// or the Agent can carry an exact ID it already verified.
	wfCtx.StepResults[imageCatalogIntentStepName] = map[string]any{
		"Query":         "训练环境",
		"InitialSource": imageSourceCustom,
	}
	skip, err := stepQueryAlternateImageCatalog().SkipIf(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip)
}

func TestPlainCreateFromCustomImagePreflightsAndCreatesTheSameImage(t *testing.T) {
	const imageID = "compshareImage-custom-001"
	executor := createMockExecutor()
	executor.results["DescribeCompShareCustomImages"] = customImageCreateFixture(imageID)

	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)
	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType":          "4090",
		"ImageSource":      imageSourceCustom,
		"CompShareImageId": imageID,
		"ChargeType":       "Postpay",
	})

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)

	listCall, ok := findExecutorCall(executor.calls, "DescribeCompShareCustomImages")
	require.True(t, ok, "the workflow must read the custom-image catalog")
	assert.Equal(t, maxCustomImageQueryLimit, listCall.args["Limit"])
	assert.NotContains(t, listCall.args, "CompShareImageId")
	for _, call := range executor.calls {
		assert.NotEqual(t, "DescribeCompShareImages", call.action,
			"custom source must not silently fall back to the platform catalog")
	}

	capacityCall, ok := findExecutorCall(executor.calls, "CheckCompShareResourceCapacity")
	require.True(t, ok, "custom image still takes the normal preflight")
	assert.Equal(t, imageID, capacityCall.args["CompShareImageId"])

	createCall, ok := findExecutorCall(executor.calls, "CreateCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, imageID, createCall.args["CompShareImageId"],
		"the exact live catalog image checked before confirmation is the one created")
}

func TestGuidedCreateCustomSourceShowsOnlyTheCustomCatalog(t *testing.T) {
	executor := formMockExecutor()
	executor.results["DescribeCompShareCustomImages"] = map[string]any{
		"TotalCount": float64(2),
		"ImageSet": []any{
			map[string]any{
				"CompShareImageId":  "custom-001",
				"Name":              "我的 PyTorch 环境",
				"ImageType":         "Custom",
				"Status":            "Available",
				"SupportedGpuTypes": []any{"4090"},
			},
			map[string]any{
				"CompShareImageId":  "custom-002",
				"Name":              "我的 ComfyUI 环境",
				"ImageType":         "Custom",
				"Status":            "Available",
				"SupportedGpuTypes": []any{"4090"},
			},
		},
	}

	var options []string
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if source := form.Field("ImageSource"); source != nil {
			assert.Contains(t, optionValues(source), imageSourceCustom)
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"ImageSource": imageSourceCustom}}
		}
		if image := form.Field("ImageId"); image != nil {
			options = optionValues(image)
			return ConfirmResolution{Confirmed: false}
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, []string{"custom-001", "custom-002"}, options)
	assert.NotContains(t, options, "img-001")
	assert.NotContains(t, options, "cimg-sd-001")

	customCall, ok := findExecutorCall(executor.calls, "DescribeCompShareCustomImages")
	require.True(t, ok)
	assert.Equal(t, maxCustomImageQueryLimit, customCall.args["Limit"])
	assert.NotContains(t, customCall.args, "CompShareImageId")
}
