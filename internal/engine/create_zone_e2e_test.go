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

// TestChineseCreateProvenanceReproduction is the reproduction gate the lead asked
// for: settle empirically whether the Chinese create sentence can produce an
// unverified_source rejection at HEAD. deriveProposalProvenance resets every
// model-claimed source and re-derives it with the SAME matcher the resolver later
// verifies with, so the expected outcome is that a value EITHER becomes
// user_explicit-that-verifies OR stays agent_inference-that-bypasses — never
// user_explicit-that-fails-verification. It records (value-free) the re-derived
// source + rejection kind per slot for audit, and asserts the EXACT re-derived
// source of each field so an all-degrade-to-agent_inference regression cannot pass
// silently. A non-standalone span does NOT reach unverified_source — it stays
// agent_inference — so if a LIVE gate ever shows unverified_source, suspect the run
// is not this commit, the process was not restarted, a different proposal entry was
// taken, the source was re-modified after re-derivation, or the diagnostic is from
// an older round — NOT this path.
func TestChineseCreateProvenanceReproduction(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := newZoneEngine(exec, "")
	const sentence = "在华北一C用最新的 PyTorch 镜像帮我开一台 4090"
	eng.lastUserMsg = sentence
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, sentence, "turn-repro", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(zoneUserCtx(), map[string]any{
		"turn_id": "turn-repro", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090", "source": "user_explicit", "evidence": map[string]any{"quote": "4090"}},
			map[string]any{"name": "ImageName", "value": "PyTorch", "source": "user_explicit", "evidence": map[string]any{"quote": "PyTorch"}},
			map[string]any{"name": "Zone", "value": "华北一C", "source": "user_explicit", "evidence": map[string]any{"quote": "华北一C"}},
		},
	})
	require.NoError(t, err)

	// Value-free audit: the re-derived source label + whether the value survived.
	for _, slot := range []string{"GpuType", "ImageName", "Zone"} {
		src := "<absent>"
		if p, ok := resolved.action.Provenance[slot]; ok {
			src = string(p.Source)
		}
		t.Logf("slot=%s re-derived_source=%s in_arguments=%t", slot, src, resolved.action.Arguments[slot] != nil)
	}
	for _, rp := range resolved.action.RejectedProblems {
		t.Logf("rejected slot=%s kind=%s", rp.Slot, rp.Kind.String())
	}

	// Assert the EXACT re-derived source per field, not just "no unverified_source":
	// GpuType/ImageName are space-delimited standalone spans → user_explicit; Zone
	// "华北一C" is CJK-adjacent → agent_inference (bypasses the span check, then
	// catalog-resolves). Without this, a regression that degraded every field to
	// agent_inference — or wrongly marked one user_explicit-that-fails — would still
	// pass the no-unverified_source check.
	wantSource := map[string]actionresolver.CandidateSource{
		"GpuType":   actionresolver.SourceUserExplicit,
		"ImageName": actionresolver.SourceUserExplicit,
		"Zone":      actionresolver.SourceAgentInference,
	}
	for slot, want := range wantSource {
		p, ok := resolved.action.Provenance[slot]
		if !ok {
			t.Errorf("slot %s absent from provenance; want source %s", slot, want)
			continue
		}
		if p.Source != want {
			t.Errorf("slot %s re-derived source = %s, want %s", slot, p.Source, want)
		}
	}

	for _, rp := range resolved.action.RejectedProblems {
		if rp.Kind == actionresolver.RejectUnverifiedSource {
			t.Errorf("unverified_source reproduced on %s — the Chinese-boundary hypothesis would hold after all", rp.Slot)
		}
	}
	require.True(t, resolved.action.ReadyForConfirmation || resolved.action.ReadyForIntake,
		"the clean Chinese create sentence must reach a card, not a rejection dead-end")
}
