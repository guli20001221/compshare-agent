package deployment

import "strings"

// ZoneCatalogSnapshot is the live availability-zone catalog, snapshotted by the
// engine once per turn and handed to BOTH the action resolver and the workflow
// as pure, read-only reference data.
//
// Like the resolver's machine-type catalog it is data the readers may consult
// but never fetch: an adjudicator that owns the network call stops being
// replayable and becomes a second workflow. And it is REFERENCE data, not
// business params — the platform's zone list is not something the user
// confirmed, so it never enters the sealed contract. Only the ONE ZonePlacement
// the user actually selected does.
//
// Available()==false means the engine could not obtain the catalog this turn.
// That is NOT an empty catalog, and it is enforced structurally, not by
// convention: an unavailable snapshot holds no entries AND refuses every read,
// so a consumer that forgets to check Available() still cannot silently fall
// back to stale data. A reader must fail rather than degrade to a built-in alias
// table — a stale local copy of a platform fact is exactly what this convergence
// removes.
//
// It keeps ONE row per zone (placement + display name together), so the label a
// form shows can never drift from the zone it executes as — the two-parallel-maps
// shape this whole change deletes cannot be reintroduced inside the type meant to
// end it. It is immutable from the outside: accessors return a ZonePlacement
// (all-scalar) BY VALUE, or a plain string, or a fresh slice.
type ZoneCatalogSnapshot struct {
	available bool
	order     []string                    // canonical zone ids, catalog order
	entries   map[string]ZoneCatalogEntry // lower(zone id) -> the one row
}

// ZoneCatalogEntry is one catalog row the engine builds from a live
// zones.ZoneInfo: the resolved placement plus the console display name. Placement
// and DisplayName are ONE unit — a snapshot stores and replaces them together so
// they cannot diverge.
type ZoneCatalogEntry struct {
	Placement   ZonePlacement
	DisplayName string
}

// NewZoneCatalogSnapshot freezes the entries into a read-only catalog.
//
// When available is false the entries are discarded entirely: a fetch failure
// cannot masquerade as data even if a caller passes a stale slice. When
// available is true, entries with a blank zone id are dropped, each stored zone
// id is trimmed to its canonical form (so Zones() and Placement().Zone always
// agree), and a later entry for the same zone id replaces the earlier one WHOLE
// — placement and display name move together, so a repeat with no display name
// does not inherit the previous label.
func NewZoneCatalogSnapshot(available bool, entries []ZoneCatalogEntry) *ZoneCatalogSnapshot {
	s := &ZoneCatalogSnapshot{available: available, entries: map[string]ZoneCatalogEntry{}}
	if !available {
		return s
	}
	for _, e := range entries {
		zone := strings.TrimSpace(e.Placement.Zone)
		if zone == "" {
			continue
		}
		e.Placement.Zone = zone // canonical form is what the row stores AND returns
		e.DisplayName = strings.TrimSpace(e.DisplayName)
		key := strings.ToLower(zone)
		if _, seen := s.entries[key]; !seen {
			s.order = append(s.order, zone)
		}
		s.entries[key] = e
	}
	return s
}

// Available reports whether the engine obtained the catalog this turn. A nil
// snapshot (no reference data on the context) is unavailable, so callers can
// treat "no snapshot" and "fetch failed" identically — both must refuse rather
// than guess.
func (s *ZoneCatalogSnapshot) Available() bool { return s != nil && s.available }

// Entry returns the single catalog row for a zone id, matched case-insensitively.
// It is the sole read choke point: an unavailable snapshot returns nothing here,
// so no accessor built on it can leak stale data. This is also the gate a form
// generator must pass before offering a zone as an executable option — a Label
// that fell back to the bare id is not proof the zone exists.
func (s *ZoneCatalogSnapshot) Entry(zone string) (ZoneCatalogEntry, bool) {
	if !s.Available() {
		return ZoneCatalogEntry{}, false
	}
	e, ok := s.entries[strings.ToLower(strings.TrimSpace(zone))]
	return e, ok
}

// Placement returns the resolved placement for a zone id. The second return is
// false when the zone is not in the catalog (or the catalog is unavailable). The
// ZonePlacement is returned by value: mutating it cannot reach the snapshot.
func (s *ZoneCatalogSnapshot) Placement(zone string) (ZonePlacement, bool) {
	e, ok := s.Entry(zone)
	return e.Placement, ok
}

// Label returns the console display name for a zone, or the zone id itself when
// the catalog carries no name for it (or is unavailable) — a caller labeling a
// form option always gets a non-empty string. It projects from the same row as
// Placement, so a label can never disagree with the zone it executes as. The
// fallback is for DISPLAY only; use Entry to decide whether a zone is real.
func (s *ZoneCatalogSnapshot) Label(zone string) string {
	zone = strings.TrimSpace(zone)
	if e, ok := s.Entry(zone); ok && e.DisplayName != "" {
		return e.DisplayName
	}
	return zone
}

// Zones returns the canonical zone ids in catalog order. The returned slice is a
// fresh copy; mutating it cannot reach the snapshot. An unavailable catalog
// returns nil.
func (s *ZoneCatalogSnapshot) Zones() []string {
	if !s.Available() {
		return nil
	}
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}
