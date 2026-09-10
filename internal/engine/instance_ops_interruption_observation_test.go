package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestInterruptedInvocationReturnsItsOwnSettledWorkToParent(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: context.Canceled, progress: []InstanceOpsProgress{{
		Kind: InstanceOpsProgressCommand, Command: "first invocation mutation", Tier: "mutating", Disposition: "ran",
	}}}
	eng := newInstanceOpsEngine(runner, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	// Identical arguments on every attempt, reaching the lane the way the ReAct
	// loop does: a retry after an interruption is the model repeating itself, and
	// each attempt must report its own run.
	const args = `{"UHostId":"uhost-1","Task":"repair service"}`
	first, ok := tools.ParseAgentToolResult(execToolInTurn(eng, toolCall("first", "DiagnoseInstanceInternals", args), noopStep))
	require.True(t, ok)
	firstData := first.Data.(map[string]any)
	require.Equal(t, float64(1), firstData["commands_ran"])
	require.Equal(t, false, firstData["run_completed"])
	require.Contains(t, firstData["report"], "first invocation mutation")

	// A retry that fails before any callback must not borrow the first run's
	// pending interruption notice and claim it executed those commands again.
	runner.progress = nil
	second, ok := tools.ParseAgentToolResult(execToolInTurn(eng, toolCall("second", "DiagnoseInstanceInternals", args), noopStep))
	require.True(t, ok)
	secondData := second.Data.(map[string]any)
	require.Equal(t, float64(0), secondData["commands_ran"])
	require.NotContains(t, secondData["report"], "first invocation mutation")
	require.Equal(t, "interrupted", second.Meta.SourceStatus)

	require.Equal(t, 2, runner.calls, "the retry re-entered rather than replaying the first attempt")
}

// A failure before the instance is reached carries no report at all: there is
// nothing to tell the user about a box that was never entered.
func TestPreflightFailureReportsNoGuestWork(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsSSHPreflightUnreachable}
	eng := newInstanceOpsEngine(runner, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}

	out, ok := tools.ParseAgentToolResult(execToolInTurn(eng, toolCall("only", "DiagnoseInstanceInternals",
		`{"UHostId":"uhost-1","Task":"repair service"}`), noopStep))
	require.True(t, ok)
	data := out.Data.(map[string]any)
	require.Equal(t, false, data["guest_commands_executed"])
	require.NotContains(t, data, "report")
}
