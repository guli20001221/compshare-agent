package workflow

import (
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	exactRecommendedImageID   = "compshareImage-1pl06yxr5lvm"
	exactRecommendedImageName = "FaceFusion 3.5.1 / 3.6.1 全模型离线版（TensorRT 加速）"
)

func exactRecommendedImageSnapshot() *deployment.ImageCatalogSnapshot {
	return deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{{
		ID:                exactRecommendedImageID,
		Name:              exactRecommendedImageName,
		VersionName:       "v3.6.1",
		Source:            "community",
		Status:            "Available",
		Container:         true,
		SupportedGPUTypes: []string{"4090"},
		SizeMB:            102400,
	}})
}

func unrelatedCommunityPage() map[string]any {
	return map[string]any{"CompshareImageGroup": []any{
		map[string]any{"ImageName": "无关镜像", "Data": []any{
			map[string]any{
				"CompShareImageId":  "compshareImage-unrelated",
				"Name":              "v1",
				"Status":            "Available",
				"Container":         "True",
				"SupportedGpuTypes": []any{"4090"},
				"Size":              float64(102400),
			},
		}},
	}}
}

func exactRecommendedReference(selection ImageSelectionState) ReferenceData {
	return ReferenceData{
		ZoneCatalog:    createZoneCatalog(),
		ImageCatalog:   exactRecommendedImageSnapshot(),
		ImageSelection: selection,
	}
}

func TestCreateImageResultMergesOnlyTheVerifiedExactImage(t *testing.T) {
	raw := unrelatedCommunityPage()
	wfCtx := NewContext(map[string]any{
		"ImageSource":      "community",
		"CompShareImageId": exactRecommendedImageID,
		"ImageName":        "陈旧的错误名称",
	})
	wfCtx.referenceData = exactRecommendedReference(ImageSelectionSuggested)
	wfCtx.StepResults["查询镜像"] = raw

	merged := createImageResult(wfCtx)

	require.NotNil(t, imageMapByID(merged, exactRecommendedImageID))
	assert.Nil(t, imageMapByID(raw, exactRecommendedImageID), "不能改写工具的原始查询结果")
	selected := selectCreateImage(wfCtx)
	assert.Equal(t, exactRecommendedImageID, selected.ID)
	assert.Equal(t, exactRecommendedImageName, selected.Name)

	noProof := NewContext(wfCtx.Params)
	noProof.StepResults["查询镜像"] = raw
	assert.Empty(t, selectCreateImage(noProof).ID,
		"精确 ID 不在查询结果和实时核验快照中时必须失败，不能按名字换成别的镜像")
}

func TestPlainCreateUsesTheExactGroupedCommunityPointResult(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeCommunityImages"] = map[string]any{
		"TotalCount": float64(1),
		"CompshareImageGroup": []any{map[string]any{
			"GroupId":   "qYl0zvqlo03V",
			"ImageName": exactRecommendedImageName,
			"Status":    "Available",
			"Data": []any{map[string]any{
				"CompShareImageId":  exactRecommendedImageID,
				"Name":              exactRecommendedImageName,
				"VersionName":       "v3.6.1",
				"ImageType":         "Community",
				"Status":            "Available",
				"Container":         "True",
				"SupportedGpuTypes": []any{"4090"},
				"Size":              float64(102400),
			}},
		}},
	}
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType":           "4090",
		"ImageSource":       "community",
		"CompShareImageId":  exactRecommendedImageID,
		"ChargeType":        "Postpay",
		"GuidedRecommended": true,
	}, WithReferenceData(exactRecommendedReference(ImageSelectionSuggested)))

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	createCall, ok := findExecutorCall(executor.calls, "CreateCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, exactRecommendedImageID, createCall.args["CompShareImageId"])
	assert.NotEqual(t, "compshareImage-unrelated", createCall.args["CompShareImageId"])
}

func TestPlainCreateUsesPlatformPointResultEvenWhenTotalCountIsZero(t *testing.T) {
	const (
		imageID   = "compshareImage-1e3udifakfm9"
		imageName = "Windows-nvidia 2022 64位"
	)
	executor := createMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{
		"TotalCount": float64(0),
		"ImageSet": []any{map[string]any{
			"CompShareImageId":  imageID,
			"Name":              imageName,
			"ImageType":         "System",
			"Status":            "Available",
			"Container":         "False",
			"SupportedGpuTypes": []any{"4090"},
			"Size":              float64(40960),
		}},
	}
	ref := ReferenceData{
		ZoneCatalog: createZoneCatalog(),
		ImageCatalog: deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{{
			ID: imageID, Name: imageName, Source: "platform", ImageType: "System",
			Status: "Available", SupportedGPUTypes: []string{"4090"}, SizeMB: 40960,
		}}),
		ImageSelection: ImageSelectionSuggested,
	}
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType":           "4090",
		"ImageSource":       "platform",
		"CompShareImageId":  imageID,
		"ChargeType":        "Postpay",
		"GuidedRecommended": true,
	}, WithReferenceData(ref))

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	queryCall, ok := findExecutorCall(executor.calls, "DescribeCompShareImages")
	require.True(t, ok)
	assert.Equal(t, imageID, queryCall.args["CompShareImageId"],
		"平台精确查询应保留，TotalCount=0 不代表 ImageSet 为空")
	createCall, ok := findExecutorCall(executor.calls, "CreateCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, imageID, createCall.args["CompShareImageId"])
}

func TestGuidedCreatePreselectsAndCreatesTheExactRecommendedImageOutsideBrowsePage(t *testing.T) {
	executor := formMockExecutor()
	executor.results["DescribeCommunityImages"] = unrelatedCommunityPage()
	var (
		imageCards int
		imageValue string
	)
	eng := NewEngine(executor, nil, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		require.NotNil(t, form)
		if field := form.Field("ImageId"); field != nil && field.Editable {
			imageCards++
			imageValue = field.Value
			require.NotEmpty(t, field.Options)
			require.Equal(t, exactRecommendedImageID, field.Options[0].Value,
				"推荐镜像必须排在卡片首位，即使它不在普通浏览页中")
		}
		return ConfirmResolution{Confirmed: true}
	})
	ref := exactRecommendedReference(ImageSelectionSuggested)
	ref.ChargeTypeUserPinned = true

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{
		"GpuType":           "4090",
		"GuidedGpuLocked":   true,
		"Zone":              "cn-wlcb-01",
		"GuidedZoneLocked":  true,
		"Gpu":               float64(1),
		"Cpu":               float64(16),
		"Memory":            float64(65536),
		"GuidedRecommended": true,
		"ImageSource":       "community",
		"CompShareImageId":  exactRecommendedImageID,
		"ChargeType":        "Postpay",
	}, WithReferenceData(ref))

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	assert.Equal(t, 1, imageCards)
	assert.Equal(t, exactRecommendedImageID, imageValue)
	createCall, ok := findExecutorCall(executor.calls, "CreateCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, exactRecommendedImageID, createCall.args["CompShareImageId"])
}
