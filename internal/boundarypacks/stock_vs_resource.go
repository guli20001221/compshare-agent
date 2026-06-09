// Package boundarypacks holds router-time classification boundary packs:
// the cross-intent tie-breaker directives the planner prompt needs to keep two
// adjacent intents from jittering into each other (e.g. platform stock vs the
// user's own instances).
//
// A BoundaryPack is NOT a skill. It carries only prompt directives that shape
// classification at ROUTER time; it has no Body, no resources, no executor, and
// never participates in execution-time methodology. That separation is
// deliberate — execution-time methodology lives in internal/skills. Keeping the
// two in different packages stops a router-time tie-breaker from ever being
// loaded as an executable skill body.
//
// PR5 of the Intent-Router / Dispatch-Contract restructure establishes this
// package with one pilot pack (stock_vs_resource), extracted verbatim from the
// planner base prompt. This is the deterministic PROJECTION mechanism only:
// every pack always projects, in a fixed order. Conditional / keyword-gated
// SELECTION is intentionally out of scope and left to a later PR.
package boundarypacks

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// BoundaryPackID identifies a router-time classification boundary pack.
type BoundaryPackID string

const (
	// BoundaryPackStockVsResource separates platform inventory/availability
	// questions (stock_availability) from the user's own-instance listing
	// questions (resource_info).
	BoundaryPackStockVsResource BoundaryPackID = "stock_vs_resource"
)

// boundaryPackOrder is the single, explicit projection order. A pack projects
// into the planner prompt ONLY if it appears here; ordering never relies on map
// iteration or append happenstance. Any future pack (finance, diagnosis, …) must
// be appended here to participate — TestBoundaryPromptFragments_OrderStable pins
// this list so the order cannot drift silently.
var boundaryPackOrder = []BoundaryPackID{
	BoundaryPackStockVsResource,
}

// BoundaryPack is a router-time classification tie-breaker projected into the
// planner system prompt as one or more directive lines. It is intentionally not
// a skill: no Body, no resources, no executor.
type BoundaryPack struct {
	ID BoundaryPackID
	// Directives are the prompt lines this pack contributes, in order. They are
	// projected verbatim — the text is the contract.
	Directives []string
}

// stockVsResourcePack is the stock-vs-resource tie-breaker. The directive text
// is byte-identical to the line it replaced in the planner base prompt (PR5
// commit 2); only its SOURCE moved (base → pack) and its POSITION in the
// assembled prompt (now projected after the routing fragments).
var stockVsResourcePack = BoundaryPack{
	ID: BoundaryPackStockVsResource,
	Directives: []string{
		"Inventory availability questions like whether a GPU model has stock, is available, is sold out, or has data-center inventory are not resource_info. resource_info is only for the user's own CompShare instances. Platform stock questions should emit stock_availability.",
	},
}

// boundaryPacks is the registry of all defined packs, keyed by ID. Projection
// reads it through boundaryPackOrder, never by ranging the map.
var boundaryPacks = map[BoundaryPackID]BoundaryPack{
	BoundaryPackStockVsResource: stockVsResourcePack,
}

// BoundaryPromptFragments returns the ordered prompt directives contributed by
// every defined boundary pack, in boundaryPackOrder. This is the deterministic
// projection path: every pack always projects (no conditional / keyword
// selection — selection is a later concern). The planner prompt builder appends
// the returned lines as a contiguous block.
func BoundaryPromptFragments() []string {
	out := make([]string, 0, len(boundaryPackOrder))
	for _, id := range boundaryPackOrder {
		pack, ok := boundaryPacks[id]
		if !ok {
			continue
		}
		out = append(out, pack.Directives...)
	}
	return out
}

// PackDirectives returns a copy of one pack's directive lines, or (nil, false)
// if the ID is unknown.
func PackDirectives(id BoundaryPackID) ([]string, bool) {
	pack, ok := boundaryPacks[id]
	if !ok {
		return nil, false
	}
	return append([]string(nil), pack.Directives...), true
}

// PackSHA256 returns the hex SHA-256 of one pack's directives joined by "\n".
// This pins each pack's content INDEPENDENTLY of the full system-prompt hash, so
// a pack edit is caught at the pack layer even if some other prompt change masks
// it at the full-prompt layer.
func PackSHA256(id BoundaryPackID) (string, bool) {
	pack, ok := boundaryPacks[id]
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(strings.Join(pack.Directives, "\n")))
	return hex.EncodeToString(sum[:]), true
}

// Order returns a copy of the projection order, for tests and audits.
func Order() []BoundaryPackID {
	return append([]BoundaryPackID(nil), boundaryPackOrder...)
}
