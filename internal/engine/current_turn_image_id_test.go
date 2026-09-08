package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An exact image goes straight to its preselected card, without asking the
// user to rediscover its source, category or family.
func TestExactCustomImageIDReachesItsCardWithoutBrowsingEndToEnd(t *testing.T) {
	const imageID = "compshareImage-custom-current-turn"
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Describe": "华北一C"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{
					"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
					"MachineSizes": []any{map[string]any{
						"Gpu": float64(1), "Collection": []any{map[string]any{
							"Cpu": float64(16), "Memory": []any{float64(64)},
						}},
					}},
				},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareCustomImages":
			return map[string]any{"TotalCount": float64(1), "ImageSet": []any{
				map[string]any{
					"CompShareImageId": imageID, "Name": "我的自制训练环境",
					"ImageType": "Custom", "Status": "Available", "Container": "False",
					"SupportedGpuTypes": []any{"4090"},
				},
			}}, nil
		default:
			// The first remaining card is the concrete-image card, before any
			// capacity/price/create call. Empty successful inventory snapshots are
			// enough for this no-write entry-point test.
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	eng.lastUserMsg = "用" + imageID + "为我创建一台4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-guided-direct-custom", time.Now(),
	)
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
		"turn_id":   "turn-guided-direct-custom",
		"operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})
	require.NoError(t, err)
	confirmable, ok := newConfirmableAction(resolved)
	require.True(t, ok)

	var firstForm *workflow.ConfirmForm
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		firstForm = form
		return workflow.ConfirmResolution{Confirmed: false}
	}
	_ = eng.executeResolvedWorkflow(context.Background(), confirmable, noopStep)

	require.NotNil(t, firstForm, "the direct-id flow must reach a selection card")
	require.NotNil(t, firstForm.Field("ImageId"))
	assert.Equal(t, imageID, firstForm.Field("ImageId").Value)
	assert.True(t, firstForm.Field("ImageId").Editable)
	for _, field := range []string{"ImageSource", "ImageCategory", "ImageTag", "ImageFamily"} {
		assert.Nil(t, firstForm.Field(field), "direct exact id must not reopen %s", field)
	}
	assert.NotContains(t, executor.calls, "DescribeCompShareImageTags",
		"a custom image inventory never queries the public taxonomy")
	assert.NotContains(t, executor.calls, "CreateCompShareInstance",
		"the regression test cancels before any create call")
}
