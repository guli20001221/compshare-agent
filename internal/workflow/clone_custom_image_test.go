package workflow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cloneCustomImageReferenceData(status, sourceZone string, sourceZoneID uint32) ReferenceData {
	return ReferenceData{
		ImageCatalog: deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{{
			ID: "cimg-source", Name: "training-base", Source: "custom",
			ImageType: "Custom", Status: status, Zone: sourceZone, ZoneID: sourceZoneID,
		}}),
		ZoneCatalog: deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
			{Placement: deployment.ZonePlacement{Zone: "cn-wlcb-01", Region: "cn-wlcb", ZoneID: 10027}, DisplayName: "华北二A"},
			{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 8200}, DisplayName: "上海二B"},
		}),
	}
}

func cloneCustomImageExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"SyncCompShareCustomImage": {
			"SuccessCount": float64(1), "FailedCount": float64(0),
			"Results": []any{map[string]any{
				"TargetZoneId": float64(8200), "CompShareImageId": "cimg-cloned",
				"RetCode": float64(0), "Message": "OK",
			}},
		},
		"DescribeCompShareCustomImageSyncDetail": {"Progress": "12"},
	}}
}

func TestCloneCustomImage_HappyPathSealsOneVerifiedTarget(t *testing.T) {
	executor := cloneCustomImageExecutor()
	var preview map[string]any
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		assert.Equal(t, "CloneCustomImageWorkflow", action)
		preview = args
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-sh2-02",
		"TargetImageName": "training-copy", "TargetImageDescription": "for Shanghai",
	}, WithReferenceData(cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)))

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	assert.Equal(t, "training-base", preview["SourceImageName"])
	assert.Equal(t, "上海二B", preview["TargetZoneName"])
	assert.Equal(t, "cimg-cloned", result.Data["CompShareImageId"])

	syncCall, ok := findExecutorCall(executor.calls, "SyncCompShareCustomImage")
	require.True(t, ok)
	assert.Equal(t, "cimg-source", syncCall.args["SourceCompShareImageId"])
	assert.Equal(t, "training-copy", syncCall.args["TargetImageName"])
	assert.Equal(t, []uint32{8200}, syncCall.args["TargetZoneIds"])
	assert.NotContains(t, syncCall.args, "Zone")

	progressCall, ok := findExecutorCall(executor.calls, "DescribeCompShareCustomImageSyncDetail")
	require.True(t, ok)
	assert.Equal(t, "cimg-cloned", progressCall.args["CompShareImageId"])
	assert.Equal(t, uint32(8200), progressCall.args["zone_id"])
}

func TestCloneCustomImage_RejectsSameZoneBeforeConfirmation(t *testing.T) {
	executor := cloneCustomImageExecutor()
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("same-zone clone must fail before confirmation")
		return false
	}, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-wlcb-01", "TargetImageName": "invalid-copy",
	}, WithReferenceData(cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "不能与源镜像所在可用区相同")
	assert.Empty(t, executor.calls)
}

func TestCloneCustomImage_RejectsLiveDisabledDestinationBeforeConfirmation(t *testing.T) {
	executor := cloneCustomImageExecutor()
	ref := cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)
	ref.ZoneCatalog = deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{{
		Placement:        deployment.ZonePlacement{Zone: "us-den-01", Region: "us-den", ZoneID: 10049, IsPod: true},
		DisplayName:      "丹佛",
		DisableImageSync: true,
	}})
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("上游标记为禁用的目标区必须在确认前拒绝")
		return false
	}, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "us-den-01", "TargetImageName": "invalid-copy",
	}, WithReferenceData(ref))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "当前不支持镜像跨区同步")
	assert.Empty(t, executor.calls)
}

func TestCloneCustomImage_RejectsVMImageToPodZoneBeforeConfirmation(t *testing.T) {
	executor := cloneCustomImageExecutor()
	ref := cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)
	ref.ZoneCatalog = deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 5001, IsPod: true}, DisplayName: "华北一C"},
	})
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("虚机自制镜像克隆到容器区必须在确认前拒绝")
		return false
	}, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-bj2-03", "TargetImageName": "invalid-pod-copy",
	}, WithReferenceData(ref))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "虚机自制镜像不能克隆到容器可用区 华北一C")
	assert.Empty(t, executor.calls)
}

func TestCloneCustomImage_AllowsContainerImageToPodZone(t *testing.T) {
	executor := cloneCustomImageExecutor()
	ref := cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)
	ref.ImageCatalog = deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{{
		ID: "cimg-source", Name: "container-base", Source: "custom", ImageType: "Custom",
		Status: deployment.ImageStatusAvailable, Zone: "cn-wlcb-01", ZoneID: 10027, Container: true,
	}})
	ref.ZoneCatalog = deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 5001, IsPod: true}, DisplayName: "华北一C"},
	})
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-bj2-03", "TargetImageName": "valid-pod-copy",
	}, WithReferenceData(ref))

	require.NoError(t, err)
	assert.True(t, result.Success, result.Message)
	call, ok := findExecutorCall(executor.calls, "SyncCompShareCustomImage")
	require.True(t, ok)
	assert.Equal(t, []uint32{5001}, call.args["TargetZoneIds"])
}

func TestCloneCustomImage_RejectsUnavailableSource(t *testing.T) {
	executor := cloneCustomImageExecutor()
	eng := NewEngine(executor, nil, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-sh2-02", "TargetImageName": "invalid-copy",
	}, WithReferenceData(cloneCustomImageReferenceData("Making", "cn-wlcb-01", 10027)))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "只有 Available 状态可以克隆")
	assert.Empty(t, executor.calls)
}

func TestCloneCustomImage_PerTargetFailureDoesNotReportSuccess(t *testing.T) {
	executor := cloneCustomImageExecutor()
	executor.results["SyncCompShareCustomImage"] = map[string]any{
		"SuccessCount": float64(0), "FailedCount": float64(1),
		"Results": []any{map[string]any{
			"TargetZoneId": float64(8200), "RetCode": float64(1), "Message": "target zone unavailable",
		}},
	}
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-sh2-02", "TargetImageName": "failed-copy",
	}, WithReferenceData(cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "target zone unavailable")
	assert.Nil(t, result.Data)
}

func TestCloneCustomImage_WorkflowPreservesTenantIdentityToSignedRequest(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Content-Type") != "application/json" {
			_, _ = io.WriteString(w, `{"RetCode":0,"Progress":"12"}`)
			return
		}
		var request map[string]any
		require.NoError(t, json.Unmarshal(body, &request))
		requests = append(requests, request)
		if request["Action"] == "SyncCompShareCustomImage" {
			_, _ = io.WriteString(w, `{"RetCode":0,"SuccessCount":1,"Results":[{"TargetZoneId":8200,"CompShareImageId":"cimg-cloned","RetCode":0,"Message":"OK"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"RetCode":0,"Progress":"12"}`)
	}))
	defer server.Close()

	external := tools.NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: server.URL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
	})
	safe := tools.NewSafeToolExecutor(external)
	eng := NewEngine(safe.AsToolExecutor(tools.OriginWorkflowInternal), func(string, map[string]any) bool { return true }, nil)
	ctx := tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: 66391350,
		OrganizationID:    64404856,
		CompanyID:         66391350,
	})

	result, err := eng.Run(ctx, CloneCustomImageDef(), map[string]any{
		"CompShareImageId": "cimg-source", "Zone": "cn-sh2-02", "TargetImageName": "identity-copy",
	}, WithReferenceData(cloneCustomImageReferenceData(deployment.ImageStatusAvailable, "cn-wlcb-01", 10027)))
	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	require.NotEmpty(t, requests)
	syncRequest := requests[0]
	assert.Equal(t, float64(66391350), syncRequest["top_organization_id"])
	assert.Equal(t, float64(64404856), syncRequest["organization_id"])
	assert.Equal(t, float64(66391350), syncRequest["company_id"])
	zones, ok := syncRequest["TargetZoneIds"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{float64(8200)}, zones)
}
