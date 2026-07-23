package actionresolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

func twoImageCatalog() *deployment.ImageCatalogSnapshot {
	return deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{
		{ID: "img-torch", Name: "PyTorch", Source: "platform", Status: "Available", Container: true},
		{ID: "img-ubuntu", Name: "Ubuntu 22.04", Source: "platform", Status: "Available", Container: true},
	})
}

// TestCodecImageActivatedForImageBearingOps pins that the schema-derived codec for
// a real CompShareImageId field is CodecImage (verify the explicit id against the
// live catalog), not plain constrained text. Both reinstall and create expose an
// explicit id, so an id either flow receives is catalog-verified or refused —
// invariant 1 holds at the same boundary for both.
//
// Create gained its id field deliberately. ImageName reaches upstream as
// FuzzySearch, so a wording the platform does not match returns zero rows and the
// flow dead-ends on an image that exists; the model sees real ids in a read
// observation's evidence.subjects[] and can now pass one instead of a phrase.
//
// This test previously asserted the opposite, with a note that flipping it would
// let a stale proposal-time snapshot shadow the guided re-query — "fix the flow
// first". The flow was fixed first: workflow.createImageCatalog now prefers THIS
// run's 查询镜像 over any engine-threaded snapshot, so the source switch and the
// name-miss browse rescue can no longer be overridden. That behaviour is gated by
// TestCreateImageCatalogPrefersThisRunsQueryOverAnInjectedSnapshot, which is where
// the protection now lives — this assertion alone no longer carries it.
func TestCodecImageActivatedForImageBearingOps(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)

	reinstall, ok := catalog.Lookup("ReinstallInstanceWorkflow")
	require.True(t, ok)
	assert.Equal(t, CodecImage, reinstall.Fields["CompShareImageId"].Codec,
		"reinstall CompShareImageId must route through the live-catalog image codec, not plain text")
	assert.True(t, SpecNeedsImageCatalog(reinstall), "reinstall must trigger the image-catalog fetch")

	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	createField, hasIDField := create.Fields["CompShareImageId"]
	require.True(t, hasIDField, "create must accept a concrete image id, not only a fuzzy name")
	assert.Equal(t, CodecImage, createField.Codec,
		"an id the model supplies must be verified against the live catalog, never passed through")
	assert.True(t, SpecNeedsImageCatalog(create),
		"…which requires the catalog, fetched only when a proposal actually names an id")

	stop, ok := catalog.Lookup("StopInstanceWorkflow")
	require.True(t, ok)
	assert.False(t, SpecNeedsImageCatalog(stop),
		"a workflow with no CompShareImageId field must not trigger the image-catalog fetch")

	clone, ok := catalog.Lookup("CloneCustomImageWorkflow")
	require.True(t, ok)
	assert.Equal(t, CodecImage, clone.Fields["CompShareImageId"].Codec)
	assert.Equal(t, "custom", clone.ImageCatalogSource,
		"the capability, not a model-authored constant or engine name switch, owns the fixed source")
	assert.True(t, SpecNeedsImageCatalog(clone))
}

func imageOnlySpecResolver(imageCatalog *deployment.ImageCatalogSnapshot) *Resolver {
	catalog := &Catalog{
		ordered: []string{"CreateInstanceWorkflow"},
		specs: map[string]OperationSpec{
			"CreateInstanceWorkflow": {
				Operation: "CreateInstanceWorkflow",
				Fields:    map[string]FieldSpec{"CompShareImageId": {Name: "CompShareImageId", Required: true, Codec: CodecImage}},
			},
		},
	}
	r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
	if imageCatalog != nil {
		r = r.WithImageCatalog(imageCatalog)
	}
	return r
}

func imageIDProposal(id string) ActionProposal {
	return ActionProposal{
		TurnID: "turn-1", Operation: "CreateInstanceWorkflow",
		Slots: []SlotCandidate{{
			Name: "CompShareImageId", Value: id, Source: SourceUserExplicit,
			Evidence: &SourceEvidence{MessageID: "turn-1", Quote: id},
		}},
	}
}

func TestResolveVerifiesImageIDAgainstCatalog(t *testing.T) {
	resolved := imageOnlySpecResolver(twoImageCatalog()).Resolve(imageIDProposal("img-torch"))

	require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
	assert.Equal(t, "img-torch", resolved.Arguments["CompShareImageId"],
		"a catalog-verified id passes through unchanged")
}

// TestResolveRejectsUnverifiedImageID is invariant 1 at the proposal boundary: an
// id the catalog does not contain is an invalid value, rejected — never passed
// through as a caller-supplied unverified id.
func TestResolveRejectsUnverifiedImageID(t *testing.T) {
	resolved := imageOnlySpecResolver(twoImageCatalog()).Resolve(imageIDProposal("img-ghost"))

	assert.False(t, resolved.ReadyForConfirmation, "an unverifiable id must not reach the gate")
	assert.NotEmpty(t, resolved.Rejected, "an id absent from the catalog is invalid, not a match")
	assert.Nil(t, resolved.Arguments["CompShareImageId"], "an unverified id must not be sealed")
	assert.Empty(t, resolved.DependencyFailures, "the catalog is available; this is the user's value, not our outage")
}

// TestResolveReportsImageCatalogOutageAsDependencyFailure pins the outage channel:
// no snapshot attached (or an unavailable one) is OUR failure — a DependencyFailure,
// never Rejected (blaming the user's id) and never passing the gate.
func TestResolveReportsImageCatalogOutageAsDependencyFailure(t *testing.T) {
	cases := map[string]*Resolver{
		"never attached": imageOnlySpecResolver(nil),
		"fetch failed":   imageOnlySpecResolver(deployment.NewImageCatalogSnapshot(false, nil)),
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			resolved := resolver.Resolve(imageIDProposal("img-torch"))

			assert.False(t, resolved.ReadyForConfirmation)
			require.Len(t, resolved.DependencyFailures, 1)
			assert.Contains(t, resolved.DependencyFailures[0], "CompShareImageId")
			assert.Empty(t, resolved.Rejected, "a server-side outage must not be blamed on the user's id")
			assert.Nil(t, resolved.Arguments["CompShareImageId"])
		})
	}
}
