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
