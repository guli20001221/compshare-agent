package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/stretchr/testify/require"
)

// The bug these tests lock: with durable_turns off, the legacy WS/HTTP transport
// passed no TurnID, so the per-turn AgentContext had an empty TurnID.
// deriveProposalProvenance stamped a standalone user_explicit field's evidence
// with MessageID="" (view.TurnID), and verifyCurrentQuestionEvidence rejects an
// empty MessageID FIRST — the server disowning its own evidence — producing a
// bogus `rejected:ImageName=unverified_source` that dead-ended the create card.
// The fix backfills an ephemeral turn-local id at ChatWithOptions entry so a turn
// always has an identity, without relaxing the resolver's trust boundary.

// createImageNameProposal is a CreateInstanceWorkflow proposal that names an image
// the user typed this turn as a standalone span. Nothing here should reject on our
// side; the only thing under test is whether the turn has an identity.
func createImageNameProposal(turnID, imageName string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "operation": "CreateInstanceWorkflow",
		"slots": []any{map[string]any{
			"name": "ImageName", "value": imageName, "source": "user_explicit",
			"evidence": map[string]any{"quote": imageName},
		}},
	}
}

// TestChatBackfillsTurnIDWhenTransportEmpty is the regression guard: it drives the
// REAL turn entry with the empty ChatOptions the legacy transport produced, and
// asserts the compiled turn view got a non-empty identity. Before the fix this
// field is "" (the whole root cause); after the fix it is the ephemeral backfill.
// This is the test that would have caught the live failure — every existing
// proposal test injected a non-empty TurnID by hand and so never exercised it.
func TestChatBackfillsTurnIDWhenTransportEmpty(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	_, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{ /* TurnID: "" — legacy transport passes none */ })

	require.NoError(t, err)
	require.NotEmpty(t, eng.turnContextViewThisTurn.TurnID,
		"a turn whose transport passed no TurnID must still be given a non-empty server-side identity")
}

// TestChatKeepsProvidedTurnID proves the backfill only fills the empty case: a
// durable turn's real identity must pass through untouched (never overwritten by
// the ephemeral id).
func TestChatKeepsProvidedTurnID(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	_, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{TurnID: "turn-durable-provided"})

	require.NoError(t, err)
	require.Equal(t, "turn-durable-provided", eng.turnContextViewThisTurn.TurnID,
		"a provided durable turn id must not be overwritten by the ephemeral backfill")
}

// TestEmptyTurnIDSelfRejectsStandaloneImageName is the root-cause characterization:
// with an empty turn identity, a standalone user_explicit ImageName the user really
// typed is rejected as unverified_source — the failure mode the backfill removes at
// the entry point.
func TestEmptyTurnIDSelfRejectsStandaloneImageName(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "")
	const msg = "用 InfiniteTalk 创建一台实例"
	eng.lastUserMsg = msg
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, msg, "", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), createImageNameProposal("", "InfiniteTalk"))
	require.NoError(t, err)

	require.True(t, hasRejection(resolved.action.RejectedProblems, "ImageName", actionresolver.RejectUnverifiedSource),
		"an empty turn identity makes the server reject its own current-turn evidence — the root cause the backfill fixes")
}

// TestBackfilledTurnIDLetsStandaloneImageNameReachCard is the payoff: with the
// engine's ephemeral turn id (what a transport-less turn now gets), the same
// standalone ImageName verifies as user_explicit and the create reaches the guided
// form instead of the unverified_source dead end.
func TestBackfilledTurnIDLetsStandaloneImageNameReachCard(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "")
	turnID := eng.ephemeralTurnID()
	require.NotEmpty(t, turnID)
	const msg = "用 InfiniteTalk 创建一台实例"
	eng.lastUserMsg = msg
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, msg, turnID, time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), createImageNameProposal(turnID, "InfiniteTalk"))
	require.NoError(t, err)

	require.False(t, hasRejection(resolved.action.RejectedProblems, "ImageName", actionresolver.RejectUnverifiedSource),
		"a real turn identity must not self-reject a standalone ImageName the user typed this turn")
	require.Equal(t, actionresolver.SourceUserExplicit, resolved.action.Provenance["ImageName"].Source,
		"the image name the user typed this turn is user_explicit and now verifies")
	require.True(t, resolved.action.ReadyForIntake,
		"the create opens the guided form for the still-missing GPU/zone, image pre-filled")
}

// TestResolverResultParityAcrossTurnIDIdentity is the durable/legacy parity guard:
// the SAME standalone-image create must resolve identically whether the turn id
// came from a durable transport or from the engine's ephemeral backfill — the fix
// removes the transport-dependent divergence.
func TestResolverResultParityAcrossTurnIDIdentity(t *testing.T) {
	resolve := func(turnID string) actionresolver.ResolvedAction {
		eng := newZoneEngine(zoneCatalogExec(), "")
		const msg = "用 InfiniteTalk 创建一台实例"
		eng.lastUserMsg = msg
		eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, msg, turnID, time.Now())
		eng.turnContextViewReady = true
		resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), createImageNameProposal(turnID, "InfiniteTalk"))
		require.NoError(t, err)
		return resolved.action
	}

	durable := resolve("turn-durable-explicit")
	backfilled := resolve(newZoneEngine(zoneCatalogExec(), "").ephemeralTurnID())

	require.Equal(t, durable.ReadyForIntake, backfilled.ReadyForIntake, "same intake outcome regardless of turn-id origin")
	require.Empty(t, durable.RejectedProblems)
	require.Empty(t, backfilled.RejectedProblems)
	require.Equal(t, durable.Provenance["ImageName"].Source, backfilled.Provenance["ImageName"].Source,
		"same re-derived source regardless of turn-id origin")
}

// TestBackfilledTurnIDDoesNotTrustAbsentImageName proves the fix is narrow: it
// gives the turn an identity but does NOT relax span verification. An image name
// that is NOT in the current message stays agent_inference (the model's honest
// guess that flows into the card to be resolved) — it is never promoted to a
// trusted user_explicit value just because the turn now has an id.
func TestBackfilledTurnIDDoesNotTrustAbsentImageName(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "")
	turnID := eng.ephemeralTurnID()
	const msg = "用 InfiniteTalk 创建一台实例" // does NOT contain "GhostImage"
	eng.lastUserMsg = msg
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, msg, turnID, time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), createImageNameProposal(turnID, "GhostImage"))
	require.NoError(t, err)

	require.Equal(t, actionresolver.SourceAgentInference, resolved.action.Provenance["ImageName"].Source,
		"a value absent from the current message stays agent_inference — the backfill fixes identity, not span trust")
	require.False(t, hasRejection(resolved.action.RejectedProblems, "ImageName", actionresolver.RejectUnverifiedSource),
		"an agent_inference non-target field flows into the card, it is not an unverified_source dead end")
}

func hasRejection(problems []actionresolver.RejectedProblem, slot string, kind actionresolver.RejectionKind) bool {
	for _, rp := range problems {
		if rp.Slot == slot && rp.Kind == kind {
			return true
		}
	}
	return false
}
