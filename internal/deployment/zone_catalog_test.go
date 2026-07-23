package deployment

import (
	"reflect"
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

// TestZoneCatalogSnapshot_EntryProjectsPlacementAndLabelTogether is the
// single-source-of-truth invariant: Placement and Label are two projections of
// ONE stored row, never two independently stored maps that can drift.
func TestZoneCatalogSnapshot_EntryProjectsPlacementAndLabelTogether(t *testing.T) {
	cat := sampleZoneCatalog()

	entry, ok := cat.Entry("cn-bj2-03")
	require.True(t, ok)
	assert.Equal(t, "华北一C", entry.DisplayName)
	assert.Equal(t, uint32(6003), entry.Placement.ZoneID)

	placement, _ := cat.Placement("cn-bj2-03")
	assert.Equal(t, entry.Placement, placement, "Placement is the entry's placement")
	assert.Equal(t, entry.DisplayName, cat.Label("cn-bj2-03"), "Label is the entry's display name")
}

func TestZoneCatalogSnapshot_LabelFallsBackToZoneID(t *testing.T) {
	cat := sampleZoneCatalog()

	assert.Equal(t, "华北一C", cat.Label("cn-bj2-03"), "a labeled zone shows its console display name")
	// A zone the catalog has no entry for must still label as SOMETHING a form can
	// render — the bare id — never an empty string. But this fallback is display
	// only: Entry, not Label, decides whether the zone is real.
	assert.Equal(t, "cn-unlabeled-01", cat.Label("cn-unlabeled-01"))
	_, ok := cat.Entry("cn-unlabeled-01")
	assert.False(t, ok, "a Label fallback is not proof the zone exists")
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

// TestZoneCatalogSnapshot_SelectedPlacementIsAValueCopy shows the current value
// semantics: mutating a returned placement does not alter the catalog. It does
// NOT prove immutability survives future field changes — that is what the
// structural gate TestZonePlacementIsDeeplyImmutable enforces.
func TestZoneCatalogSnapshot_SelectedPlacementIsAValueCopy(t *testing.T) {
	cat := sampleZoneCatalog()

	selected, ok := cat.Placement("cn-bj2-03")
	require.True(t, ok)
	selected.ZoneID = 9999
	selected.IsPod = false
	assert.Equal(t, uint32(9999), selected.ZoneID)
	assert.False(t, selected.IsPod)

	again, _ := cat.Placement("cn-bj2-03")
	assert.Equal(t, uint32(6003), again.ZoneID, "the caller mutating its placement must not alter the catalog")
	assert.True(t, again.IsPod)
}

// TestZonePlacementIsDeeplyImmutable is acceptance gate #9's real backstop. A
// snapshot severs itself from a selected placement only because ZonePlacement is
// deeply value-typed and a value return fully copies it. If someone adds a slice,
// map, pointer or interface field to ZonePlacement (or a struct that transitively
// contains one), a value return would start SHARING that inner object with the
// frozen catalog — so this test fails the moment such a field appears, forcing an
// explicit Clone before it can pass. Unlike a scalar-mutation test it cannot go
// vacuous: it inspects the type, not a hand-picked field.
func TestZonePlacementIsDeeplyImmutable(t *testing.T) {
	assertDeeplyValueTyped(t, reflect.TypeOf(ZonePlacement{}), "ZonePlacement")
}

func assertDeeplyValueTyped(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		t.Fatalf("%s is a reference type (%s); a value return no longer severs ZonePlacement from the frozen ZoneCatalogSnapshot. Give ZonePlacement an explicit Clone and copy it in the snapshot before adding this field.", path, typ.Kind())
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			assertDeeplyValueTyped(t, f.Type, path+"."+f.Name)
		}
	case reflect.Array:
		assertDeeplyValueTyped(t, typ.Elem(), path+"[]")
	}
}

func TestZoneCatalogSnapshot_UnavailableRefusesAllReads(t *testing.T) {
	// "could not fetch" is enforced structurally, not by convention. Even handed a
	// non-empty slice, an unavailable snapshot exposes nothing — so a consumer that
	// forgets to check Available() cannot silently read stale zones.
	down := NewZoneCatalogSnapshot(false, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "cn-bj2-03", ZoneID: 6003}, DisplayName: "华北一C"},
	})
	assert.False(t, down.Available())
	assert.Nil(t, down.Zones())
	_, ok := down.Placement("cn-bj2-03")
	assert.False(t, ok, "an unavailable catalog must refuse to resolve even a zone it was handed")
	_, ok = down.Entry("cn-bj2-03")
	assert.False(t, ok)

	empty := NewZoneCatalogSnapshot(true, nil)
	assert.True(t, empty.Available(), "an available-but-empty catalog is a real, if empty, answer")
	_, ok = empty.Placement("cn-bj2-03")
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
	_, ok = nilCat.Entry("cn-bj2-03")
	assert.False(t, ok)
}

func TestZoneCatalogSnapshot_NormalizesStoredZone(t *testing.T) {
	// The stored placement's Zone is trimmed to the same canonical form used as
	// the index key, so Zones() (order) and Placement().Zone can never disagree —
	// a downstream request built from the placement carries the clean id.
	cat := NewZoneCatalogSnapshot(true, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "  cn-sh2-02 ", ZoneID: 2002}, DisplayName: "上海二B"},
	})
	assert.Equal(t, []string{"cn-sh2-02"}, cat.Zones())
	got, ok := cat.Placement("cn-sh2-02")
	require.True(t, ok)
	assert.Equal(t, "cn-sh2-02", got.Zone, "the stored placement's Zone is the canonical, trimmed id")
}

func TestZoneCatalogSnapshot_DuplicateWithoutDisplayNameDoesNotInheritOldLabel(t *testing.T) {
	// A repeated zone id replaces the whole row, placement and label together. The
	// bug this guards against is a later record with no display name leaving the
	// earlier label stranded on the new placement — the exact two-sources-drift
	// this type exists to prevent.
	cat := NewZoneCatalogSnapshot(true, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002}, DisplayName: "上海二B"},
		{Placement: ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2999}, DisplayName: ""},
	})

	assert.Equal(t, []string{"cn-sh2-02"}, cat.Zones(), "a duplicate id is not listed twice")
	got, ok := cat.Placement("cn-sh2-02")
	require.True(t, ok)
	assert.Equal(t, uint32(2999), got.ZoneID, "the later entry for a repeated zone id wins")
	assert.Equal(t, "cn-sh2-02", cat.Label("cn-sh2-02"), "an empty new display name does not inherit the old label; it falls back to the id")
}

func TestZoneCatalogSnapshot_DropsBlankZones(t *testing.T) {
	cat := NewZoneCatalogSnapshot(true, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "  ", ZoneID: 1}},
		{Placement: ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002}, DisplayName: "上海二B"},
	})
	assert.Equal(t, []string{"cn-sh2-02"}, cat.Zones(), "a blank zone id is dropped")
}
