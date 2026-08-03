package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentToolResultHasOneStableShape(t *testing.T) {
	raw := MarshalAgentToolResult(AgentToolSuccess("DescribeCompShareInstance", map[string]any{"count": 1}, AgentToolMeta{Attempts: 1}))
	result, ok := ParseAgentToolResult(raw)
	require.True(t, ok, raw)
	require.Equal(t, AgentToolStatusSuccess, result.Status)
	require.Equal(t, agentToolNoErrorCode, result.Error.Code)
	require.False(t, result.Retryable)
	require.Equal(t, AgentToolNextAnswerUser, result.NextStep)
	require.Equal(t, "DescribeCompShareInstance", result.Meta.Action)
	require.Equal(t, 1, result.Meta.Attempts)
}

func TestParseAgentToolResultRejectsIncoherentControlPlane(t *testing.T) {
	raw := `{"status":"success","data":null,"error":{"code":"NONE"},"retryable":true,"next_step":"retry_later","meta":{"action":"DescribeCompShareInstance"}}`
	_, ok := ParseAgentToolResult(raw)
	require.False(t, ok, "an envelope with mismatched status/retry/next fields must be normalised instead of trusted")
}

func TestAgentToolResultFromUpstreamErrorUsesAgentDisposition(t *testing.T) {
	cases := []struct {
		name       string
		retCode    int
		wantStatus AgentToolStatus
		wantNext   AgentToolNextStep
		retryable  bool
	}{
		{name: "temporary service", retCode: 150, wantStatus: AgentToolStatusRetryLater, wantNext: AgentToolNextRetryLater, retryable: true},
		{name: "image or placement rejected", retCode: 230, wantStatus: AgentToolStatusChooseAlternative, wantNext: AgentToolNextChooseOption},
		{name: "permission", retCode: 240, wantStatus: AgentToolStatusFailed, wantNext: AgentToolNextAnswerUser},
		{name: "released resource", retCode: 8351, wantStatus: AgentToolStatusChooseAlternative, wantNext: AgentToolNextChooseOption},
		{name: "too frequent", retCode: 226619, wantStatus: AgentToolStatusRetryLater, wantNext: AgentToolNextRetryLater, retryable: true},
		{name: "capacity sold out", retCode: 226604, wantStatus: AgentToolStatusChooseAlternative, wantNext: AgentToolNextChooseOption},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The message deliberately looks like an upstream body. It must not
			// reach the agent-visible observation; only the safe hint does.
			err := fmt.Errorf("workflow failed: %w", NewUpstreamAPIError(tc.retCode, "raw upstream detail should not leak"))
			result := AgentToolResultFromError("CreateInstanceWorkflow", err, AgentToolMeta{})
			require.Equal(t, tc.wantStatus, result.Status)
			require.Equal(t, tc.wantNext, result.NextStep)
			require.Equal(t, tc.retryable, result.Retryable)
			require.Equal(t, "UPSTREAM_RETCODE_"+fmt.Sprint(tc.retCode), result.Error.Code)

			raw := MarshalAgentToolResult(result)
			require.NotContains(t, raw, "raw upstream detail should not leak")
			require.NotContains(t, raw, "RetCode=")
			require.True(t, strings.Contains(raw, `"status"`))
		})
	}
}

func TestAgentToolResultFromUnknownUpstreamErrorKeepsOnlySafeDiagnosticCode(t *testing.T) {
	err := NewUpstreamAPIError(17000, "raw upstream tenant and request detail")
	result := AgentToolResultFromError("CreateInstanceWorkflow", err, AgentToolMeta{})
	require.Equal(t, AgentToolStatusFailed, result.Status)
	require.Equal(t, "UPSTREAM_RETCODE_17000", result.Error.Code)
	require.Equal(t, "上游服务未能完成本次请求。", result.Error.Message)
	require.NotContains(t, MarshalAgentToolResult(result), "raw upstream tenant")
}

// A malformed tool call is the one failure the user cannot help with, so it is
// the one next step that must not end in a question. The rest of the contract
// deliberately does end there — that is why the exception needs pinning.
func TestInvalidToolCallRoutesBackToTheModelNotTheUser(t *testing.T) {
	result := AgentToolInvalidToolCall(
		"SearchKnowledge",
		"INVALID_TOOL_ARGUMENTS",
		"工具参数必须是合法的 JSON 对象，请按该工具的参数结构重新调用。",
		AgentToolMeta{SourceStatus: "argument_parse_error"},
	)

	require.Equal(t, AgentToolNextCorrectToolCall, result.NextStep)
	require.NotEqual(t, AgentToolNextAskUser, result.NextStep)
	require.Equal(t, AgentToolStatusNeedsInput, result.Status)
	require.Equal(t, "INVALID_TOOL_ARGUMENTS", result.Error.Code)
	require.False(t, result.Retryable, "the same malformed call must not be re-sent unchanged")
	require.Nil(t, result.Data)

	// The engine's observation boundary re-parses every tool result and wraps
	// anything it does not recognise. If correct_tool_call were missing from the
	// valid set, this result would come back out as a plain success — the exact
	// regression this whole change removes, restored silently.
	parsed, ok := ParseAgentToolResult(MarshalAgentToolResult(result))
	require.True(t, ok, "the contract must recognise its own output")
	require.Equal(t, AgentToolNextCorrectToolCall, parsed.NextStep)
}

// Control: no other constructor may quietly adopt the model-owned next step.
// Without this, a later edit could route an upstream failure to correct_tool_call
// and the assertion above would still pass.
func TestOnlyAMalformedCallGetsTheModelOwnedNextStep(t *testing.T) {
	others := []AgentToolResult{
		AgentToolSuccess("A", nil, AgentToolMeta{}),
		AgentToolNeedsInput("A", nil, "", "", AgentToolMeta{}),
		AgentToolRetryLater("A", nil, "", "", AgentToolMeta{}),
		AgentToolChooseAlternative("A", nil, "", "", AgentToolMeta{}),
		AgentToolFailure("A", nil, "", "", AgentToolMeta{}),
		AgentToolNoCitableEvidence("A", nil, AgentToolMeta{}),
		AgentToolResultFromError("A", NewUpstreamAPIError(17000, "x"), AgentToolMeta{}),
	}
	for _, result := range others {
		require.NotEqual(t, AgentToolNextCorrectToolCall, result.NextStep,
			"%s/%s must not claim the model-owned next step", result.Status, result.Error.Code)
	}
}
