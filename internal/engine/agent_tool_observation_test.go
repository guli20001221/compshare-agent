package engine

import (
	"context"
	"strconv"
	"testing"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestTypedReadPreservesUpstreamFailureDisposition(t *testing.T) {
	for _, tc := range []struct {
		code   int
		status tools.AgentToolStatus
		next   tools.AgentToolNextStep
	}{
		{120, tools.AgentToolStatusRetryLater, tools.AgentToolNextRetryLater},
		{240, tools.AgentToolStatusFailed, tools.AgentToolNextAnswerUser},
		{230, tools.AgentToolStatusChooseAlternative, tools.AgentToolNextChooseOption},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			upstreamErr := tools.NewUpstreamAPIError(tc.code, "upstream detail")
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				require.Equal(t, "DescribeCompShareImageTags", action)
				return nil, upstreamErr
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			onStep, steps := collectSteps()
			const action = "ReadCapability_image_tag_catalog"
			raw := eng.executeConcreteReadCapability(context.Background(), action, map[string]any{}, onStep)
			result, ok := tools.ParseAgentToolResult(agentToolObservation(action, raw))
			require.True(t, ok)
			require.Equal(t, tc.status, result.Status)
			require.Equal(t, tc.next, result.NextStep)
			require.Equal(t, "UPSTREAM_RETCODE_"+strconv.Itoa(tc.code), result.Error.Code)
			require.Equal(t, upstreamErr.UserMessage(), result.Error.Message)
			require.Equal(t, result.Error.Code, (*steps)[len(*steps)-1].ErrorCode)
		})
	}
}

func TestAgentToolObservationMapsFiveStableOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantStatus tools.AgentToolStatus
		wantNext   tools.AgentToolNextStep
		retryable  bool
		missing    []string
	}{
		{
			name:       "success",
			raw:        `{"status":"handled","evidence":{"facts":[]}}`,
			wantStatus: tools.AgentToolStatusSuccess,
			wantNext:   tools.AgentToolNextAnswerUser,
		},
		{
			name:       "missing fields",
			raw:        `{"status":"needs_input","missing_fields":[{"name":"source","reason":"required"}]}`,
			wantStatus: tools.AgentToolStatusNeedsInput,
			wantNext:   tools.AgentToolNextAskUser,
			missing:    []string{"source"},
		},
		{
			name:       "retryable read failure",
			raw:        `{"status":"failure_after_tool","failure_class":"generic_read"}`,
			wantStatus: tools.AgentToolStatusRetryLater,
			wantNext:   tools.AgentToolNextRetryLater,
			retryable:  true,
		},
		{
			name:       "alternative required",
			raw:        `{"status":"conflict","candidates":["a","b"]}`,
			wantStatus: tools.AgentToolStatusChooseAlternative,
			wantNext:   tools.AgentToolNextChooseOption,
		},
		{
			name:       "unrecoverable",
			raw:        `{"success":false,"message":"not safe to retry"}`,
			wantStatus: tools.AgentToolStatusFailed,
			wantNext:   tools.AgentToolNextAnswerUser,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, ok := tools.ParseAgentToolResult(agentToolObservation("ReadCapability_image_list", tc.raw))
			require.True(t, ok)
			require.Equal(t, tc.wantStatus, result.Status)
			require.Equal(t, tc.wantNext, result.NextStep)
			require.Equal(t, tc.retryable, result.Retryable)
			require.Equal(t, tc.missing, result.Meta.MissingFields)
			require.NotEmpty(t, result.Error.Code)
			require.Equal(t, "ReadCapability_image_list", result.Meta.Action)
		})
	}
}

func TestMissingWriteTargetAsksTheAgentToCompleteItsCall(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "关闭 uhost-1"
	resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
		"operation": "StopInstanceWorkflow", "slots": []any{},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"UHostId"}, resolved.action.Missing)
	require.Empty(t, resolved.action.Arguments, "the server does not fill the omitted target")

	result, ok := tools.ParseAgentToolResult(agentToolObservation("RequestStopInstance", resolvedActionForModel(resolved.action)))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolNextCorrectToolCall, result.NextStep)
	require.Equal(t, tools.AgentToolCodeInvalidArguments, result.Error.Code)
	require.Equal(t, []string{"UHostId"}, result.Meta.MissingFields)
	require.Contains(t, result.Error.Message, "用户消息、对话历史或工具结果")
	require.Contains(t, result.Error.Message, "确实无法确定目标时才询问用户")
}

func TestMissingCreateSpecificationsKeepTheExistingInputPath(t *testing.T) {
	raw := resolvedActionForModel(actionresolver.ResolvedAction{
		Operation: "CreateInstanceWorkflow", Missing: []string{"GpuType", "Zone"}, ReadyForIntake: true,
	})
	result, ok := tools.ParseAgentToolResult(agentToolObservation("RequestCreateInstance", raw))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolNextAskUser, result.NextStep)
	require.Equal(t, "MISSING_REQUIRED_FIELDS", result.Error.Code)
	require.Equal(t, []string{"GpuType", "Zone"}, result.Meta.MissingFields)
	require.Equal(t, true, result.Data.(map[string]any)["ready_for_intake"])
}

func TestRejectedWriteProposalIsRoutedToTheResponsibleActor(t *testing.T) {
	tests := []struct {
		name     string
		actor    actionresolver.RejectionActor
		wantNext tools.AgentToolNextStep
		wantCode string
	}{
		{
			name:     "model fixes its malformed proposal",
			actor:    actionresolver.RejectionActorModel,
			wantNext: tools.AgentToolNextCorrectToolCall,
			wantCode: tools.AgentToolCodeInvalidArguments,
		},
		{
			name:     "user chooses a different invalid value",
			actor:    actionresolver.RejectionActorUser,
			wantNext: tools.AgentToolNextAskUser,
			wantCode: "INVALID_FIELD_VALUE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := resolvedActionForModel(actionresolver.ResolvedAction{
				Operation: "CreateInstanceWorkflow",
				Rejected:  []string{"Zone: internal validation detail"},
				RejectedProblems: []actionresolver.RejectedProblem{{
					Slot: "Zone", Kind: actionresolver.RejectInvalidValue, Actor: tc.actor,
				}},
			})
			result, ok := tools.ParseAgentToolResult(agentToolObservation("RequestCreateInstance", raw))
			require.True(t, ok)
			require.Equal(t, tools.AgentToolStatusNeedsInput, result.Status)
			require.Equal(t, tc.wantNext, result.NextStep)
			require.Equal(t, tc.wantCode, result.Error.Code)
			data, ok := result.Data.(map[string]any)
			require.True(t, ok)
			require.NotContains(t, data, "rejected", "human/internal rejection prose must not enter the model payload")
			details, ok := data["rejection_details"].([]any)
			require.True(t, ok)
			require.Len(t, details, 1)
			detail := details[0].(map[string]any)
			require.Equal(t, "Zone", detail["slot"])
			require.Equal(t, "invalid_value", detail["kind"])
			require.Equal(t, string(tc.actor), detail["actor"])
		})
	}
}

func TestAgentToolObservationTreatsEmptyKnowledgeAsNoCitableEvidence(t *testing.T) {
	result, ok := tools.ParseAgentToolResult(agentToolObservation("SearchKnowledge", `{"EvidenceLedger":{"items":[]},"empty":true}`))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolStatusFailed, result.Status)
	require.Equal(t, "NO_CITABLE_EVIDENCE", result.Error.Code)
	require.False(t, result.Retryable)
	require.Equal(t, tools.AgentToolNextAnswerWithLimits, result.NextStep)
	require.Equal(t, "SearchKnowledge", result.Meta.Action)
	data, ok := result.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, data["empty"])
}

func TestAgentToolObservationDirectsReviewableKnowledgeCandidatesToReadFirst(t *testing.T) {
	result, ok := tools.ParseAgentToolResult(agentToolObservation("SearchKnowledge", `{
		"EvidenceLedger":{"items":[]},
		"empty":true,
		"floor_dropped_all":true,
		"below_floor_candidates":[{"chunk_id":"candidate-1","strength":"below_floor"}],
		"note":"用 ReadChunk 核验候选全文"
	}`))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolStatusFailed, result.Status)
	require.Equal(t, tools.AgentToolCodeNoCitableEvidence, result.Error.Code)
	require.Equal(t, tools.AgentToolNextInspectCandidates, result.NextStep)
	require.False(t, result.Retryable)
}

func TestAgentToolObservationKeepsEmptyPlatformReadsAsSuccess(t *testing.T) {
	result, ok := tools.ParseAgentToolResult(agentToolObservation("DescribeCompShareInstance", `{"instances":[],"empty":true}`))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolStatusSuccess, result.Status)
	require.Equal(t, tools.AgentToolNextAnswerUser, result.NextStep)
}

func TestAgentToolObservationKeepsLegacySourceStatusOnlyInMeta(t *testing.T) {
	result, ok := tools.ParseAgentToolResult(agentToolObservation("DescribeCompShareInstance", `{"status":"success","instances":[]}`))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolStatusSuccess, result.Status)
	require.Equal(t, "success", result.Meta.SourceStatus)
	data, ok := result.Data.(map[string]any)
	require.True(t, ok)
	_, present := data["status"]
	require.False(t, present, "data must not carry a second status vocabulary")
}

func TestAgentToolObservationKeepsScopedEmptyCatalogGuidance(t *testing.T) {
	raw := `{"status":"empty","guidance":"当前目录筛选未命中；不代表知识库中没有使用说明。","can_assert_absence":true}`
	result, ok := tools.ParseAgentToolResult(agentToolObservation("ReadCapability_image_list", raw))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolStatusSuccess, result.Status)
	data, ok := result.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "当前目录筛选未命中；不代表知识库中没有使用说明。", data["guidance"])
	require.Equal(t, true, data["can_assert_absence"])
}

func TestAgentOnlyReceivesP2ContractForNormalToolRound(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read-instance", "ReadCapability_resource_info", `{}`)}},
		{Content: "已读取实例信息。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"RetCode": 0, "UHostSet": []any{}},
	}}, nil)

	_, err := eng.Chat(context.Background(), "查看实例", noopStep)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(model.calls), 2)

	var observation string
	for _, message := range model.calls[1].Messages {
		if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == "read-instance" {
			observation = message.Content
			break
		}
	}
	require.NotEmpty(t, observation)
	result, ok := tools.ParseAgentToolResult(observation)
	require.True(t, ok, observation)
	require.Equal(t, tools.AgentToolStatusSuccess, result.Status)
	require.Equal(t, "ReadCapability_resource_info", result.Meta.Action)
	data, ok := result.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "empty", result.Meta.SourceStatus)
	require.Equal(t, true, data["can_assert_absence"], "a successful empty upstream listing is authoritative for its query")
	require.Len(t, eng.platformReadEvidenceThisTurn, 1, "the final gateway must receive the authoritative empty result as platform evidence")
}
