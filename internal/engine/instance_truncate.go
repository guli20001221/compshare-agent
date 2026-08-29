package engine

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/readprojection"
)

// truncateDescribeResultForReAct caps the UHostSet length of a raw
// DescribeCompShareInstance result map to readprojection.DefaultMaxInstancesPerDisplay
// when the LLM did a full-account list (i.e. no specific UHostIds were
// pinned in the call args). Mutates result in place, adding "Shown" and
// "Truncated" fields so the LLM sees the pagination signal.
//
// This protects the model context when a raw account-wide Describe call returns
// more instances than the display budget.
//
// Returns (shown, total, truncated). When no truncation happens the
// function is a no-op except for the return values.
func truncateDescribeResultForReAct(args, result map[string]any) (shown, total int, truncated bool) {
	if result == nil {
		return 0, 0, false
	}
	if hasPinnedUHostIds(args) {
		return 0, 0, false
	}
	rawHosts, ok := result["UHostSet"].([]any)
	if !ok {
		return 0, 0, false
	}
	total = len(rawHosts)
	if total <= readprojection.DefaultMaxInstancesPerDisplay {
		return total, total, false
	}

	type rankedRow struct {
		row  any
		snap entity.InstanceSnapshot
	}
	ranked := make([]rankedRow, 0, total)
	for _, raw := range rawHosts {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ranked = append(ranked, rankedRow{row: raw, snap: entity.InstanceFromMap(row)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return readprojection.InstanceDisplayLess(ranked[i].snap, ranked[j].snap)
	})

	limit := readprojection.DefaultMaxInstancesPerDisplay
	kept := make([]any, 0, limit)
	for i := 0; i < len(ranked) && i < limit; i++ {
		kept = append(kept, ranked[i].row)
	}
	result["UHostSet"] = kept
	result["Shown"] = limit
	result["Truncated"] = true
	return limit, total, true
}

// hasPinnedUHostIds returns true when the caller passed at least one
// UHostId in args — in that case the LLM has a specific target list and
// truncation must not drop anything.
func hasPinnedUHostIds(args map[string]any) bool {
	if args == nil {
		return false
	}
	switch v := args["UHostIds"].(type) {
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	case string:
		return v != ""
	}
	return false
}
