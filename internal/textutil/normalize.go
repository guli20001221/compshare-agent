// Package textutil holds pure string helpers shared across runtime packages.
package textutil

import (
	"strings"
	"unicode"
)

// Normalize standardizes a user message for signal matching: trims
// whitespace, collapses internal whitespace runs to a single space, and
// lowercases ASCII letters. CJK characters are preserved as-is. Returns
// a new string; the input is never mutated.
//
// Callers that compare user-entered labels or commands should share this
// normalization instead of maintaining local variants.
func Normalize(s string) string {
	var b strings.Builder
	prevSpace := true // treat start as space so leading whitespace collapses
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	out := b.String()
	return strings.TrimRight(out, " ")
}
