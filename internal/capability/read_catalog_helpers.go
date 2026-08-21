package capability

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/platform"
)

// Shared JSON-shape and display helpers for catalog read capabilities.

// imageModelBrowseDisplayCap bounds how many image/model candidates the
// catalog renderers list.
const imageModelBrowseDisplayCap = 10

func safeValue(v any) string { return platform.SafeValue(v) }

func mapSliceAt(m map[string]any, key string) []any { return platform.MapSliceAt(m, key) }

func safeString(m map[string]any, key string) string { return platform.SafeString(m, key) }

func nestedValue(m map[string]any, key string) string { return platform.NestedValue(m, key) }

func matchUserTextToInstanceTypeNames(userText string, items []any, includeFamilyMemoryVariants bool) []string {
	return platform.MatchUserTextToInstanceTypeNames(userText, items, includeFamilyMemoryVariants)
}

func safeNumeric(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		clean := strings.TrimSpace(safeValue(value))
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func entryMatchesSlotQuery(entry map[string]any, query string, fields []string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(safeString(entry, field)), query) {
			return true
		}
	}
	return false
}

func stringSliceAt(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	return stringsFromAny(m[key])
}

func stringSliceMapAt(m map[string]any, key string) map[string][]string {
	out := map[string][]string{}
	if m == nil {
		return out
	}
	switch typed := m[key].(type) {
	case map[string][]string:
		for k, v := range typed {
			out[safeValue(k)] = limitStrings(v, len(v))
		}
	case map[string]any:
		for k, v := range typed {
			out[safeValue(k)] = stringsFromAny(v)
		}
	}
	return out
}

func stringsFromAny(v any) []string {
	switch typed := v.(type) {
	case []string:
		return limitStrings(typed, len(typed))
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s := strings.TrimSpace(safeValue(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func limitStrings(values []string, max int) []string {
	if max <= 0 || len(values) == 0 {
		return nil
	}
	limit := max
	if len(values) < limit {
		limit = len(values)
	}
	out := make([]string, 0, limit)
	for _, value := range values {
		value = strings.TrimSpace(safeValue(value))
		if value == "" {
			continue
		}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}
