package intent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/tools"
)

// TestFailureAfterToolForError_DirectDispatchHint is the P0 阶段1B fix-forward
// guard: a *tools.UpstreamAPIError with a hinted RetCode returned by a
// direct-dispatched route (e.g. stock / gpu_specs / pricing) must reply with the
// recovery hint, NOT the generic "查询暂时失败". Without UpstreamAPIError.UserMessage
// + the non-empty guard in failureAfterToolForError, the hint was silently
// dropped on this path (it only reached the ReAct branch), which was the merged
// 阶段1B's scope gap.
func TestFailureAfterToolForError_DirectDispatchHint(t *testing.T) {
	const action = "DescribeAvailableCompShareInstanceTypes"
	args := map[string]any{"Zone": "cn-sh2-02"}

	t.Run("hinted code surfaces the hint as the reply", func(t *testing.T) {
		apiErr := tools.NewUpstreamAPIError(230, "Params [Zone] not available")
		got := failureAfterToolForError(action, args, "stock_availability", apiErr)

		if got.Status != HandlerStatusFailureAfterTool {
			t.Fatalf("status = %v, want FailureAfterTool", got.Status)
		}
		if got.FailureClass != HandlerFailureActionableUpstream {
			t.Fatalf("failure class = %q, want actionable_upstream", got.FailureClass)
		}
		if got.Reply != apiErr.Hint {
			t.Fatalf("reply = %q, want the recovery hint %q", got.Reply, apiErr.Hint)
		}
		if got.Reply == FriendlyToolFailureReply {
			t.Fatal("reply fell back to the generic failure text — hint was dropped")
		}
		if got.ToolAction != action {
			t.Errorf("ToolAction = %q, want %q (trace must still be attached)", got.ToolAction, action)
		}
		// The user-facing reply must not leak the raw upstream tokens.
		for _, tok := range []string{"RetCode", "not available", "CompShareImageId"} {
			if strings.Contains(got.Reply, tok) {
				t.Errorf("reply leaked forbidden token %q: %s", tok, got.Reply)
			}
		}
	})

	t.Run("every hinted code surfaces its hint end-to-end", func(t *testing.T) {
		// The path is code-identical across hinted codes, but assert each so a
		// future table change can't silently drop a code from the user reply.
		for _, code := range []int{230, 226604, 226603, 8433} {
			apiErr := tools.NewUpstreamAPIError(code, "upstream message")
			got := failureAfterToolForError(action, args, "stock_availability", apiErr)
			if got.Reply != apiErr.Hint || got.Reply == "" {
				t.Errorf("code %d: reply = %q, want non-empty hint %q", code, got.Reply, apiErr.Hint)
			}
			if got.Reply == FriendlyToolFailureReply {
				t.Errorf("code %d: fell back to generic reply — hint dropped", code)
			}
		}
	})

	t.Run("hint survives a wrapped error", func(t *testing.T) {
		// The saga / executor wrap the typed error with %w; errors.As must still
		// recover it so the direct-dispatch path is not the only one that loses it.
		wrapped := fmt.Errorf("步骤「检查库存」执行失败: %w", tools.NewUpstreamAPIError(226604, "out of resources"))
		got := failureAfterToolForError(action, args, "stock_availability", wrapped)
		want := tools.NewUpstreamAPIError(226604, "out of resources").Hint
		if got.Reply != want {
			t.Fatalf("reply = %q, want the 226604 hint %q", got.Reply, want)
		}
	})

	t.Run("un-hinted code falls back to the generic reply", func(t *testing.T) {
		apiErr := tools.NewUpstreamAPIError(17000, "some unhinted upstream error")
		got := failureAfterToolForError(action, args, "stock_availability", apiErr)
		if got.FailureClass != HandlerFailureGenericRead {
			t.Fatalf("failure class = %q, want generic_read", got.FailureClass)
		}
		if !strings.Contains(got.Reply, FriendlyToolFailureReply) {
			t.Fatalf("reply = %q, want it to contain the generic failure text %q", got.Reply, FriendlyToolFailureReply)
		}
	})

	t.Run("plain error falls back to the generic reply", func(t *testing.T) {
		got := failureAfterToolForError(action, args, "stock_availability", fmt.Errorf("boom"))
		if !strings.Contains(got.Reply, FriendlyToolFailureReply) {
			t.Fatalf("reply = %q, want the generic failure text", got.Reply)
		}
	})
}
