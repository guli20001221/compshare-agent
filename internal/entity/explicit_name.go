package entity

import "strings"

// TextExplicitlyMentionsName reports whether text contains name as a complete
// resource name. Adjacent ASCII resource-token characters prevent a match, so
// "pytest" cannot name an instance called "test". Chinese text may touch an
// ASCII name naturally without forcing users to insert spaces.
func TextExplicitlyMentionsName(text, name string) bool {
	text = strings.TrimSpace(text)
	name = strings.TrimSpace(name)
	if text == "" || name == "" {
		return false
	}
	if !asciiOnly(name) {
		return strings.Contains(text, name)
	}
	lowerText := strings.ToLower(text)
	lowerName := strings.ToLower(name)
	for offset := 0; offset <= len(lowerText)-len(lowerName); {
		rel := strings.Index(lowerText[offset:], lowerName)
		if rel < 0 {
			return false
		}
		start := offset + rel
		end := start + len(lowerName)
		leftOK := start == 0 || !asciiResourceTokenByte(lowerText[start-1])
		rightOK := end == len(lowerText) || !asciiResourceTokenByte(lowerText[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func asciiOnly(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiResourceTokenByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_' || value == '-' || value == '.'
}
