package actionresolver

import (
	"fmt"

	"github.com/compshare-agent/internal/deployment"
)

// canonicalImageValue adapts an explicit CompShareImageId to the codec pipeline. It
// VERIFIES the id against the live image catalog through the shared resolver, and —
// exactly like CodecZone/CodecMachineType — maps the outcome onto the refusal
// channel that names WHO failed: an unavailable catalog is our outage
// (dependencyError → DependencyFailure), and an id the catalog does not contain is
// an invalid value (plain error → Rejected). None fall back to a guess.
//
// Invariant 1 lives here: only a catalog-verified id survives, so an unverified id
// can never reach a sealed contract via the resolver. Free-text image NAMES are not
// this codec's job — they stay CodecConstrainedText and are resolved (with
// recommend-and-confirm) by the workflow on the SAME snapshot.
func (r *Resolver) canonicalImageValue(text string) (any, error) {
	res := deployment.ResolveImage(r.imageCatalog, deployment.ImageRequest{ID: text})
	switch res.Status {
	case deployment.ResolutionResolved:
		return res.Selection.ID, nil
	case deployment.ResolutionCatalogUnavailable:
		return nil, dependencyError{detail: "镜像目录当前不可用，无法确认该镜像 ID"}
	default:
		// not_found — an explicitly named id absent from the catalog is invalid.
		return nil, fmt.Errorf("镜像 %q 不在当前镜像目录中", text)
	}
}

// WithImageCatalog attaches the engine's live image snapshot to a resolver so a
// CodecImage field can be verified. Like the zone/machine-type catalogs it is pure
// data the engine fetched — the resolver performs no I/O. Chainable; a resolver left
// without one reports every image id as catalog-unavailable (refuse, never guess),
// the same as a failed fetch.
func (r *Resolver) WithImageCatalog(catalog *deployment.ImageCatalogSnapshot) *Resolver {
	r.imageCatalog = catalog
	return r
}

// SpecNeedsImageCatalog reports whether resolving this operation requires a live
// image snapshot. The engine uses it to skip the upstream image query for
// operations that carry no image field.
func SpecNeedsImageCatalog(spec OperationSpec) bool {
	for _, field := range spec.Fields {
		if field.Codec == CodecImage {
			return true
		}
	}
	return false
}
