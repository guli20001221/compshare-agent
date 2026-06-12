package engine

import (
	"slices"
	"testing"

	"github.com/compshare-agent/internal/intent"
)

// TestSpecForIntent_MatchesExistingSurfaces pins specForIntent as a faithful
// projection: for every intent it must reproduce the three live surfaces it
// claims to mirror. Iterating intent.AllIntents() (wider than RuntimeIntents() —
// it includes the legacy/mixed intents and deploy_model) ensures a future intent
// added to any surface cannot drift from the spec without this test failing.
// This parity is the whole point of PR1: downstream PRs may read routing truth
// from the spec ONLY because this proves the spec equals the surfaces today.
func TestSpecForIntent_MatchesExistingSurfaces(t *testing.T) {
	for _, i := range intent.AllIntents() {
		spec := specForIntent(i)

		if spec.Intent != i {
			t.Errorf("specForIntent(%q).Intent = %q, want %q", i, spec.Intent, i)
		}
		if want := intent.PlannedExecutionPathForIntent(i); spec.NominalLane != want {
			t.Errorf("specForIntent(%q).NominalLane = %q, want %q", i, spec.NominalLane, want)
		}
		if want := intent.IntentToolSubset(i); !slices.Equal(spec.ToolSubset, want) {
			t.Errorf("specForIntent(%q).ToolSubset = %v, want %v", i, spec.ToolSubset, want)
		}
		if want := agentSkillForIntent[i]; spec.AgentSkillName != want {
			t.Errorf("specForIntent(%q).AgentSkillName = %q, want %q", i, spec.AgentSkillName, want)
		}
	}
}

// TestSpecForIntent_ReturnsDefensiveToolSubsetCopy proves the projected
// ToolSubset is a copy, not an alias of the live surface: mutating the returned
// slice must not corrupt intent.IntentToolSubset for the same intent. The spec is
// a projection of shared routing truth, so a caller holding it must not be able
// to rewrite the subset every other consumer reads.
func TestSpecForIntent_ReturnsDefensiveToolSubsetCopy(t *testing.T) {
	// IntentDiagnosis has a non-empty subset, so the mutation below is observable.
	const probe = intent.IntentDiagnosis
	if len(intent.IntentToolSubset(probe)) == 0 {
		t.Fatalf("precondition: IntentToolSubset(%q) is empty; pick an intent with a subset", probe)
	}

	spec := specForIntent(probe)
	if len(spec.ToolSubset) == 0 {
		t.Fatalf("specForIntent(%q).ToolSubset is empty", probe)
	}
	spec.ToolSubset[0] = "MUTATED-BY-TEST"

	if got := intent.IntentToolSubset(probe); got[0] == "MUTATED-BY-TEST" {
		t.Errorf("mutating spec.ToolSubset corrupted intent.IntentToolSubset(%q): got %v", probe, got)
	}
}
