package workflow

import (
	"fmt"
	"unicode/utf8"
)

func validatedCompShareResourceName(value, label string, maxRunes int) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s不能为空", label)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return "", fmt.Errorf("%s不能超过 %d 个字符", label, maxRunes)
	}
	for _, r := range value {
		if !compShareResourceNameRune(r) {
			return "", fmt.Errorf("%s只能包含中文、英文字母、数字以及 _ , . : -", label)
		}
	}
	return value, nil
}

// compShareResourceNameRune is the executable form of upstream StringCheck's
// shared resource-name contract. Keep this one protocol validator rather than
// copying regular expressions into each workflow (which would also create
// several independent semantic decision sites).
func compShareResourceNameRune(r rune) bool {
	if r >= '\u4e00' && r <= '\u9fa5' {
		return true
	}
	if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '_', ',', '.', ':', '-':
		return true
	default:
		return false
	}
}
