package policy

import "strings"

const RedactedQueryValue = "[REDACTED]"

func RedactQueryDerivedValue(value string) string {
	if ContainsQueryLeakage(value) {
		return RedactedQueryValue
	}
	return value
}

func ContainsQueryLeakage(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	lowered := strings.ToLower(trimmed)
	for _, marker := range []string{
		"spt-record",
		"internal_case",
		"workorder",
		"/cloud/",
		"gitlab.",
		"feishu.",
		"lark.",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
