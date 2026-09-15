package platform

import "fmt"

// mapSliceAt returns m[key].([]any) if shape matches, nil otherwise.
func MapSliceAt(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	return arr
}

// SafeString returns m[key] as display text, or "" when the map or key is
// absent.
func SafeString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return SafeValue(v)
}

// nestedValue extracts the "Value" field from a nested map response shape like
// `{"Performance": {"Rate": 3, "Value": 83}}`. Returns "" if shape doesn't match.
// Used by gpu_specs_query to pretty-print Performance + GraphicsMemory.
func NestedValue(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	if nested, ok := v.(map[string]any); ok {
		if value, ok := nested["Value"]; ok {
			return fmt.Sprint(value)
		}
	}
	return SafeValue(v)
}

// SafeValue formats any decoded JSON value as display text without panicking
// on nil or on a non-string scalar. The value itself is carried as the
// platform returned it.
func SafeValue(v any) string {
	return fmt.Sprint(v)
}
