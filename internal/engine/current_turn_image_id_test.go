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

func TestExactCommunityImageIDReachesItsCardBeforeGPUSelection(t *testing.T) {
	for _, imageName := range []string{"v3.6.1", "", "FaceFusion"} {
		t.Run("ImageName="+imageName, func(t *testing.T) {
			const imageID = "compshareImage-1pl06yxr5lvm"
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeCompShareSupportZone":
					return map[string]any{"ZoneInfo": []any{map[string]any{
						"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "Describe": "华北一C",
					}}}, nil
				case "DescribeAvailableCompShareInstanceTypes":
					return map[string]any{"AvailableInstanceTypes": []any{map[string]any{
						"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
						"MachineSizes": []any{map[string]any{
							"Gpu": float64(1), "Collection": []any{map[string]any{
								"Cpu": float64(16), "Memory": []any{float64(64)},
							}},
						}},
					}}}, nil
				case "DescribeCompShareImages", "DescribeCompShareCustomImages", "DescribeCompShareSharingImages":
					return map[string]any{"ImageSet": []any{}}, nil
				case "DescribeCommunityImages":
					return map[string]any{"TotalCount": float64(1), "CompshareImageGroup": []any{
						map[string]any{"GroupId": "group-facefusion", "ImageName": "FaceFusion", "Data": []any{
							map[string]any{
								"CompShareImageId": imageID, "Name": "v3.6.1", "VersionName": "v3.6.1",
								"Status": "Available", "ImageType": "Community", "Container": true,
								"SupportedGpuTypes": []any{"4090"}, "Size": float64(102400),
							},
						}},
					}}, nil
				case "CreateCompShareInstance":
					t.Fatal("the image-card test must not create an instance")
				}
				return map[string]any{"RetCode": float64(0)}, nil
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.guidedCreate = true
			eng.lastUserMsg = "用 v3.6.1 开一台，GPU 帮我选"
			eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
				eng, eng.lastUserMsg, "turn-guided-community-version", time.Now(),
			)
			eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
				User:      "推荐一个换脸镜像",
				Assistant: "推荐社区镜像 FaceFusion · v3.6.1，镜像 ID：" + imageID,
			}}
			eng.turnContextViewReady = true
			slots := []any{map[string]any{"name": "CompShareImageId", "value": imageID}}
			if imageName != "" {
				slots = append(slots, map[string]any{"name": "ImageName", "value": imageName})
			}
			resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
				"turn_id": "turn-guided-community-version", "operation": "CreateInstanceWorkflow", "slots": slots,
			})
			require.NoError(t, err)
			require.Equal(t, imageID, resolved.action.Arguments["CompShareImageId"])
			require.NotContains(t, resolved.action.Arguments, "GpuType")
			require.Empty(t, resolved.action.Rejected)
			require.Empty(t, resolved.action.DependencyFailures)
			require.True(t, resolved.action.ReadyForIntake)
			confirmable, ok := newConfirmableAction(resolved)
			require.True(t, ok)

			var imageForm *workflow.ConfirmForm
			eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
				imageForm = form
				return workflow.ConfirmResolution{Confirmed: false}
			}
			reply := eng.executeResolvedWorkflow(context.Background(), confirmable, noopStep)
			require.NotNil(t, imageForm, "verified exact image must reach the guided image card: %v", reply)
			require.NotNil(t, imageForm.Field("ImageId"), "the image card must precede hardware selection")
			assert.True(t, imageForm.Field("ImageId").Editable)
			assert.Equal(t, imageID, imageForm.Field("ImageId").Value)
			assert.NotContains(t, executor.calls, "CreateCompShareInstance")
		})
	}
}
