package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleZoneCatalog() *ZoneCatalogSnapshot {
	return NewZoneCatalogSnapshot(true, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "cn-wlcb-01", Region: "cn-wlcb", ZoneID: 5001, AzGroup: 3001, IsPod: false}, DisplayName: "华北二A"},
		{Placement: ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, DisplayName: "华北一C"},
	})
}

func TestZoneCatalogSnapshot_PlacementMatchesCaseInsensitively(t *testing.T) {
	cat := sampleZoneCatalog()

	// The upstream zone id is lower-case, but an agent or an older param may echo
	// it in any case. Resolution must not hinge on the punctuation of the echo.
	got, ok := cat.Placement("CN-BJ2-03")
	require.True(t, ok)
	assert.Equal(t, ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, got)

	_, ok = cat.Placement("cn-nope-99")
	assert.False(t, ok, "a zone absent from the catalog must miss, not resolve to a zero placement that reads as real")
}

func TestZoneCatalogSnapshot_LabelFallsBackToZoneID(t *testing.T) {
	cat := sampleZoneCatalog()

	assert.Equal(t, "华北一C", cat.Label("cn-bj2-03"), "a labeled zone shows its console display name")
	// A zone the catalog has no name for must still label as SOMETHING a form can
	// render — the bare id — never as an empty string.
	assert.Equal(t, "cn-unlabeled-01", cat.Label("cn-unlabeled-01"))
}

func TestZoneCatalogSnapshot_ZonesPreserveOrderAndAreACopy(t *testing.T) {
	cat := sampleZoneCatalog()

	zones := cat.Zones()
	assert.Equal(t, []string{"cn-wlcb-01", "cn-bj2-03"}, zones, "catalog order is the form's option order")

	// Mutating the returned slice must not reach the snapshot: reference data is
	// read-only to every consumer.
	zones[0] = "TAMPERED"
	assert.Equal(t, []string{"cn-wlcb-01", "cn-bj2-03"}, cat.Zones())
}

// TestZoneCatalogSnapshot_SelectedPlacementDoesNotShareWithCatalog is acceptance
// gate #9: the snapshot and a selected placement must not share a mutable inner
// object. ZonePlacement is all-scalar today so a value return fully severs them;
// this test pins that invariant so a future reference-typed field on
// ZonePlacement cannot silently reintroduce aliasing between the frozen catalog
// and the placement a workflow carries into its sealed draft.
func TestZoneCatalogSnapshot_SelectedPlacementDoesNotShareWithCatalog(t *testing.T) {
	cat := sampleZoneCatalog()

	selected, ok := cat.Placement("cn-bj2-03")
	require.True(t, ok)
	selected.ZoneID = 9999
	selected.IsPod = false
	// The local copy did change — proving the write landed somewhere...
	assert.Equal(t, uint32(9999), selected.ZoneID)
	assert.False(t, selected.IsPod)

	// ...but the catalog it came from did not.
	again, _ := cat.Placement("cn-bj2-03")
	assert.Equal(t, uint32(6003), again.ZoneID, "the caller mutating its placement must not alter the catalog")
	assert.True(t, again.IsPod)
}

func TestZoneCatalogSnapshot_UnavailableIsNotEmpty(t *testing.T) {
	// "could not fetch" is a distinct state from "fetched nothing": a consumer
	// reads Available()==false and refuses, rather than treating an empty catalog
	// as "no such zone exists".
	down := NewZoneCatalogSnapshot(false, nil)
	assert.False(t, down.Available())

	empty := NewZoneCatalogSnapshot(true, nil)
	assert.True(t, empty.Available(), "an available-but-empty catalog is a real, if empty, answer")
	_, ok := empty.Placement("cn-bj2-03")
	assert.False(t, ok)
}

func TestZoneCatalogSnapshot_NilSnapshotIsSafelyUnavailable(t *testing.T) {
	// The workflow accessor returns nil when the context carries no reference
	// data (CLI path, catalog down). Every method must treat nil as "unavailable"
	// without panicking, so "no snapshot" and "fetch failed" collapse to one
	// refusing branch in the consumers.
	var nilCat *ZoneCatalogSnapshot
	assert.False(t, nilCat.Available())
	assert.Nil(t, nilCat.Zones())
	assert.Equal(t, "cn-bj2-03", nilCat.Label("cn-bj2-03"))
	_, ok := nilCat.Placement("cn-bj2-03")
	assert.False(t, ok)
}

func TestZoneCatalogSnapshot_DropsBlankZonesAndDedupesLastWins(t *testing.T) {
	cat := NewZoneCatalogSnapshot(true, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "  ", ZoneID: 1}}, // blank id → dropped
		{Placement: ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002}, DisplayName: "上海二B"},
		{Placement: ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2999}, DisplayName: "上海二B改"}, // same id → last wins
	})

	assert.Equal(t, []string{"cn-sh2-02"}, cat.Zones(), "a blank zone is dropped and a duplicate id is not listed twice")
	got, ok := cat.Placement("cn-sh2-02")
	require.True(t, ok)
	assert.Equal(t, uint32(2999), got.ZoneID, "the later entry for a repeated zone id wins")
	assert.Equal(t, "上海二B改", cat.Label("cn-sh2-02"))
}
