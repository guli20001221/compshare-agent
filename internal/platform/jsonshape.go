package platform

import (
	"fmt"

	"github.com/compshare-agent/internal/security"
)

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

func SafeString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch typed := v.(type) {
	case string:
		return SafeValue(typed)
	default:
		return SafeValue(typed)
	}
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

func SafeValue(v any) string {
	return fmt.Sprint(security.RedactForLLM(v))
}

// SafeValueMap redacts a whole map for LLM consumption, returning an empty map
// when redaction does not preserve the map shape. It is the map-level companion
// to SafeValue, used by the read-projection monitor path's raw-payload fallback.
func SafeValueMap(v map[string]any) map[string]any {
	if redacted, ok := security.RedactForLLM(v).(map[string]any); ok {
		return redacted
	}
	return map[string]any{}
}

func CopyArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		switch typed := value.(type) {
		case []string:
			out[key] = append([]string(nil), typed...)
		default:
			out[key] = typed
		}
	}
	return out
}
