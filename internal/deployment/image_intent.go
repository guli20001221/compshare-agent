package deployment

import (
	"strings"
	"unicode"

	"github.com/compshare-agent/internal/platform"
)

// InferImageCatalogRequest recovers one image preference that is literally
// present in userText and in the live image catalog.
//
// This is deliberately narrower than semantic intent classification. It has no
// framework names, aliases or purpose mappings of its own: candidates come only
// from SoftwareFacts.Framework and Tags on the supplied snapshot, and the user's
// text must contain that catalog value literally (case/whitespace folded). Tags
// remain useful when a catalog/compatibility response omits SoftwareFacts but a
// runtime-named row (for example cuda130_torch291_py312) carries the real
// "pytorch" tag. The result is an ImageRequest, not a selected image; RankImages
// still applies the catalog's version ladder and the user still confirms the
// concrete id.
//
// If the text names more than one unrelated catalog term, the request is
// ambiguous and this function returns false rather than choosing one. A longer
// matched term subsumes a shorter catalog value inside it (for example, "PyTorch"
// wins over a hypothetical separate "Torch" value), preventing a literal
// substring from manufacturing ambiguity.
func InferImageCatalogRequest(snap *ImageCatalogSnapshot, userText, source string) (ImageRequest, bool) {
	if !snap.Available() || strings.TrimSpace(userText) == "" {
		return ImageRequest{}, false
	}

	type catalogMatch struct {
		folded    string
		framework string
		tag       string
	}
	matches := map[string]catalogMatch{}
	addMatch := func(value, kind string) {
		value = strings.TrimSpace(value)
		folded := platform.FoldLiteralSpan(value)
		if !usableCatalogLiteral(folded) || !platform.ContainsLiteralSpan(userText, value) {
			return
		}
		match := matches[folded]
		match.folded = folded
		if kind == "framework" && match.framework == "" {
			match.framework = value
		}
		if kind == "tag" && match.tag == "" {
			match.tag = value
		}
		matches[folded] = match
	}
	for _, entry := range scopeBySource(snap.Entries(), source) {
		if entry.Software.Present {
			addMatch(entry.Software.Framework, "framework")
		}
		for _, tag := range entry.Tags {
			addMatch(tag, "tag")
		}
	}

	// Remove a shorter value wholly contained by another matched catalog value.
	// This is a relation between two live catalog facts, not a user-text keyword
	// heuristic.
	for key, candidate := range matches {
		for otherKey, other := range matches {
			if key == otherKey || len([]rune(other.folded)) <= len([]rune(candidate.folded)) {
				continue
			}
			if platform.ContainsLiteralSpan(other.folded, candidate.folded) {
				delete(matches, key)
				break
			}
		}
	}
	if len(matches) != 1 {
		return ImageRequest{}, false
	}
	for _, match := range matches {
		// Framework and tag can carry the same literal ("PyTorch") on the same
		// catalog. They are two representations of ONE user fact, not two votes.
		// Prefer the structured framework when available; retain the tag only for
		// catalogs/rows that do not expose SoftwareFacts.
		if match.framework != "" {
			match.tag = ""
		}
		return ImageRequest{
			Framework: match.framework,
			Tag:       match.tag,
			Source:    source,
		}, true
	}
	return ImageRequest{}, false
}

func usableCatalogLiteral(folded string) bool {
	if len([]rune(folded)) < 3 {
		return false
	}
	for _, r := range folded {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
