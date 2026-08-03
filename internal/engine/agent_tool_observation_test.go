package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

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

func TestAgentOnlyReceivesP2ContractForNormalToolRound(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read-instance", "DescribeCompShareInstance", `{}`)}},
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
	require.Equal(t, "DescribeCompShareInstance", result.Meta.Action)
}
