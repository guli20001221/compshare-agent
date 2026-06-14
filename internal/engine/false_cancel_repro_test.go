package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/llm"

	openai "github.com/sashabaranov/go-openai"
)

// TestFalseCancel_UnresolvedConfirmRendersHonestNotExecuted locks the fix for
// the top non-upstream production failure cluster (P0/P1): a confirm that was
// not granted must NOT be narrated as "好的，已取消关机操作。" (falsely claiming
// the user cancelled).
//
// Real production transcript that motivated this (console agent, single turn):
//
//	[user] 帮我关闭下实例[INSTANCE_ID]
//	[assistant] 好的，已取消关机操作。   <-- the lie this test forbids
//
// Root cause: the console confirm card is resolved by a button click
// (ConfirmCSAgentAction). When the user does not click — they let it time out,
// or they type in the (unlocked) chat box instead — httpapi.WaitForConfirmation
// returns the ZERO ConfirmDecision{} (Confirmed:false), indistinguishable from
// an explicit decline. The workflow stamps "用户取消了操作" and engine.go used to
// render "好的，已取消关机操作。". A confirmFn returning false is exactly what that
// unresolved gate produces.
//
// The fix renders an honest, non-accusatory, recoverable message instead.
func TestFalseCancel_UnresolvedConfirmRendersHonestNotExecuted(t *testing.T) {
	// DescribeCompShareInstance returns one Running instance so the stop
	// workflow reaches its 确认关机 gate.
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"TotalCount": float64(1),
				"UHostSet": []any{map[string]any{
					"UHostId":    "uhost-falsecancel",
					"Name":       "host-dhtest",
					"State":      "Running",
					"Zone":       "cn-sh2-02",
					"GpuType":    "2080Ti",
					"ChargeType": "Postpay",
				}},
			},
			"StopCompShareInstance": {"RetCode": 0},
		},
	}

	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("StopInstanceWorkflow", map[string]any{"UHostId": "uhost-falsecancel"})}},
			{Content: "narration-should-be-skipped"},
		},
	}

	// confirmFn returning false === what WaitForConfirmation yields on
	// timeout / disconnect / no-resolve (the zero ConfirmDecision{}).
	eng := NewWithDeps(mock, exec, func(action string, args map[string]any) bool {
		return false
	})
	eng.Init(context.Background())

	reply, err := eng.Chat(context.Background(), "帮我关闭下实例uhost-falsecancel", noopStep)
	require.NoError(t, err)

	// The fix: an unresolved confirm must NEVER falsely claim the user cancelled.
	assert.NotContains(t, reply, "已取消关机操作",
		"unresolved confirm must not be narrated as a user cancellation")
	// It must honestly say the action was not executed.
	assert.Contains(t, reply, "未执行",
		"unresolved confirm must honestly report the action was not executed")

	// Safety invariant: no shutdown was executed.
	for _, c := range exec.calls {
		assert.NotEqual(t, "StopCompShareInstance", c,
			"must never execute the shutdown when the confirm was not granted")
	}
}
