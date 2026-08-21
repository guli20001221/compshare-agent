package engine

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestModelToolBoundaryRejectsRawActionsMissingFromTheExactWindow(t *testing.T) {
	eng := &Engine{}
	window := centralAgentToolWindow(true, true)
	for _, action := range []string{"StopCompShareInstance", "DescribeCompShareJupyterToken"} {
		t.Run(action, func(t *testing.T) {
			require.False(t, toolListContainsFunction(window, action), "raw action unexpectedly advertised")
			out := eng.executeModelTool(context.Background(), toolCall("hidden", action, `{}`), window, noopStep)
			require.Contains(t, out, `"code":"TOOL_NOT_ALLOWED"`)
		})
	}
}

func TestModelToolBoundaryLetsAnAdvertisedNameReachTheNormalDispatcher(t *testing.T) {
	eng := &Engine{}
	window := centralAgentToolWindow(false, false)
	require.True(t, toolListContainsFunction(window, "SearchKnowledge"))

	// Malformed arguments are useful here: they prove the call crossed the
	// window boundary and reached the ordinary parser without needing a live
	// retriever or platform executor.
	out := eng.executeModelTool(context.Background(), toolCall("visible", "SearchKnowledge", `{`), window, noopStep)
	require.Contains(t, out, `"code":"INVALID_TOOL_ARGUMENTS"`)
	require.NotContains(t, out, "TOOL_NOT_ALLOWED")
}

func TestImageRequestsFollowTheSharedActionFirstContract(t *testing.T) {
	window := centralAgentToolWindow(true, false)
	for _, name := range []string{"RequestCreateCustomImage", "RequestCloneCustomImage"} {
		t.Run(name, func(t *testing.T) {
			var fn *openai.FunctionDefinition
			for _, tool := range window {
				if tool.Function != nil && tool.Function.Name == name {
					fn = tool.Function
					break
				}
			}
			require.NotNil(t, fn)
			require.NotContains(t, fn.Description, "先追问")
			require.NotContains(t, fn.Description, "明确后才")
			require.Contains(t, fn.Description, "编造")

			root, ok := fn.Parameters.(map[string]any)
			require.True(t, ok)
			required, ok := root["required"].([]string)
			require.True(t, ok)
			require.Empty(t, required, "partial Request calls must remain schema-valid")
		})
	}

	// Keep the shared prompt itself authoritative; operation descriptions must
	// not silently reintroduce a prose-first exception under a synonym.
	for _, tool := range window {
		if tool.Function == nil || !strings.HasPrefix(tool.Function.Name, "Request") {
			continue
		}
		require.NotContains(t, tool.Function.Description, "缺任一项先追问", tool.Function.Name)
	}
}
