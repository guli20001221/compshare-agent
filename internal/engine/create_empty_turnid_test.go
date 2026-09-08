package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/stretchr/testify/require"
)

// Each turn still has a transport identity, independent of argument semantics.
func createImageNameProposal(turnID, imageName string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "operation": "CreateInstanceWorkflow",
		"slots": []any{map[string]any{
			"name": "ImageName", "value": imageName,
		}},
	}
}

// TestChatBackfillsTurnIDWhenTransportEmpty is the regression guard: it drives the
// REAL turn entry with empty ChatOptions and
// asserts the compiled turn view got a non-empty identity. Before the fix this
// field is "" (the whole root cause); after the fix it is the ephemeral backfill.
// This is the test that would have caught the live failure — every existing
// proposal test injected a non-empty TurnID by hand and so never exercised it.
func TestChatBackfillsTurnIDWhenTransportEmpty(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	_, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{})

	require.NoError(t, err)
	require.NotEmpty(t, eng.turnContextViewThisTurn.TurnID,
		"a turn whose transport passed no TurnID must still be given a non-empty server-side identity")
}

// TestChatKeepsProvidedTurnID proves the fallback only fills the empty case.
func TestChatKeepsProvidedTurnID(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	_, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{TurnID: "turn-provided"})

	require.NoError(t, err)
	require.Equal(t, "turn-provided", eng.turnContextViewThisTurn.TurnID,
		"a provided turn id must not be overwritten by the ephemeral fallback")
}

// TestResolverResultParityAcrossTurnIDIdentity requires the same proposal result
// for caller-provided and engine-generated turn identities.
func TestResolverResultParityAcrossTurnIDIdentity(t *testing.T) {
	resolve := func(turnID string) actionresolver.ResolvedAction {
		eng := newZoneEngine(zoneCatalogExec(), "")
		const msg = "用 InfiniteTalk 创建一台实例"
		eng.lastUserMsg = msg
		eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, msg, turnID, time.Now())
		eng.turnContextViewReady = true
		resolved, err := eng.resolveActionProposal(zoneUserCtx(), createImageNameProposal(turnID, "InfiniteTalk"))
		require.NoError(t, err)
		return resolved.action
	}

	provided := resolve("turn-explicit")
	backfilled := resolve(newZoneEngine(zoneCatalogExec(), "").ephemeralTurnID())

	require.Equal(t, provided.ReadyForIntake, backfilled.ReadyForIntake, "same intake outcome regardless of turn-id origin")
	require.Empty(t, provided.RejectedProblems)
	require.Empty(t, backfilled.RejectedProblems)

}
