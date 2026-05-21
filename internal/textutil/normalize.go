// Package textutil holds small string helpers shared across engine,
// router, and other packages that need to do signal matching on
// user-typed messages.
//
// The functions here MUST stay pure (no IO, no logging, no state) and
// strictly behavior-preserving — multiple defense-in-depth layers
// (preblock predicates, planner pre-text snapshot, knowledge retriever
// query normalization) call Normalize and any drift between callers
// silently breaks hard-block precision. See memory rules
// feedback_cjk_security_regex and feedback_l0_stop_grow_dictionary.
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
// This is the canonical normalization used by engine.Chat preblock
// predicates and router.PreBlock rules. Do NOT fork; if a new
// normalization rule is needed (e.g. full-width punctuation), add it
// here so all callers see it uniformly.
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
