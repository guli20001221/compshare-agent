package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageUnavailableErrorIdentifiesTheUpstreamField(t *testing.T) {
	assert.True(t, isImageUnavailableError(tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available")))
	assert.False(t, isImageUnavailableError(tools.NewUpstreamAPIError(230, "Params [Zone] not available")))
}

type recoveryMockExecutor struct {
	calls []string
}

func (m *recoveryMockExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"TotalCount": float64(0), "UHostSet": []any{}}, nil
	case "DescribeCompShareImages":
		// The catalog contains both the requested image and other available images.
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-bad", "Name": "PyTorch:24.04-py3", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-good", "Name": "cuda128_torch291_py312", "ImageType": "App"},
			map[string]any{"CompShareImageId": "img-win", "Name": "Windows-nvidia 2022", "ImageType": "System"},
		}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A"},
		}}, nil
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal", "MachineSizes": []any{
				map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}},
			}},
		}}, nil
	case "CheckCompShareResourceCapacity":
		if id, _ := args["CompShareImageId"].(string); id == "img-bad" {
			return nil, tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available")
		}
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}}, nil
	case "GetCompShareInstanceUserPrice":
		return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": float64(1.58)}}}, nil
	case "CreateCompShareInstance":
		return map[string]any{"UHostIds": []any{"uhost-good1"}}, nil
	}
	return map[string]any{"Action": action, "RetCode": float64(0)}, nil
}

func TestUnavailableCreateImageDoesNotSubstituteAnotherImage(t *testing.T) {
	exec := &recoveryMockExecutor{}
	confirmations := 0
	eng := NewWithDeps(&mockLLM{}, exec, func(_ string, _ map[string]any) bool {
		confirmations++
		return true
	})
	reply := eng.executeResolvedWorkflow(context.Background(),
		mustConfirmable("CreateInstanceWorkflow", map[string]any{
			"GpuType": "4090", "ImageName": "PyTorch", "CompShareImageId": "img-bad",
		}, zoneRefData(eng.zoneCatalogSnapshot(context.Background()))), noopStep)

	require.False(t, reply.terminatesTurn(), "a pre-confirmation validation failure must return to the Agent")
	var result workflow.Result
	require.NoError(t, json.Unmarshal([]byte(reply.Observation), &result))
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "请求参数不符合接口要求或存在冲突")
	assert.Equal(t, "检查库存", result.StoppedAt)
	assert.Zero(t, confirmations, "the unavailable exact image must not be replaced by another image's card")
	imageReads := 0
	for _, action := range exec.calls {
		if action == "DescribeCompShareImages" {
			imageReads++
		}
		assert.NotEqual(t, "CreateCompShareInstance", action)
	}
	assert.Equal(t, 1, imageReads, "workflow failure must return without a second automatic image search")
}
