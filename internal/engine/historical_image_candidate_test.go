package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recent assistant answer is visible to the model as ordinary conversation.
// This test starts at the action-contract boundary: the model carries the exact
// historical id, while the server treats it as an unconfirmed candidate, discovers
// its real source with live point queries, and verifies it before the workflow.
func TestProposalMayCarryPriorRecommendedIDAndDetectsCommunitySource(t *testing.T) {
	const (
		imageID   = "compshareImage-1pl06yxr5lvm"
		imageName = "FaceFusion 3.5.1 / 3.6.1 全模型离线版（TensorRT 加速）"
	)
	var communityArgs map[string]any
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCompShareImages":
			return nil, tools.NewUpstreamAPIError(230, "Params [CompShareImageId] not available")
		case "DescribeCommunityImages":
			communityArgs = args
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": imageName, "Data": []any{
					map[string]any{
						"CompShareImageId": imageID,
						"Name":             "v3.6.1",
						"Status":           "Available",
					},
				}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "用该镜像为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-image-context", time.Now(),
	)
	eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
		User: "推荐一个换脸视频镜像",
		Assistant: "推荐使用社区镜像：" + imageName +
			"，镜像 ID：" + imageID + "。",
	}}
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-image-context", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})

	require.NoError(t, err)
	require.Equal(t, imageID, resolved.action.Arguments["CompShareImageId"])
	require.Equal(t, actionresolver.SourceAgentInference,
		resolved.action.Provenance["CompShareImageId"].Source,
		"历史 ID 不是本轮逐字选择，不能提升为用户授权")
	require.Equal(t, "community", resolved.action.Arguments["ImageSource"])
	require.Equal(t, workflow.ImageSelectionSuggested, resolved.referenceData.ImageSelection,
		"历史推荐是待确认候选，不能冒充用户已选择")
	require.NotNil(t, resolved.referenceData.ImageCatalog)
	_, ok := resolved.referenceData.ImageCatalog.ByID(imageID)
	require.True(t, ok)
	require.Equal(t, imageID, communityArgs["CompShareImageId"])
	require.NotContains(t, communityArgs, "Offset", "精确 ID 查询不能退化为全量分页")
}

func TestStaleHistoricalImageIDIsRejectedWithoutClaimingCatalogOutage(t *testing.T) {
	const imageID = "compshareImage-stale"
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"TotalCount": float64(0), "ImageSet": []any{}}, nil
		case "DescribeCommunityImages":
			return nil, tools.NewUpstreamAPIError(8039, "Resource not exist [compshareImage-stale]")
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "用该镜像为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-stale-image", time.Now(),
	)
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-stale-image", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})

	require.NoError(t, err)
	require.Contains(t, resolved.action.RejectedProblems,
		actionresolver.RejectedProblem{Slot: "CompShareImageId", Kind: actionresolver.RejectInvalidValue})
	assert.Empty(t, resolved.action.DependencyFailures,
		"上游明确报告所有来源都无此镜像时，不能误报成目录不可用")
	assert.NotContains(t, resolved.action.Arguments, "CompShareImageId")
	assert.False(t, resolved.action.ReadyForConfirmation)
}

func TestLocalizedUserImageSourceIsAConstraint(t *testing.T) {
	const imageID = "compshareImage-community-only"
	communityCalls := 0
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"TotalCount": float64(0), "ImageSet": []any{}}, nil
		case "DescribeCommunityImages":
			communityCalls++
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "community-only", "Data": []any{
					map[string]any{"CompShareImageId": imageID, "Status": "Available"},
				}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "请用平台镜像为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-explicit-image-source", time.Now(),
	)
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(),
		proposalArgsForOperation("CreateInstanceWorkflow", map[string]any{
			"GpuType":                         "4090",
			"ImageSource":                     "platform",
			"CompShareImageId":                imageID,
			proposalChargeTypeUserQuoteField:  "",
			proposalImageSourceUserQuoteField: "平台镜像",
		}))

	require.NoError(t, err)
	require.Equal(t, actionresolver.SourceUserExplicit,
		resolved.action.Provenance["ImageSource"].Source)
	require.Zero(t, communityCalls,
		"用户明确选择平台来源后，不能为了让 ID 命中而静默改查社区来源")
	require.Contains(t, resolved.action.RejectedProblems,
		actionresolver.RejectedProblem{Slot: "CompShareImageId", Kind: actionresolver.RejectInvalidValue})
	require.False(t, resolved.action.ReadyForConfirmation)
}
