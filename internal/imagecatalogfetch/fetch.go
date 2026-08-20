// Package imagecatalogfetch owns the bounded upstream pagination used to build
// image catalogs. It contains no selection or ranking semantics: callers get
// the complete real rows and decide how to render or resolve them.
package imagecatalogfetch

import (
	"context"
	"fmt"
	"reflect"
)

const (
	DefaultPageSize = 100
	MaxPages        = 20
)

type QueryFunc func(context.Context, string, map[string]any) (map[string]any, error)

// FetchAll retrieves every page for listKey using Limit/Offset, bounded by
// MaxPages. Every full page is followed by another probe, even when TotalCount
// equals the number collected so far: some tenant-scoped upstream lists report a
// page-local TotalCount rather than the full result count. If the upstream ignores
// Offset or the bound is reached, it fails instead of presenting a partial catalog
// as whole.
func FetchAll(ctx context.Context, query QueryFunc, action, listKey string, baseArgs map[string]any) (map[string]any, error) {
	var merged map[string]any
	var previous []any
	for page := 0; page < MaxPages; page++ {
		args := cloneArgs(baseArgs)
		args["Limit"] = DefaultPageSize
		args["Offset"] = page * DefaultPageSize
		result, err := query(ctx, action, args)
		if err != nil {
			return nil, err
		}
		if result == nil {
			result = map[string]any{}
		}
		rows, _ := result[listKey].([]any)
		if page > 0 && len(rows) > 0 && reflect.DeepEqual(rows, previous) {
			return nil, fmt.Errorf("%s pagination did not advance at offset %d", action, page*DefaultPageSize)
		}
		if merged == nil {
			merged = cloneMap(result)
			merged[listKey] = []any{}
		}
		merged[listKey] = append(merged[listKey].([]any), rows...)
		if len(rows) < DefaultPageSize {
			return merged, nil
		}
		previous = rows
	}
	return nil, fmt.Errorf("%s image catalog exceeded pagination bound (%d rows)", action, DefaultPageSize*MaxPages)
}

func cloneArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
