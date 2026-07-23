package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// popularityLadder is catalog order = popularity order, which is what the community
// browse actually receives: communityImageBrowseArgs asks upstream for
// SortCondition{Field: CreatedCount, ASC: false}, so row 0 is the most-deployed
// image. Both rows support the same GPU and the LESS popular one is newer, so any
// ordering that reads PubTime flips them.
func popularityLadder() *ImageCatalogSnapshot {
	return NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "most-deployed", Name: "InfiniteTalk", Source: "community", Status: ImageStatusAvailable,
			SupportedGPUTypes: []string{"4090"}, PubTime: 100},
		{ID: "barely-deployed", Name: "Something new", Source: "community", Status: ImageStatusAvailable,
			SupportedGPUTypes: []string{"4090"}, PubTime: 999},
	})
}

// TestKnowingTheGpuDoesNotReorderByRecency is the fix for a live symptom: browsing
// community images inside the guided create flow returned the newest image first
// instead of the most-deployed one, even though the catalog arrived sorted by
// deploy count.
//
// The mechanism was that gpuBump counted as "the request discriminated", which
// enabled the PubTime tiebreak. SupportedGpuTypes is a hardware COMPATIBILITY hint,
// not a statement about which image the user wants — and by the time the guided
// picker runs, the GPU is nearly always known, so that flag was set on essentially
// every ordinary create turn and the caller's popularity order was discarded.
//
// The pair is asserted both ways round: with no GPU (the order was always right) and
// with one (where it was not), so a regression cannot hide behind the passing half.
func TestKnowingTheGpuDoesNotReorderByRecency(t *testing.T) {
	snap := popularityLadder()

	withoutGPU := RankImages(snap, ImageRequest{})
	require.Len(t, withoutGPU, 2)
	assert.Equal(t, "most-deployed", withoutGPU[0].ID,
		"premise: with nothing to discriminate on, the catalog's own order stands")

	withGPU := RankImages(snap, ImageRequest{RequestedGPU: "4090"})
	require.Len(t, withGPU, 2)
	assert.Equal(t, "most-deployed", withGPU[0].ID,
		"a GPU compatibility hint is not a preference about the image; it must not re-sort by PubTime")
}

// TestAnActualImagePreferenceStillBreaksTiesByRecency keeps the fix from being a
// blanket removal. When the user DID express a preference about the image, several
// rows can match it equally well and the newest of those is the right one — that is
// the case PubTime was added for, and it must survive.
func TestAnActualImagePreferenceStillBreaksTiesByRecency(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "older-torch", Name: "PyTorch", Source: "platform", Status: ImageStatusAvailable, PubTime: 100},
		{ID: "newer-torch", Name: "PyTorch", Source: "platform", Status: ImageStatusAvailable, PubTime: 999},
	})

	ranked := RankImages(snap, ImageRequest{Name: "PyTorch"})
	require.Len(t, ranked, 2)
	assert.Equal(t, "newer-torch", ranked[0].ID,
		"between rows the request could not tell apart, the newest match is the right default")
}

// TestGpuCompatibilityStillRanksAboveIncompatible confirms the bump itself is
// intact — it was only stopped from enabling the recency tiebreak, not removed.
func TestGpuCompatibilityStillRanksAboveIncompatible(t *testing.T) {
	snap := NewImageCatalogSnapshot(true, []ImageCatalogEntry{
		{ID: "wrong-gpu", Name: "A", Source: "community", Status: ImageStatusAvailable,
			SupportedGPUTypes: []string{"H100"}, PubTime: 999},
		{ID: "right-gpu", Name: "B", Source: "community", Status: ImageStatusAvailable,
			SupportedGPUTypes: []string{"4090"}, PubTime: 1},
	})

	ranked := RankImages(snap, ImageRequest{RequestedGPU: "4090"})
	require.Len(t, ranked, 2)
	assert.Equal(t, "right-gpu", ranked[0].ID, "the compatible image still leads")
}
