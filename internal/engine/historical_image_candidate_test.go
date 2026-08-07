package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
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
	eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
		User:      "推荐一个镜像",
		Assistant: "此前推荐的镜像 ID：" + imageID,
	}}
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
	eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
		User:      "此前推荐过什么镜像",
		Assistant: "社区候选的镜像 ID：" + imageID,
	}}
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

func TestCurrentImageNameKeepsRelatedHistoricalIDAsSuggested(t *testing.T) {
	const (
		imageID   = "compshareImage-facefusion-361"
		otherID   = "compshareImage-comfyui-51"
		imageName = "FaceFusion 3.5.1 / 3.6.1 全模型离线版"
	)
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCommunityImages":
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
	eng.lastUserMsg = "用 FaceFusion 为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-related-history-image", time.Now(),
	)
	eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
		User: "推荐两个视频镜像",
		Assistant: "1. FaceFusion，镜像 ID：" + imageID +
			"；2. ComfyUI，镜像 ID：" + otherID,
	}}
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-related-history-image", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "ImageSource", "value": "community"},
			map[string]any{"name": "ImageName", "value": "FaceFusion"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})

	require.NoError(t, err)
	require.Equal(t, imageID, resolved.action.Arguments["CompShareImageId"])
	require.Equal(t, "FaceFusion", resolved.action.Arguments["ImageName"])
	require.Equal(t, actionresolver.SourceUserExplicit,
		resolved.action.Provenance["ImageName"].Source)
	require.Equal(t, actionresolver.SourceAgentInference,
		resolved.action.Provenance["CompShareImageId"].Source)
	require.Equal(t, workflow.ImageSelectionSuggested, resolved.referenceData.ImageSelection,
		"用户只复制名称时，Agent 选择的具体版本仍必须在镜像卡片中确认")
}

// The current request is the production-shaped regression: the recommendation
// printed the complete upstream community label, while the user copied only its
// readable shorthand with spaces. The exact ID is stronger evidence than that
// presentation difference, so it must stay suggested and skip the redundant
// source card (the workflow's existing ImageSelectionSuggested contract handles
// the latter).
func TestCurrentImageShorthandKeepsHistoricalIDAsSuggested(t *testing.T) {
	const (
		imageID   = "compshareImage-1mefk6bv35xn"
		imageName = "最强AI数字人InfiniteTalk-图片和视频数字人"
	)
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": imageName, "Data": []any{
					map[string]any{
						"CompShareImageId": imageID,
						"Name":             "v1",
						"Status":           "Available",
					},
				}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "最强 AI 数字人 InfiniteTalk，用这个镜像为我创建机器"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-infinite-talk-shorthand", time.Now(),
	)
	eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
		User:      "我想做数字人，为我推荐镜像",
		Assistant: "首选：" + imageName + "，镜像 ID：" + imageID + "。",
	}}
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-infinite-talk-shorthand", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "ImageSource", "value": "community"},
			map[string]any{"name": "ImageName", "value": "最强 AI 数字人 InfiniteTalk"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})

	require.NoError(t, err)
	require.Equal(t, imageID, resolved.action.Arguments["CompShareImageId"],
		"格式化简称不能让服务端丢弃模型从近期已提供的历史承接的精确 ID")
	require.Equal(t, "community", resolved.action.Arguments["ImageSource"])
	require.Equal(t, workflow.ImageSelectionSuggested, resolved.referenceData.ImageSelection,
		"保留的是待确认推荐，不是跳过镜像确认的用户授权")
}

func TestCurrentImageNameDropsUnrelatedHistoricalID(t *testing.T) {
	const (
		faceFusionID = "compshareImage-facefusion-361"
		svcFusionID  = "compshareImage-svc-fusion-16"
	)
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "SVC-Fusion_api_rvc", "Data": []any{
					map[string]any{
						"CompShareImageId": svcFusionID,
						"Name":             "v1.6",
						"Status":           "Available",
					},
				}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "用 FaceFusion 为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-unrelated-history-image", time.Now(),
	)
	eng.turnContextViewThisTurn.RecentConversation = []ConversationPair{{
		User: "推荐两个视频镜像",
		Assistant: "1. FaceFusion，镜像 ID：" + faceFusionID +
			"；2. SVC-Fusion，镜像 ID：" + svcFusionID,
	}}
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-unrelated-history-image", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "ImageSource", "value": "community"},
			map[string]any{"name": "ImageName", "value": "FaceFusion"},
			map[string]any{"name": "CompShareImageId", "value": svcFusionID},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "FaceFusion", resolved.action.Arguments["ImageName"])
	assert.NotContains(t, resolved.action.Arguments, "CompShareImageId",
		"本轮名称必须覆盖无关的历史具体版本")
	assert.Equal(t, "community", resolved.action.Arguments["ImageSource"],
		"丢弃错误 ID 不应把已知社区来源静默改回默认平台来源")
	assert.Empty(t, resolved.action.RejectedProblems)
	assert.Empty(t, resolved.action.DependencyFailures)
	assert.Nil(t, resolved.referenceData.ImageCatalog)
	assert.Equal(t, workflow.ImageSelectionUserPinned, resolved.referenceData.ImageSelection,
		"丢弃错误 ID 后应回到用户名称驱动的普通镜像选择")
}

func TestUngroundedButValidImageIDIsDiscardedBeforeCatalogLookup(t *testing.T) {
	const imageID = "compshareImage-valid-but-invented"
	imageQueries := 0
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCompShareImages", "DescribeCommunityImages":
			imageQueries++
			return map[string]any{"ImageSet": []any{
				map[string]any{
					"CompShareImageId": imageID,
					"Name":             "A real upstream image",
					"Status":           "Available",
				},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-ungrounded-image", time.Now(),
	)
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-ungrounded-image", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "ImageSource", "value": "platform"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})

	require.NoError(t, err)
	assert.Zero(t, imageQueries,
		"仅仅在上游真实存在，不能反过来给 Agent 编出的 ID 补证据")
	assert.NotContains(t, resolved.action.Arguments, "CompShareImageId")
	assert.Equal(t, "platform", resolved.action.Arguments["ImageSource"])
	assert.Equal(t, workflow.ImageSelectionUnset, resolved.referenceData.ImageSelection)
}

func TestCurrentImageReadEvidenceMayGroundExactID(t *testing.T) {
	const imageID = "compshareImage-current-read"
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"},
			}}, nil
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{
				map[string]any{"ImageName": "FaceFusion", "Data": []any{
					map[string]any{
						"CompShareImageId": imageID,
						"Status":           "Available",
					},
				}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "用刚查询到的镜像为我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(
		eng, eng.lastUserMsg, "turn-read-grounded-image", time.Now(),
	)
	eng.turnContextViewReady = true
	payload, err := json.Marshal(ReadCapabilityObservation{
		Status: platform.ReadStatusHandled,
		Envelope: &envelope.Envelope{
			Kind: envelope.KindImageList,
			Subjects: []envelope.Subject{{
				ID: imageSubjectIDPrefix + imageID, Type: envelope.SubjectImage,
			}},
		},
	})
	require.NoError(t, err)
	eng.toolResultsByCallThisTurn = map[string]string{"read-image": string(payload)}

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-read-grounded-image", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090"},
			map[string]any{"name": "ImageSource", "value": "community"},
			map[string]any{"name": "CompShareImageId", "value": imageID},
		},
	})

	require.NoError(t, err)
	require.Equal(t, imageID, resolved.action.Arguments["CompShareImageId"])
	require.Equal(t, workflow.ImageSelectionSuggested, resolved.referenceData.ImageSelection)
}

func TestFailedOrNonImageReadEvidenceCannotGroundExactID(t *testing.T) {
	const imageID = "compshareImage-not-proven"
	encode := func(observation ReadCapabilityObservation) string {
		payload, err := json.Marshal(observation)
		require.NoError(t, err)
		return string(payload)
	}
	eng := &Engine{toolResultsByCallThisTurn: map[string]string{
		"failed-image": encode(ReadCapabilityObservation{
			Status: platform.ReadStatusFailureAfterTool,
			Envelope: &envelope.Envelope{
				Kind: envelope.KindImageList,
				Subjects: []envelope.Subject{{
					ID: imageSubjectIDPrefix + imageID, Type: envelope.SubjectImage,
				}},
			},
		}),
		"other-read": encode(ReadCapabilityObservation{
			Status: platform.ReadStatusHandled,
			Envelope: &envelope.Envelope{
				Kind: envelope.KindZoneCatalog,
				Subjects: []envelope.Subject{{
					ID: imageSubjectIDPrefix + imageID, Type: envelope.SubjectImage,
				}},
			},
		}),
	}}

	assert.False(t, eng.imageIDAppearsInCurrentReadEvidence(imageID),
		"只有成功的镜像目录证据能支持精确 ID，错误回显和其他能力结果都不能")
}

func TestHistoricalImageIDEvidenceRequiresTokenBoundary(t *testing.T) {
	assert.True(t, containsStandaloneValue("镜像 ID：img-1。", "img-1"))
	assert.False(t, containsStandaloneValue("镜像 ID：img-10。", "img-1"),
		"短 ID 不能借更长 ID 的子串获得历史证据")
}
