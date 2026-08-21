package engine

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// harnessDispositionForTerminalReason mirrors the ONE translation this repo performs in Python:
// deploy/ssh_ops_harness/harness.py's _CONFIRMATION_REFUSAL_DISPOSITIONS, which turns the reply
// field the Go supervisor puts on the wire into the @@STEP `reason` the Go engine reads back.
//
// It is a mirror on purpose. The per-command confirmation crosses the language boundary twice —
// Go writes terminal_reason, Python answers with a disposition — and neither end could previously
// fail on its own if the other end changed, because each was only ever tested against itself.
// TestHarnessAndEngineAgreeOnEveryConfirmationDisposition pins this table against the real file, so
// a rename on either side breaks here rather than in production.
func harnessDispositionForTerminalReason(reason string) string {
	switch reason {
	case observability.ConfirmationReasonUserDeclined:
		return "refused_user_declined"
	case observability.ConfirmationReasonTimeout:
		return "refused_confirmation_timeout"
	case observability.ConfirmationReasonClientDisconnect:
		return "refused_client_disconnect"
	case observability.ConfirmationReasonDeliveryFailed:
		return "refused_confirmation_delivery_failed"
	case observability.ConfirmationReasonBrokerCancelled:
		return "refused_confirmation_broker_cancelled"
	default:
		// What the harness answers when the field is absent or unknown — a supervisor older than
		// it, or newer. It can prove only that no approval arrived.
		return "refused_not_approved"
	}
}

// confirmingInstanceOpsRunner stands in for the subprocess+SSH runner at the seam that matters: it
// asks about one write, then reports the command the way the harness would have reported it given
// the answer it got. A runner that ignored ConfirmWrite's reason would produce the generic
// disposition and the generic sentence, which is exactly the regression under test.
type confirmingInstanceOpsRunner struct {
	command  string
	gotReply ConfirmationResult
	asked    int
}

func (r *confirmingInstanceOpsRunner) Run(_ context.Context, req InstanceOpsRequest, onProgress func(InstanceOpsProgress)) (InstanceOpsVerdict, error) {
	if req.ConfirmWrite == nil {
		return InstanceOpsVerdict{Text: "结论：写确认回调未接线"}, nil
	}
	r.asked++
	r.gotReply = req.ConfirmWrite(r.command)
	if r.gotReply.Confirmed {
		onProgress(InstanceOpsProgress{
			Kind: InstanceOpsProgressCommand, Command: r.command, Disposition: "ran",
		})
		return InstanceOpsVerdict{Text: "结论：已修复", Ran: 1}, nil
	}
	onProgress(InstanceOpsProgress{
		Kind:        InstanceOpsProgressCommand,
		Command:     r.command,
		Disposition: "refused",
		Reason:      harnessDispositionForTerminalReason(r.gotReply.TerminalReason),
	})
	return InstanceOpsVerdict{Text: "结论：未修复", Refused: 1}, nil
}

// The per-command write card has the same four causes as the lane entry card, and until now only
// the entry card's wording was pinned (TestInstanceOps_UnauthorizedReplyNamesTheActualCause). The
// per-command half carried the reason through a line no test read: blanking
// `TerminalReason: e.lastConfirmationTerminalReason` in executeInstanceOps left the entire suite
// green while every write refusal silently degraded to 「未收到对这条命令的确认」.
//
// Driven through Chat so the whole chain runs: ConfirmResultFunc -> the per-turn confirmation
// wrapper -> the field -> the ConfirmWrite closure -> what the runner (standing in for the harness)
// answers with -> the step line the user reads. Setting the field by hand, or asserting only on the
// ConfirmationResult, would keep passing if any hop in between stopped carrying it.
func TestInstanceOps_PerCommandRefusalNamesTheActualCause(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reason      string
		wantContain string
		wantAbsent  string
	}{
		{"declined", observability.ConfirmationReasonUserDeclined, "你未批准这条命令", "超时"},
		{"timeout", observability.ConfirmationReasonTimeout, "等待你的确认超过 120 秒", "未批准"},
		{"disconnect", observability.ConfirmationReasonClientDisconnect, "连接已断开", "未批准"},
		{"undeliverable", observability.ConfirmationReasonDeliveryFailed, "确认卡片未能送达", "未批准"},
		{"cancelled", observability.ConfirmationReasonBrokerCancelled, "确认请求已取消", "未批准"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &confirmingInstanceOpsRunner{command: "systemctl restart comfyui"}
			model := &mockLLM{responses: []llm.ChatResponse{
				{ToolCalls: []openai.ToolCall{toolCall("t1", "DiagnoseInstanceInternals",
					`{"UHostId":"uhost-1","Task":"修复 ComfyUI"}`)}},
				{Content: "done"},
			}}
			eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
			eng.SetInstanceOps(runner)

			var steps []StepEvent
			_, err := eng.ChatWithOptions(context.Background(), "请修复 uhost-1 的 ComfyUI", captureSteps(&steps), ChatOptions{
				// The lane card is approved; only the per-command card ends the way this case
				// describes. Answering both the same way would let an entry-card regression pass
				// as a per-command success.
				ConfirmResultFunc: func(action string, _ map[string]any) ConfirmationResult {
					if action == instanceOpsWriteAction {
						return ConfirmationResult{TerminalReason: tc.reason}
					}
					return ConfirmationResult{Confirmed: true, TerminalReason: observability.ConfirmationReasonUserConfirmed}
				},
			})
			require.NoError(t, err)
			require.Equal(t, 1, runner.asked, "the write must have been put to a card")
			require.False(t, runner.gotReply.Confirmed, "an unapproved write must never come back approved")
			require.Equal(t, tc.reason, runner.gotReply.TerminalReason,
				"the harness has to learn WHY the card ended, not just that it did")

			var blocked string
			for _, s := range steps {
				if s.Type == StepBlocked && strings.Contains(s.Message, runner.command) {
					blocked = s.Message
				}
			}
			require.NotEmpty(t, blocked, "the refused command must appear in the activity stream")
			require.Contains(t, blocked, tc.wantContain)
			require.NotContains(t, blocked, tc.wantAbsent)
			require.Contains(t, blocked, "命令未执行", "every branch must say the box was untouched")
		})
	}
}

// An approval must still carry through unchanged. The wrapper writes the terminal reason on the
// approve path too, so a bug that mixed the two up would show here rather than as a write that
// quietly did not happen.
func TestInstanceOps_ApprovedWriteStillRuns(t *testing.T) {
	runner := &confirmingInstanceOpsRunner{command: "systemctl restart comfyui"}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("t1", "DiagnoseInstanceInternals",
			`{"UHostId":"uhost-1","Task":"修复 ComfyUI"}`)}},
		{Content: "done"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)

	_, err := eng.ChatWithOptions(context.Background(), "请修复 uhost-1 的 ComfyUI", noopStep, ChatOptions{
		ConfirmResultFunc: func(string, map[string]any) ConfirmationResult {
			return ConfirmationResult{Confirmed: true, TerminalReason: observability.ConfirmationReasonUserConfirmed}
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, runner.asked)
	require.True(t, runner.gotReply.Confirmed, "an approved write must reach the harness as approved")
}

// The Go engine and the Python harness each hold half of the confirmation-disposition translation,
// and each was previously tested only against its own half. This reads the real table out of
// harness.py and requires that every disposition it can emit has its own sentence here.
//
// A disposition the engine does not know does not fail loudly: instanceOpsRefusalReason falls
// through to 「属于高危操作或命令形式不被接受」, so a forgotten entry shows up as a user being told
// their timed-out card was a dangerous command.
func TestHarnessAndEngineAgreeOnEveryConfirmationDisposition(t *testing.T) {
	src, err := os.ReadFile("../../deploy/ssh_ops_harness/harness.py")
	require.NoError(t, err, "the harness is part of the same deploy; a moved file must fail here")

	block := regexp.MustCompile(`(?s)_CONFIRMATION_REFUSAL_DISPOSITIONS\s*=\s*\{(.*?)\}`).FindSubmatch(src)
	require.Len(t, block, 2,
		"could not find _CONFIRMATION_REFUSAL_DISPOSITIONS in harness.py — this test must fail loudly "+
			"rather than silently verify an empty set")

	pairs := regexp.MustCompile(`"([a-z_]+)"\s*:\s*"([a-z_]+)"`).FindAllStringSubmatch(string(block[1]), -1)
	require.NotEmpty(t, pairs, "the mapping block parsed to zero entries")

	fallback := instanceOpsRefusalReason("refused_something_added_later")
	declined := instanceOpsRefusalReason("refused_user_declined")
	for _, p := range pairs {
		terminalReason, disposition := p[1], p[2]

		require.Equal(t, disposition, harnessDispositionForTerminalReason(terminalReason),
			"harness.py maps %q -> %q; the mirror in this package disagrees", terminalReason, disposition)

		got := instanceOpsRefusalReason(disposition)
		require.NotEqual(t, fallback, got,
			"%q reaches the engine with no sentence of its own and degrades to the collapsed one", disposition)
		if disposition != "refused_user_declined" {
			require.NotEqual(t, declined, got,
				"%q renders identically to an explicit decline", disposition)
		}
	}

	// Every reason the transport can produce for a NON-approval must be in that table. A reason the
	// harness has no entry for silently becomes refused_not_approved, which is the pre-fix behaviour.
	inTable := map[string]bool{}
	for _, p := range pairs {
		inTable[p[1]] = true
	}
	for _, reason := range []string{
		observability.ConfirmationReasonUserDeclined,
		observability.ConfirmationReasonTimeout,
		observability.ConfirmationReasonClientDisconnect,
		observability.ConfirmationReasonDeliveryFailed,
		observability.ConfirmationReasonBrokerCancelled,
	} {
		require.True(t, inTable[reason],
			"the transport can end a card with %q and harness.py has no entry for it", reason)
	}
}

// The confirmation table is only one producer of refused reasons. Structured tools also emit
// refused_precondition when a hash is stale, a match is ambiguous or a job id cannot be resolved.
// Parse the complete wire map so adding any future refusal in Python without user-facing wording in
// Go fails here instead of degrading to the generic "high risk or bad command form" sentence.
func TestHarnessAndEngineAgreeOnEveryRefusalDisposition(t *testing.T) {
	src, err := os.ReadFile("../../deploy/ssh_ops_harness/harness.py")
	require.NoError(t, err, "the harness is part of the same deploy; a moved file must fail here")

	block := regexp.MustCompile(`(?s)_DISPOSITION_MAP\s*=\s*\{(.*?)\}`).FindSubmatch(src)
	require.Len(t, block, 2,
		"could not find _DISPOSITION_MAP in harness.py — this test must fail loudly rather than verify an empty set")

	pairs := regexp.MustCompile(`"([a-z_]+)"\s*:\s*"([a-z_]+)"`).FindAllStringSubmatch(string(block[1]), -1)
	require.NotEmpty(t, pairs, "the disposition map parsed to zero entries")
	fallback := instanceOpsRefusalReason("refused_something_added_later")
	seenRefusal := false
	for _, pair := range pairs {
		if pair[2] != "refused" {
			continue
		}
		seenRefusal = true
		require.True(t, strings.HasPrefix(pair[1], "refused_"),
			"a wire refusal should carry a refusal-shaped reason, got %q", pair[1])
		require.NotEqual(t, fallback, instanceOpsRefusalReason(pair[1]),
			"%q is emitted as refused but has no actionable engine wording", pair[1])
	}
	require.True(t, seenRefusal, "the wire map contains no refused dispositions")
}
