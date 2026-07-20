package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/actionresolver"
)

// TestResolveCreateWithValidChineseZoneEndToEnd drives the full Chinese create
// sentence through the REAL production path (resolveActionProposalShadow):
// provenance derivation + the live zone-catalog snapshot + the resolver. The valid
// console zone "华北一C" is canonicalized and KEPT; the create opens the guided form
// for the still-missing GPU. This is the end-to-end "valid 华北一C keeps its
// selection" acceptance (the resolver-level twin lives in internal/actionresolver).
func TestResolveCreateWithValidChineseZoneEndToEnd(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "")
	eng.lastUserMsg = "在华北一C用最新的PyTorch镜像帮我开一台4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cn-zone", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), map[string]any{
		"turn_id": "turn-cn-zone", "operation": "CreateInstanceWorkflow",
		"slots": []any{map[string]any{
			"name": "Zone", "value": "华北一C", "source": "user_explicit",
			"evidence": map[string]any{"quote": "华北一C"},
		}},
	})

	require.NoError(t, err)
	require.Equal(t, "cn-bj2-03", resolved.action.Arguments["Zone"],
		"the valid Chinese console zone is canonicalized and kept through the production path")
	require.True(t, resolved.action.ReadyForIntake,
		"the create opens the guided form for the still-missing GPU, zone pre-filled")
	require.Empty(t, resolved.action.RejectedProblems, "a valid zone is not rejected")
	require.Empty(t, resolved.action.DependencyFailures, "the zone catalog resolved; no outage")
}

// TestResolveCreateWithInvalidChineseZoneEntersFormEndToEnd is the end-to-end
// "truly-invalid zone enters the form": a partial console name ("华北一区" is NOT the
// zone "华北一C") is a form-correctable invalid value, so the create discards it and
// opens the guided form to re-collect the zone — it never silently guesses a zone
// or drops the request.
func TestResolveCreateWithInvalidChineseZoneEntersFormEndToEnd(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "")
	eng.lastUserMsg = "在华北一区帮我开一台4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cn-zone-bad", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), map[string]any{
		"turn_id": "turn-cn-zone-bad", "operation": "CreateInstanceWorkflow",
		"slots": []any{map[string]any{
			"name": "Zone", "value": "华北一区", "source": "user_explicit",
			"evidence": map[string]any{"quote": "华北一区"},
		}},
	})

	require.NoError(t, err)
	require.NotContains(t, resolved.action.Arguments, "Zone", "the invalid zone value is discarded, never carried forward")
	require.True(t, resolved.action.ReadyForIntake, "a truly-invalid zone opens the form to re-collect it")
	require.Equal(t, []actionresolver.RejectedProblem{{Slot: "Zone", Kind: actionresolver.RejectInvalidValue}},
		resolved.action.RejectedProblems, "华北一区 is a partial name (an invalid value), not a live zone")
}
