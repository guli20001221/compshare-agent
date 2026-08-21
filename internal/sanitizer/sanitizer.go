package sanitizer

import "github.com/compshare-agent/internal/guardrails"

// sensitiveActions maps specific API actions to their known sensitive field names.
var sensitiveActions = map[string][]string{
	"ResetCompShareInstancePassword": {"Password"},
	"ResetPasswordWorkflow":          {"Password"},
}

// Sanitize returns a deep copy of result with sensitive fields replaced.
// The original map is never modified.
func Sanitize(action string, result map[string]any) map[string]any {
	if result == nil {
		return nil
	}
	out := deepCopyMap(result)

	// Generic pattern-based redaction first
	redactByPattern(out)

	// Action-specific redaction with friendly messages (overrides generic)
	if fields, ok := sensitiveActions[action]; ok {
		for _, field := range fields {
			redactNested(out, field, friendlyRedaction(field))
		}
	}

	return out
}

// SanitizeArgs returns a deep copy safe for event callbacks.
func SanitizeArgs(action string, args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := deepCopyMap(args)

	if fields, ok := sensitiveActions[action]; ok {
		for _, field := range fields {
			if _, exists := out[field]; exists {
				out[field] = "[REDACTED]"
			}
		}
	}
	redactByPattern(out)
	return out
}

// friendlyRedaction returns a user-friendly placeholder for known sensitive fields.
func friendlyRedaction(field string) string {
	switch {
	case field == "Password":
		return "[已设置]"
	default:
		return "[REDACTED]"
	}
}

// redactNested replaces a field value at any nesting depth.
func redactNested(m map[string]any, field, replacement string) {
	for k, v := range m {
		if k == field {
			m[k] = replacement
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			redactNested(val, field, replacement)
		case []any:
			for _, item := range val {
				if sub, ok := item.(map[string]any); ok {
					redactNested(sub, field, replacement)
				}
			}
		}
	}
}

// redactByPattern scans all keys and replaces values matching generic patterns.
func redactByPattern(m map[string]any) {
	for k, v := range m {
		if matchesSensitivePattern(k) {
			if _, isStr := v.(string); isStr {
				m[k] = "[REDACTED]"
			}
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			redactByPattern(val)
		case []any:
			for _, item := range val {
				if sub, ok := item.(map[string]any); ok {
					redactByPattern(sub)
				}
			}
		}
	}
}

func matchesSensitivePattern(key string) bool {
	return guardrails.IsCredentialKey(key)
}

// deepCopyMap returns a deep copy of a map[string]any.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			out[k] = deepCopyMap(val)
		case []any:
			out[k] = deepCopySlice(val)
		default:
			out[k] = v
		}
	}
	return out
}

func deepCopySlice(s []any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]any:
			out[i] = deepCopyMap(val)
		case []any:
			out[i] = deepCopySlice(val)
		default:
			out[i] = v
		}
	}
	return out
}
