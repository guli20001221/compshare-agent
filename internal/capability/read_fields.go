package capability

import (
	"fmt"
	"strings"
)

// Leaf map-field coercions used by the relocated net-accelerator / refund
// renderers. They mirror the generic intent helpers (stringField, boolField,
// numericField) verbatim but are kept local to this package: they are
// primitive string/number extraction, not part of the Resource/Monitor read
// projection's fact model, so they deliberately do NOT go into the shared
// readprojection package (which owns only the unified projection cluster).

func stringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := m[key]
		if !ok || value == nil {
			continue
		}
		if s := strings.TrimSpace(fmt.Sprint(value)); s != "" {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string) (bool, bool) {
	switch v := m[key].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "y", "on":
			return true, true
		case "false", "0", "no", "n", "off":
			return false, true
		}
	case int:
		return v != 0, true
	case int64:
		return v != 0, true
	case float64:
		return v != 0, true
	}
	return false, false
}

func numericField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}
