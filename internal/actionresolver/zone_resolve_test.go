package actionresolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

func twoZoneCatalog() *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, DisplayName: "华北一C"},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 2002, AzGroup: 3002}, DisplayName: "华东二B"},
	})
}

// TestCodecZoneActivatedForZoneWorkflows pins S5c activation: the schema-derived
// codec for a real "Zone" field is CodecZone (canonicalize against the live
// catalog), not plain constrained text — and all three zone-carrying operations
// need the catalog. Lifecycle operations are tested separately below because a
// workflow may explicitly consume zone facts without accepting a Zone field.
func TestCodecZoneActivatedForZoneWorkflows(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)

	for _, op := range []string{"CreateInstanceWorkflow", "CreateCFSWorkflow", "EnableNetOptimizerWorkflow"} {
		spec, ok := catalog.Lookup(op)
		require.True(t, ok, op)
		assert.Equal(t, CodecZone, spec.Fields["Zone"].Codec,
			"%s Zone must route through the live-catalog zone codec, not plain text", op)
		assert.True(t, SpecNeedsZoneCatalog(spec), "%s must trigger the zone-catalog fetch", op)
	}

	stop, ok := catalog.Lookup("StopInstanceWorkflow")
	require.True(t, ok)
	assert.False(t, SpecNeedsZoneCatalog(stop),
		"a workflow with no Zone field must not trigger the zone-catalog fetch")

	reinstall, ok := catalog.Lookup("ReinstallInstanceWorkflow")
	require.True(t, ok)
	assert.NotContains(t, reinstall.Fields, "Zone",
		"reinstall learns its placement from the selected instance, not the proposal")
	assert.True(t, reinstall.NeedsZoneCatalog,
		"the workflow must explicitly declare its execution-time zone dependency")
	assert.True(t, SpecNeedsZoneCatalog(reinstall),
		"reinstall must receive the same live snapshot used by zone-carrying operations")
}

// zoneOnlySpecResolver builds a resolver over a synthetic one-field spec whose
// Zone is a CodecZone. No shipping spec wires CodecZone until S4, so this is how
// S3 exercises the codec through the whole Resolve pipeline — the same way the
// machine-type codec is tested through the real catalog.
func zoneOnlySpecResolver(zoneCatalog *deployment.ZoneCatalogSnapshot) *Resolver {
	catalog := &Catalog{
		ordered: []string{"CreateInstanceWorkflow"},
		specs: map[string]OperationSpec{
			"CreateInstanceWorkflow": {
				Operation: "CreateInstanceWorkflow",
				Fields:    map[string]FieldSpec{"Zone": {Name: "Zone", Required: true, Codec: CodecZone}},
			},
		},
	}
	r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
	if zoneCatalog != nil {
		r = r.WithZoneCatalog(zoneCatalog)
	}
	return r
}

func zoneProposal(zone string) ActionProposal {
	return ActionProposal{
		TurnID: "turn-1", Operation: "CreateInstanceWorkflow",
		Slots: []SlotCandidate{{
			Name: "Zone", Value: zone, Source: SourceUserExplicit,
			Evidence: &SourceEvidence{MessageID: "turn-1", Quote: zone},
		}},
	}
}

func TestResolveCanonicalizesZoneByIdBeforeConfirmation(t *testing.T) {
	resolved := zoneOnlySpecResolver(twoZoneCatalog()).Resolve(zoneProposal("CN-BJ2-03"))

	require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
	assert.Equal(t, "cn-bj2-03", resolved.Arguments["Zone"], "the echoed case is folded to the canonical zone id")
	require.NotNil(t, resolved.Confirmation)
	assert.Equal(t, "cn-bj2-03", resolved.Confirmation.Arguments["Zone"],
		"the confirm card must show the exact zone id that will execute")
}

func TestResolveCanonicalizesZoneByDisplayName(t *testing.T) {
	resolved := zoneOnlySpecResolver(twoZoneCatalog()).Resolve(zoneProposal("华北一C"))

	require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
	assert.Equal(t, "cn-bj2-03", resolved.Arguments["Zone"],
		"a full console display name resolves to its zone id")
}

// TestResolveRejectsPartialOrKeywordZoneWithoutGuessing is the no-alias-table
// contract: a partial display name or a city keyword is NOT a zone. It must be
// rejected (the user's value is invalid), never guessed via substring or a
// keyword table — that is the guesswork this convergence removes.
func TestResolveRejectsPartialOrKeywordZoneWithoutGuessing(t *testing.T) {
	for _, raw := range []string{"华北一区", "华北一", "上海", "cn-bj2"} {
		resolved := zoneOnlySpecResolver(twoZoneCatalog()).Resolve(zoneProposal(raw))

		assert.False(t, resolved.ReadyForConfirmation, "%q must not resolve", raw)
		assert.NotEmpty(t, resolved.Rejected, "%q is an invalid value, not a match", raw)
		assert.Nil(t, resolved.Arguments["Zone"], "%q must not resolve to anything", raw)
		assert.Empty(t, resolved.DependencyFailures, "%q is the user's input, not our outage", raw)
		assert.NotContains(t, resolved.Missing, "Zone",
			"a value we rejected is not one the user withheld")
	}
}

// TestResolveReportsZoneCatalogOutageAsDependencyFailure pins the outage channel:
// no snapshot attached (or an unavailable one) is OUR failure, and must arrive as
// a DependencyFailure — never Rejected (blaming the user's value) and never
// passing the gate.
func TestResolveReportsZoneCatalogOutageAsDependencyFailure(t *testing.T) {
	// Two ways the catalog can be unavailable: never attached, or fetched-and-failed.
	cases := map[string]*Resolver{
		"never attached": zoneOnlySpecResolver(nil),
		"fetch failed":   zoneOnlySpecResolver(deployment.NewZoneCatalogSnapshot(false, nil)),
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			resolved := resolver.Resolve(zoneProposal("cn-bj2-03"))

			assert.False(t, resolved.ReadyForConfirmation, "an unconfirmable zone must never reach the gate")
			require.Len(t, resolved.DependencyFailures, 1)
			assert.Contains(t, resolved.DependencyFailures[0], "Zone")
			assert.Empty(t, resolved.Rejected, "a server-side outage must not be blamed on the user's value")
			assert.Nil(t, resolved.Arguments["Zone"])
			assert.Nil(t, resolved.Confirmation)
			assert.Empty(t, resolved.Missing,
				"our failed catalog query must never be reported as the user withholding a value")
		})
	}
}

func TestResolveReportsZoneAmbiguityAsConflict(t *testing.T) {
	// Two zones share a console display name — deciding which the user meant is a
	// question for them, not a guess for us.
	ambiguous := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-a-01"}, DisplayName: "同名区"},
		{Placement: deployment.ZonePlacement{Zone: "cn-b-02"}, DisplayName: "同名区"},
	})

	resolved := zoneOnlySpecResolver(ambiguous).Resolve(zoneProposal("同名区"))

	assert.False(t, resolved.ReadyForConfirmation)
	require.Len(t, resolved.Conflicts, 1)
	assert.Equal(t, "Zone", resolved.Conflicts[0].Slot)
	assert.ElementsMatch(t, []string{"cn-a-01", "cn-b-02"}, resolved.Conflicts[0].CatalogCandidates)
	assert.Nil(t, resolved.Arguments["Zone"])
	assert.Empty(t, resolved.Missing, "an ambiguous value was supplied — the question is which, not whether")
}

// TestResolveValidChineseZoneKeptOpensIntakeForm and its invalid twin exercise the
// zone codec through the REAL CreateInstanceWorkflow spec (guided intake), the link
// the zoneOnlySpec tests above (confirmation-only) do not cover: a VALID console
// zone is canonicalized and KEPT while the form still collects the GPU; a
// truly-INVALID zone is a form-correctable invalid value that opens the form to
// re-collect it (variance #2 for the zone codec).
func TestResolveValidChineseZoneKeptOpensIntakeForm(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{}).
		WithZoneCatalog(twoZoneCatalog())

	resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "Zone", Value: "华北一C", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "华北一C"}},
	}})

	require.False(t, resolved.ReadyForConfirmation, "GpuType is still missing")
	require.True(t, resolved.ReadyForIntake, "a valid zone + missing GPU opens the guided form")
	require.Equal(t, "cn-bj2-03", resolved.Arguments["Zone"],
		"华北一C is kept — canonicalized to its zone id, never dropped")
	require.Contains(t, resolved.Missing, "GpuType", "the form still needs the GPU")
	require.Empty(t, resolved.RejectedProblems, "a valid zone is not a rejection")
}

func TestResolveInvalidChineseZoneEntersIntakeForm(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{Names: []string{"4090"}, Available: true}).
		WithZoneCatalog(twoZoneCatalog())

	resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "4090"}},
		{Name: "Zone", Value: "华北一区", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "华北一区"}},
	}})

	require.False(t, resolved.ReadyForConfirmation)
	require.True(t, resolved.ReadyForIntake, "a truly-invalid zone is form-correctable — the form re-collects it")
	require.NotContains(t, resolved.Arguments, "Zone", "the invalid zone value is discarded, never carried forward")
	require.Equal(t, []RejectedProblem{{Slot: "Zone", Kind: RejectInvalidValue, Actor: RejectionActorUser}}, resolved.RejectedProblems,
		"华北一区 is a partial name (an invalid value), not a live zone")
}

func TestSpecNeedsZoneCatalog(t *testing.T) {
	withZone := OperationSpec{Fields: map[string]FieldSpec{
		"Zone":    {Codec: CodecZone},
		"GpuType": {Codec: CodecMachineType},
	}}
	assert.True(t, SpecNeedsZoneCatalog(withZone))

	explicitDependency := OperationSpec{
		Fields:           map[string]FieldSpec{"UHostId": {Codec: CodecResourceRef}},
		NeedsZoneCatalog: true,
	}
	assert.True(t, SpecNeedsZoneCatalog(explicitDependency),
		"a workflow may consume zone facts after resolving its resource target")

	withoutZone := OperationSpec{Fields: map[string]FieldSpec{"UHostId": {Codec: CodecResourceRef}}}
	assert.False(t, SpecNeedsZoneCatalog(withoutZone),
		"an operation with no CodecZone field must not trigger the zone fetch")
}
