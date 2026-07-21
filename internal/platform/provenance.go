package platform

import (
	"strings"
	"unicode"
)

// ContainsLiteralSpan is the single provenance primitive for proving that a
// model-supplied value is copied from the current user turn. It performs no
// semantic interpretation: case and whitespace are normalized, then a literal
// substring check is made. Architectureguard grants only this named primitive
// an exception from its general ban on new string heuristics.
func ContainsLiteralSpan(userText, span string) bool {
	span = FoldLiteralSpan(span)
	return span != "" && strings.Contains(FoldLiteralSpan(userText), span)
}

// FoldLiteralSpan applies the byte-insensitive normalization shared by literal
// provenance checks. It removes Unicode whitespace and lowercases runes.
func FoldLiteralSpan(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(s))
}
