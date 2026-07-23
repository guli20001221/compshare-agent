package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A confirmation card is a genuine HUMAN GATE, not an advance authorization. For a
// concrete, existent target the Agent proposes — even one it self-elected for a
// zero-referent request like "怎么关机" — the server verifies existence and SHOWS a card
// naming the exact id, but:
//   - nothing mutates until the user confirms;
//   - a decline persists no selection;
//   - a confirm executes exactly the card's id.
//
// These replace the old "model self-election is refused" assertion. That test
// recompiled the turn context AFTER a mid-turn describe — a shape production never
// produces, since the AgentContext is an immutable turn-entry snapshot (Chat freezes
// it at turn entry). Whether the Agent SHOULD answer "怎么关机" instead of proposing a
// stop is a P7 behavioral concern, separate from the target-authorization layer; the
// human gate makes a stray self-elected proposal harmless either way.

func selfElectStopEngine(t *testing.T, confirm ConfirmFunc) (*Engine, *mockExecutorFn) {
	t.Helper()
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01",
			}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, confirm)
	eng.SetMutatingToolsEnabled(true)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	// A multi-instance account, nothing pre-selected: "怎么关机" references no instance,
	// so uhost-a is a pure Agent self-election.
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(2), "UHostSet": []any{
		map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running", "Zone": "cn-wlcb-01"},
		map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
	}}, "test"))
	eng.lastUserMsg = "怎么关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-selfelect", time.Now())
	eng.turnContextViewReady = true
	return eng, exec
}

// The card is generated for the self-elected existent target, naming the exact id.
func TestSelfElectedExistentTargetReachesConfirmationCard(t *testing.T) {
	eng, _ := selfElectStopEngine(t, func(string, map[string]any) bool { return true })

	resolved, err := eng.resolveActionProposalShadow(context.Background(),
		stopInstanceProposal("turn-selfelect", "uhost-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.NotNil(t, resolved.action.Confirmation)
	require.Equal(t, "uhost-a", resolved.action.Confirmation.Arguments["UHostId"],
		"the card names the exact id the human must confirm")
}

// Declined confirm: nothing mutates and no selection is persisted.
func TestSelfElectedTargetNotExecutedNorRecordedWhenDeclined(t *testing.T) {
	eng, exec := selfElectStopEngine(t, func(string, map[string]any) bool { return false })

	_ = eng.executeActionProposal(context.Background(),
		stopInstanceProposal("turn-selfelect", "uhost-a"), noopStep)

	require.NotContains(t, exec.calls, "StopCompShareInstance", "a declined card must not mutate")
	require.Empty(t, eng.sessionState.SelectedInstanceID, "a declined card must persist no selection")
}

// Confirmed: the workflow executes exactly the card's id.
func TestConfirmedSelfElectedTargetExecutesExactID(t *testing.T) {
	var stopArgs map[string]any
	eng, exec := selfElectStopEngine(t, func(string, map[string]any) bool { return true })
	base := exec.fn
	exec.fn = func(action string, args map[string]any) (map[string]any, error) {
		if action == "StopCompShareInstance" {
			stopArgs = args
		}
		return base(action, args)
	}

	out := eng.executeActionProposal(context.Background(),
		stopInstanceProposal("turn-selfelect", "uhost-a"), noopStep)

	require.Contains(t, out, "执行关机")
	require.Contains(t, exec.calls, "StopCompShareInstance")
	require.Equal(t, "uhost-a", stopArgs["UHostId"], "the workflow executes exactly the card's id")
}
