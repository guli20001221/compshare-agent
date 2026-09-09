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
	args := map[string]any{"UHostId": "uhost-1", "Task": "repair service"}
	first, ok := tools.ParseAgentToolResult(eng.executeInstanceOpsInvocation(context.Background(), "DiagnoseInstanceInternals", args, "first", noopStep))
	require.True(t, ok)
	firstData := first.Data.(map[string]any)
	require.Equal(t, float64(1), firstData["commands_ran"])
	require.Equal(t, false, firstData["run_completed"])
	require.Contains(t, firstData["report"], "first invocation mutation")

	// A retry that fails before any callback must not borrow the first run's
	// pending interruption notice and claim it executed those commands again.
	runner.progress = nil
	second, ok := tools.ParseAgentToolResult(eng.executeInstanceOpsInvocation(context.Background(), "DiagnoseInstanceInternals", args, "second", noopStep))
	require.True(t, ok)
	secondData := second.Data.(map[string]any)
	require.Equal(t, float64(0), secondData["commands_ran"])
	require.NotContains(t, secondData["report"], "first invocation mutation")
	require.Equal(t, "interrupted", second.Meta.SourceStatus)

	runner.err = ErrInstanceOpsSSHPreflightUnreachable
	third, ok := tools.ParseAgentToolResult(eng.executeInstanceOpsInvocation(context.Background(), "DiagnoseInstanceInternals", args, "third", noopStep))
	require.True(t, ok)
	thirdData := third.Data.(map[string]any)
	require.Equal(t, false, thirdData["guest_commands_executed"])
	require.NotContains(t, thirdData, "report")
}
