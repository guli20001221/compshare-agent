package capability

import (
	"strings"

	"github.com/compshare-agent/internal/platform"
)

// read_catalog_helpers.go holds the generic JSON-shape and display helpers the
// migrated catalog read capabilities (image tag catalog, model repository, GPU
// specs, image list, stock) share. They are byte-identical copies of the legacy
// intent helpers so the relocated renderer bodies compile and render unchanged;
// the ones that have a platform-leaf equivalent forward to it. Under the P3.3
// migration (option B) the legacy intent copies stay as dead code until P6.

func safeValue(v any) string { return platform.SafeValue(v) }

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
