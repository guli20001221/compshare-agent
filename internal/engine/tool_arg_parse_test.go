package engine

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteTool_MalformedArgsReturnsCorrectiveHint pins the two things the
// corrective-hint change must guarantee for the ~4% of production SearchKnowledge
// calls whose arguments are not a JSON object (flash emits a leaked tag or a bare
// query string instead of `{"query":"…"}`):
//
//  1. The tool result FED BACK to the agent keeps the "parameter parse error:"
//     contract prefix AND carries an explicit "re-emit valid JSON" hint, so the
//     next ReAct round can recover instead of stalling on a bare error.
//  2. The RECORDED StepError message (which becomes the trace error_class) stays
//     the concise parse error WITHOUT the hint — so error_class grouping in
//     production telemetry is byte-identical to before this change.
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

			// (1) Model-facing tool result: parse-error prefix + corrective JSON hint.
			assert.Truef(t, strings.HasPrefix(got, "parameter parse error:"),
				"returned result must keep the parse-error prefix, got %q", got)
			assert.Containsf(t, got, "JSON", "returned result must steer the agent to emit JSON, got %q", got)
			assert.Contains(t, got, "重新调用", "returned result must tell the agent to retry the call")

			// (2) Recorded telemetry (error_class): concise error, NOT the hint.
			require.Len(t, steps, 1)
			assert.Equal(t, StepError, steps[0].Type)
			assert.Equal(t, "SearchKnowledge", steps[0].Action)
			assert.Truef(t, strings.HasPrefix(steps[0].Message, "parameter parse error:"),
				"recorded message must be the concise parse error, got %q", steps[0].Message)
			assert.NotContains(t, steps[0].Message, "重新调用",
				"recorded error_class must NOT carry the corrective hint (telemetry stability)")
		})
	}
}
