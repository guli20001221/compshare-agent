package entity

import (
	"strings"
	"unicode"
)

// TextExplicitlyMentionsName reports whether text contains name as a complete
// resource name. Names written entirely in a non-ASCII script require Unicode
// boundaries, so "机器" cannot silently name "机". Names containing an ASCII
// identifier segment use ASCII token boundaries, preserving natural Chinese
// grammar while still preventing "pytest" from naming "test".
func TextExplicitlyMentionsName(text, name string) bool {
	text = strings.TrimSpace(text)
	name = strings.TrimSpace(name)
	if text == "" || name == "" {
		return false
	}
	textRunes := []rune(strings.ToLower(text))
	nameRunes := []rune(strings.ToLower(name))
	strictUnicodeBoundary := !containsASCIINameRune(nameRunes)
	for start := 0; start+len(nameRunes) <= len(textRunes); start++ {
		if !equalRunes(textRunes[start:start+len(nameRunes)], nameRunes) {
			continue
		}
		end := start + len(nameRunes)
		leftOK := start == 0 || !boundaryRune(textRunes[start-1], strictUnicodeBoundary)
		rightOK := end == len(textRunes) || !boundaryRune(textRunes[end], strictUnicodeBoundary)
		if leftOK && rightOK {
			return true
		}
	}
	return false
}

func equalRunes(left, right []rune) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func containsASCIINameRune(value []rune) bool {
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}

func boundaryRune(value rune, strictUnicode bool) bool {
	if strictUnicode {
		return unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_' || value == '-' || value == '.'
	}
	return value <= unicode.MaxASCII && (unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_' || value == '-' || value == '.')
}
