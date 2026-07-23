package actionresolver

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/deployment"
)

type zoneStatus int

const (
	zoneResolved zoneStatus = iota
	zoneAmbiguous
	zoneUnknown
	zoneCatalogUnavailable
)

type zoneMatch struct {
	Canonical  string
	Candidates []string
	Status     zoneStatus
}

// canonicalZone maps an agent-supplied zone token onto the live catalog. It
// accepts a full zone id or a full console display name and normalizes FORMAT
// ONLY — surrounding whitespace and case. It keeps NO alias table, no city
// keyword list, and never matches a substring.
//
// The distinction is the whole point, exactly as for machine types. "cn-bj2-03"
// in any case, or its console name "华北一C", is the same zone punctuated or
// labelled differently, so it resolves. "华北一区" is NOT "华北一C": whether a
// partial Chinese name means a specific zone is a platform judgment this package
// does not make — it stays unknown and the agent asks, rather than a city-keyword
// table re-acquiring the guesswork this convergence deletes.
func canonicalZone(raw string, catalog *deployment.ZoneCatalogSnapshot) zoneMatch {
	if !catalog.Available() {
		return zoneMatch{Status: zoneCatalogUnavailable}
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return zoneMatch{Status: zoneUnknown}
	}
	zones := catalog.Zones()
	// Exact zone id (case-sensitive) — the unambiguous common case.
	for _, id := range zones {
		if id == value {
			return zoneMatch{Canonical: id, Status: zoneResolved}
		}
	}
	// Case-insensitive zone id OR full console display name.
	var hits []string
	seen := map[string]struct{}{}
	for _, id := range zones {
		if _, ok := seen[id]; ok {
			continue
		}
		if strings.EqualFold(id, value) || strings.EqualFold(catalog.Label(id), value) {
			seen[id] = struct{}{}
			hits = append(hits, id)
		}
	}
	switch len(hits) {
	case 1:
		return zoneMatch{Canonical: hits[0], Status: zoneResolved}
	case 0:
		return zoneMatch{Status: zoneUnknown}
	default:
		return zoneMatch{Candidates: hits, Status: zoneAmbiguous}
	}
}

// canonicalZoneValue adapts canonicalZone to the codec pipeline, mapping each
// status onto the refusal channel that names WHO failed: an unavailable catalog
// is our outage (dependencyError → DependencyFailure), several matches is a
// question for the user (ambiguityError → Conflict), and no match is an invalid
// value (plain error → Rejected). None fall back to a guess.
func (r *Resolver) canonicalZoneValue(text string) (any, error) {
	match := canonicalZone(text, r.zoneCatalog)
	switch match.Status {
	case zoneResolved:
		return match.Canonical, nil
	case zoneCatalogUnavailable:
		return nil, dependencyError{detail: "可用区目录当前不可用，无法确认该可用区"}
	case zoneAmbiguous:
		return nil, ambiguityError{
			detail:     "该名称匹配到多个可用区，请确认具体可用区",
			candidates: match.Candidates,
		}
	default:
		return nil, fmt.Errorf("可用区 %q 不在当前可用区目录中", text)
	}
}

// WithZoneCatalog attaches the engine's live zone snapshot to a resolver so a
// CodecZone field can be canonicalized. Like the machine-type catalog it is pure
// data the engine fetched — the resolver performs no I/O. Chainable so the
// engine can build a resolver in one expression; a resolver left without one
// reports every zone as catalog-unavailable (refuse, never guess), the same as a
// failed fetch.
func (r *Resolver) WithZoneCatalog(catalog *deployment.ZoneCatalogSnapshot) *Resolver {
	r.zoneCatalog = catalog
	return r
}

// SpecNeedsZoneCatalog reports whether resolving this operation requires a live
// zone snapshot. The engine uses it to skip the upstream zone query for
// operations that carry no zone field.
func SpecNeedsZoneCatalog(spec OperationSpec) bool {
	for _, field := range spec.Fields {
		if field.Codec == CodecZone {
			return true
		}
	}
	return false
}
