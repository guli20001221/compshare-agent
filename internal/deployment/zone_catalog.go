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
// That is NOT an empty catalog, and a reader must fail rather than fall back to
// a built-in alias table — a stale local copy of a platform fact is exactly what
// this convergence removes.
//
// It is immutable from the outside: every accessor returns a ZonePlacement
// (all-scalar) BY VALUE, or a plain string, or a fresh slice — so a selected
// placement can never share a mutable inner object with the snapshot.
type ZoneCatalogSnapshot struct {
	available  bool
	order      []string                 // canonical zone ids, catalog order
	placements map[string]ZonePlacement // lower(zone id) -> placement
	labels     map[string]string        // lower(zone id) -> 显示名 (Describe)
}

// ZoneCatalogEntry is one catalog row the engine builds from a live
// zones.ZoneInfo: the resolved placement plus the console display name.
type ZoneCatalogEntry struct {
	Placement   ZonePlacement
	DisplayName string
}

// NewZoneCatalogSnapshot freezes the entries into a read-only catalog. Entries
// with a blank zone id are dropped; a later entry for the same zone id wins
// (deterministic — the live list is already deduped upstream). available records
// whether the engine actually obtained the list this turn: pass false for "could
// not fetch", never an empty slice standing in for a fetch failure.
func NewZoneCatalogSnapshot(available bool, entries []ZoneCatalogEntry) *ZoneCatalogSnapshot {
	s := &ZoneCatalogSnapshot{
		available:  available,
		placements: make(map[string]ZonePlacement, len(entries)),
		labels:     make(map[string]string, len(entries)),
	}
	for _, e := range entries {
		zone := strings.TrimSpace(e.Placement.Zone)
		if zone == "" {
			continue
		}
		key := strings.ToLower(zone)
		if _, seen := s.placements[key]; !seen {
			s.order = append(s.order, zone)
		}
		s.placements[key] = e.Placement
		if d := strings.TrimSpace(e.DisplayName); d != "" {
			s.labels[key] = d
		}
	}
	return s
}

// Available reports whether the engine obtained the catalog this turn. A nil
// snapshot (no reference data on the context) is unavailable, so callers can
// treat "no snapshot" and "fetch failed" identically — both must refuse rather
// than guess.
func (s *ZoneCatalogSnapshot) Available() bool { return s != nil && s.available }

// Placement returns the resolved placement for a zone id, matched
// case-insensitively. The second return is false when the zone is not in the
// catalog (or there is no snapshot). The ZonePlacement is returned by value:
// mutating it cannot reach the snapshot.
func (s *ZoneCatalogSnapshot) Placement(zone string) (ZonePlacement, bool) {
	if s == nil {
		return ZonePlacement{}, false
	}
	p, ok := s.placements[strings.ToLower(strings.TrimSpace(zone))]
	return p, ok
}

// Label returns the console display name for a zone, or the zone id itself when
// the catalog carries no name for it (or there is no snapshot) — a caller
// labeling a form option always gets a non-empty string. Display name and
// placement come from the same catalog row, so a label can never disagree with
// the zone it executes as.
func (s *ZoneCatalogSnapshot) Label(zone string) string {
	zone = strings.TrimSpace(zone)
	if s == nil {
		return zone
	}
	if d, ok := s.labels[strings.ToLower(zone)]; ok {
		return d
	}
	return zone
}

// Zones returns the canonical zone ids in catalog order. The returned slice is a
// fresh copy; mutating it cannot reach the snapshot.
func (s *ZoneCatalogSnapshot) Zones() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}
