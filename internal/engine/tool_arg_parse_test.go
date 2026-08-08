package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteTool_MalformedArgsReturnsCorrectiveHint pins the two things the
// corrective-hint change must guarantee for the ~4% of production SearchKnowledge
// calls whose arguments are not a JSON object (flash emits a leaked tag or a bare
// query string instead of `{"query":"…"}`):
//
//  1. The tool result FED BACK to the agent is a P2 needs_input observation with
//     an explicit valid-JSON hint, so the next ReAct round can recover instead
//     of stalling on a bare parser error.
//  2. The RECORDED StepError message stays concise and omits the corrective
//     hint. Trace records intentionally reduce that user-facing message to the
//     closed-set "tool_error" class, so telemetry does not retain raw upstream
//     detail.
//
// The two inputs mirror the two observed production failure modes: a `<`-prefixed
// leaked tag (78/90 errors) and a bare CJK query string (`æ/å/è`, 12/90).
func TestExecuteTool_MalformedArgsReturnsCorrectiveHint(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"leaked_tag_prefix", "<think>需要搜索显卡驱动</think>"},
		{"bare_cjk_query", "如何安装 GPU 驱动"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var steps []StepEvent
			onStep := func(ev StepEvent) { steps = append(steps, ev) }
			tool := openai.ToolCall{Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: tc.args}}

			got := (&Engine{}).executeTool(context.Background(), tool, onStep)

			// (1) Model-facing tool result: stable P2 control plane + corrective hint.
			result, ok := tools.ParseAgentToolResult(got)
			require.Truef(t, ok, "returned result must use the agent tool-result contract, got %q", got)
			assert.Equal(t, tools.AgentToolStatusNeedsInput, result.Status)
			assert.Equal(t, "INVALID_TOOL_ARGUMENTS", result.Error.Code)
			assert.Contains(t, result.Error.Message, "JSON")
			// The model malformed its OWN arguments. The user said nothing wrong
			// and has nothing to add, so this must route back to the model. The
			// prompt turns ask_user into "补问缺字段", which would make ~4% of
			// SearchKnowledge calls (the rate this file's fixtures come from) ask
			// the user to restate a question they already stated correctly.
			assert.Equal(t, tools.AgentToolNextCorrectToolCall, result.NextStep)
			assert.NotEqual(t, tools.AgentToolNextAskUser, result.NextStep,
				"a model-side format error must never be turned into a question for the user")
			assert.False(t, result.Retryable,
				"retryable means the SAME call may succeed later; this one must be corrected first")

			// (2) UI step message: concise error, NOT the hint.
			require.Len(t, steps, 1)
			assert.Equal(t, StepError, steps[0].Type)
			assert.Equal(t, "SearchKnowledge", steps[0].Action)
			assert.Contains(t, steps[0].Message, "parameter parse error:",
				"recorded message must be the concise parse error, got %q", steps[0].Message)
			assert.NotContains(t, steps[0].Message, "重新调用",
				"the UI message must NOT carry the corrective hint")
		})
	}
}
