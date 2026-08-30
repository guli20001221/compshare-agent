package capability

import (
	"context"
	"sort"
)

const (
	describeInstancePageLimit = 100
	maxDescribeInstancePages  = 100
)

// describeAllAccountInstances follows DescribeCompShareInstance's shared
// Offset/Limit contract until every reported instance has been observed. The
// upstream dispatcher applies the same page to its UHost and Pod stores, so a
// page may contain up to twice the requested Limit; TotalCount is the combined
// account total.
//
// The hard page bound is only a termination guard. If it is reached, the
// aggregated response keeps the upstream TotalCount, and the existing registry
// parser marks the snapshot truncated instead of treating a partial view as an
// absence oracle.
func describeAllAccountInstances(ctx context.Context, exec ReadExecutor) (map[string]any, error) {
	var merged map[string]any
	rows := make([]any, 0)
	seen := make(map[string]struct{})
	totalCount := 0
	totalKnown := false

	for page := 0; page < maxDescribeInstancePages; page++ {
		raw, err := exec.Execute(ctx, resourceInfoAction, map[string]any{
			"Limit":  describeInstancePageLimit,
			"Offset": page * describeInstancePageLimit,
		})
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = make(map[string]any, len(raw))
			for key, value := range raw {
				merged[key] = value
			}
		}
		if reported, ok := numericField(raw, "TotalCount"); ok {
			totalKnown = true
			if int(reported) > totalCount {
				totalCount = int(reported)
			}
		}

		pageRows := mapSliceAt(raw, "UHostSet")
		added := 0
		for _, item := range pageRows {
			row, _ := item.(map[string]any)
			id := stringField(row, "UHostId")
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			rows = append(rows, item)
			added++
		}

		if totalKnown && len(seen) >= totalCount {
			break
		}
		if !totalKnown && len(pageRows) < describeInstancePageLimit {
			break
		}
		if len(pageRows) == 0 || added == 0 {
			break
		}
	}

	if merged == nil {
		merged = map[string]any{}
	}
	if len(seen) > totalCount {
		totalCount = len(seen)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, _ := rows[i].(map[string]any)
		right, _ := rows[j].(map[string]any)
		leftCreated, _ := numericField(left, "CreateTime")
		rightCreated, _ := numericField(right, "CreateTime")
		return leftCreated > rightCreated
	})
	merged["UHostSet"] = rows
	merged["TotalCount"] = float64(totalCount)
	return merged, nil
}
