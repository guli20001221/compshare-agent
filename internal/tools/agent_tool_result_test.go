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

	// The engine's observation boundary re-parses every tool result and re-wraps
	// anything it does not recognise. A rejected pairing does not fail loudly:
	// the re-wrap reads the raw envelope's own status field, so this comes back
	// out as READ_INPUT_INCOMPLETE / ask_user with correct_tool_call buried in
	// data.next_step — the same question for the user, one layer down.
	parsed, ok := ParseAgentToolResult(MarshalAgentToolResult(result))
	require.True(t, ok, "the contract must recognise its own output")
	require.Equal(t, AgentToolNextCorrectToolCall, parsed.NextStep)
}

// "Re-send the same call" is safe advice only for arguments this binary rejected
// before any user-requested effect. Bound to the constructor alone, the
// guarantee lasts until the next hand-built AgentToolResult; bound in the parser,
// it holds for every result the engine will ever observe — including one that
// pairs this next step with a real upstream failure, which would be a retry loop
// against a side effect that already happened.
func TestTheModelOwnedNextStepIsBoundToTheMalformedArgumentCode(t *testing.T) {
	for _, code := range []string{
		"UPSTREAM_RETCODE_8964", "TOOL_EXECUTION_FAILED", "READ_INPUT_INCOMPLETE",
		"MISSING_REQUIRED_FIELDS", "NO_CITABLE_EVIDENCE", "",
	} {
		forged := AgentToolResult{
			Status:   AgentToolStatusNeedsInput,
			Error:    AgentToolError{Code: code},
			NextStep: AgentToolNextCorrectToolCall,
			Meta:     AgentToolMeta{Action: "CreateInstanceWorkflow"},
		}
		_, ok := ParseAgentToolResult(MarshalAgentToolResult(forged))
		require.Falsef(t, ok, "error code %q must not be allowed to tell the model to re-send the call", code)
	}

	// Control: the one code that may. Without it this test passes against a
	// parser that rejects correct_tool_call outright, which would silently undo
	// the fix above.
	allowed := AgentToolResult{
		Status:   AgentToolStatusNeedsInput,
		Error:    AgentToolError{Code: AgentToolCodeInvalidArguments},
		NextStep: AgentToolNextCorrectToolCall,
		Meta:     AgentToolMeta{Action: "SearchKnowledge"},
	}
	_, ok := ParseAgentToolResult(MarshalAgentToolResult(allowed))
	require.True(t, ok, "INVALID_TOOL_ARGUMENTS must still be able to route back to the model")
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
