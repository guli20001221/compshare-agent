package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sharedImageCreateFixture(id string) map[string]any {
	return map[string]any{
		"TotalCount": float64(1),
		"ImageSet": []any{map[string]any{
			"CompShareImageId":  id,
			"Name":              "共享训练环境",
			"ImageType":         "Custom",
			"Status":            "Reviewing",
			"Container":         "False",
			"SupportedGpuTypes": []any{"4090"},
			"Size":              float64(102400),
		}},
	}
}

func TestPlainCreateFromSharedImageUsesTheTenantVisibleCatalog(t *testing.T) {
	const imageID = "compshareImage-shared-001"
	executor := createMockExecutor()
	executor.results["DescribeCompShareSharingImages"] = sharedImageCreateFixture(imageID)

	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)
	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType":          "4090",
		"ImageSource":      imageSourceSharing,
		"CompShareImageId": imageID,
		"ChargeType":       "Postpay",
	})

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)

	listCall, ok := findExecutorCall(executor.calls, "DescribeCompShareSharingImages")
	require.True(t, ok)
	assert.Equal(t, imageID, listCall.args["CompShareImageId"])
	for _, call := range executor.calls {
		assert.NotEqual(t, "DescribeCompShareImages", call.action,
			"sharing must not silently fall back to the platform catalog")
	}

	capacityCall, ok := findExecutorCall(executor.calls, "CheckCompShareResourceCapacity")
	require.True(t, ok)
	assert.Equal(t, imageID, capacityCall.args["CompShareImageId"])
	createCall, ok := findExecutorCall(executor.calls, "CreateCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, imageID, createCall.args["CompShareImageId"])
}

func TestSharedImageSourceIsAFirstClassGuidedChoice(t *testing.T) {
	wfCtx := NewContext(map[string]any{"ImageSource": "shared"})
	form, err := buildGuidedImageSourceForm(wfCtx)
	require.NoError(t, err)

	source := form.Field("ImageSource")
	require.NotNil(t, source)
	assert.Equal(t, imageSourceSharing, source.Value)
	assert.Contains(t, optionValues(source), imageSourceSharing)

	initial := stepQueryImages(true)
	assert.Equal(t, "DescribeCompShareSharingImages", initial.ToolFunc(wfCtx))
	args, err := initial.BuildArgs(wfCtx)
	require.NoError(t, err)
	assert.Equal(t, maxCustomImageQueryLimit, args["Limit"])
	assert.NotContains(t, args, "CompShareImageId")

	wfCtx.StepResults["查询镜像"] = sharedImageCreateFixture("compshareImage-shared-001")
	assert.True(t, tenantImageInventorySelected(wfCtx))
	skip, err := shouldSkipGuidedImageFacetsStep(wfCtx)
	require.NoError(t, err)
	assert.True(t, skip, "shared inventory must not be forced through public taxonomy facets")
}
