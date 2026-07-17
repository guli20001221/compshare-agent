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

func TestSpecNeedsZoneCatalog(t *testing.T) {
	withZone := OperationSpec{Fields: map[string]FieldSpec{
		"Zone":    {Codec: CodecZone},
		"GpuType": {Codec: CodecMachineType},
	}}
	assert.True(t, SpecNeedsZoneCatalog(withZone))

	withoutZone := OperationSpec{Fields: map[string]FieldSpec{"UHostId": {Codec: CodecResourceRef}}}
	assert.False(t, SpecNeedsZoneCatalog(withoutZone),
		"an operation with no CodecZone field must not trigger the zone fetch")
}
