package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/actionresolver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImageCatalogFetchedOnlyWhenAProposalNamesAnId guards the cost condition that
// came with giving create a CompShareImageId field.
//
// SpecNeedsImageCatalog is a property of the SCHEMA, so it is true for every
// create now — including the overwhelming majority that name no image at all. The
// catalog's only consumer is CodecImage, and a codec runs only on a slot the
// proposal carries; fetching a fully paginated image catalog for a proposal with
// no id would be upstream cost on every create turn producing an answer nothing
// reads. nil here is "not needed", which is distinct from the unavailable snapshot
// a FAILED fetch returns — the resolver refuses on the latter and must never see
// it just because nobody asked for an image.
func TestImageCatalogFetchedOnlyWhenAProposalNamesAnId(t *testing.T) {
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	create, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	require.True(t, actionresolver.SpecNeedsImageCatalog(create),
		"premise: the schema alone says create needs the catalog")

	eng := &Engine{}

	assert.Nil(t, eng.imageCatalogSnapshotForSpec(context.Background(), create, "community", ""),
		"a create naming no image must not pay for a catalog fetch")
	assert.Nil(t, eng.imageCatalogSnapshotForSpec(context.Background(), create, "community", "   "),
		"…and blank is not an id")

	// With an id there IS something to verify, so the fetch is attempted. This
	// engine has no executor, so it yields the unavailable snapshot rather than
	// nil — and unavailable is what makes the resolver REFUSE the id instead of
	// passing it through, which is the behavior that must not be skipped.
	snap := eng.imageCatalogSnapshotForSpec(context.Background(), create, "community", "compshareImage-abc")
	require.NotNil(t, snap, "an id must be verified, so the catalog must be fetched")
	assert.False(t, snap.Available(),
		"a fetch that could not run reports unavailable, so the id is refused rather than trusted")

	stop, ok := catalog.Lookup("StopInstanceWorkflow")
	require.True(t, ok)
	assert.Nil(t, eng.imageCatalogSnapshotForSpec(context.Background(), stop, "", "compshareImage-abc"),
		"an operation with no image field never needs the catalog, whatever it was handed")
}
