package knowledge

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// NormalizeQuery is the content-free form of a question that the retrieval
// trace and the evidence ledger key on: NFKC-folded, lower-cased, punctuation
// and symbols dropped, whitespace collapsed to single spaces.
func NormalizeQuery(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	var builder strings.Builder
	lastWasSpace := true
	for _, r := range value {
		switch {
		case isASCIIAlnum(r) || isCJK(r):
			builder.WriteRune(r)
			lastWasSpace = false
		case unicode.IsSpace(r):
			if !lastWasSpace {
				builder.WriteByte(' ')
				lastWasSpace = true
			}
		default:
			// Punctuation and symbols are intentionally dropped.
		}
	}
	return strings.TrimSpace(builder.String())
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func isCJK(r rune) bool {
	for _, rng := range cjkRanges {
		if r >= rng.lo && r <= rng.hi {
			return true
		}
	}
	return false
}

var cjkRanges = []struct {
	lo rune
	hi rune
}{
	{0x3400, 0x4DBF},
	{0x4E00, 0x9FFF},
	{0xF900, 0xFAFF},
	{0x20000, 0x2A6DF},
	{0x2A700, 0x2EBEF},
	{0x30000, 0x3134F},
}
