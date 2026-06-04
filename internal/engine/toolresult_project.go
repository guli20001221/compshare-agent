package engine

import (
	"encoding/json"
	"sort"
	"strings"
)

const projectedListLimit = 20

var reactProjectionActions = map[string]struct{}{
	"GetCompShareInstanceMonitor":             {},
	"DescribeAvailableCompShareInstanceTypes": {},
	"DescribeCompShareImages":                 {},
	"DescribeCompShareCustomImages":           {},
	"DescribeCommunityImages":                 {},
	"DescribeCompShareSharingImages":          {},
}

// projectToolResultForReAct shrinks selected bulky read-only tool results in
// place. It returns true only when it actually shrank the model-visible result
// (serialized smaller), and false on every no-op path: nil, action not
// eligible, no projectable fields matched, or the result was already minimal so
// projection produced no reduction. Callers use the bool as an observability
// signal, so it must mean "the result got smaller", not merely "was rewritten".
func projectToolResultForReAct(action string, llmResult map[string]any) bool {
	if llmResult == nil {
		return false
	}
	if _, ok := reactProjectionActions[action]; !ok {
		return false
	}
	projected := make(map[string]any)
	copyAlwaysRetainedFields(projected, llmResult)
	switch action {
	case "GetCompShareInstanceMonitor":
		projectMonitorResult(projected, llmResult)
	case "DescribeAvailableCompShareInstanceTypes":
		projectListKey(projected, llmResult, "AvailableInstanceTypes", availabilityProjectionKeys())
	case "DescribeCompShareImages", "DescribeCompShareCustomImages", "DescribeCompShareSharingImages":
		projectImageLists(projected, llmResult)
	case "DescribeCommunityImages":
		projectCommunityImages(projected, llmResult)
	}
	if len(projected) == 0 {
		return false
	}
	// Only signal (and replace) when projection actually shrinks the
	// model-visible result; otherwise the result was already minimal and a
	// "projected" signal would be a false positive in observability.
	if !jsonSmaller(projected, llmResult) {
		return false
	}
	for key := range llmResult {
		delete(llmResult, key)
	}
	for key, value := range projected {
		llmResult[key] = value
	}
	return true
}

// jsonSmaller reports whether a marshals to fewer bytes than b. If either fails
// to marshal it returns false (conservative: never claim a reduction we cannot
// confirm).
func jsonSmaller(a, b map[string]any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return len(ab) < len(bb)
}

func copyAlwaysRetainedFields(dst, src map[string]any) {
	for _, key := range []string{
		"RetCode", "Action", "Message", "RequestId", "RequestID",
		"Error", "Code", "ErrCode", "ErrMsg",
		"MonitorDataStatus", "MonitorDataGuidance",
		"SshLoginCommand", "DiskSet",
		"DataStatus", "NoData", "NoDataReason", "CannotConfirm", "CannotConfirmReason",
	} {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

func projectMonitorResult(dst, src map[string]any) {
	var rows []any
	collectMonitorRows(src, &rows, projectedListLimit)
	if len(rows) > 0 {
		dst["MonitorSummary"] = rows
	}
}

func collectMonitorRows(value any, rows *[]any, limit int) {
	if len(*rows) >= limit {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		if row := monitorSummaryRow(typed); len(row) > 0 {
			*rows = append(*rows, row)
			if len(*rows) >= limit {
				return
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectMonitorRows(typed[key], rows, limit)
			if len(*rows) >= limit {
				return
			}
		}
	case []any:
		for _, item := range typed {
			collectMonitorRows(item, rows, limit)
			if len(*rows) >= limit {
				return
			}
		}
	}
}

func monitorSummaryRow(m map[string]any) map[string]any {
	row := keepKeys(m, []string{
		"UHostId", "UHostID", "ResourceId", "ResourceID", "Metric", "MetricName", "Key", "Name",
		"Value", "Unit", "Timestamp", "Time",
	})
	if values, ok := m["Values"].([]any); ok && len(values) > 0 {
		row["LatestValue"] = values[len(values)-1]
		row["SampleCount"] = len(values)
	}
	if values, ok := m["Value"].([]any); ok && len(values) > 0 {
		row["LatestValue"] = values[len(values)-1]
		row["SampleCount"] = len(values)
	}
	if len(row) == 0 {
		return nil
	}
	return row
}

func projectImageLists(dst, src map[string]any) {
	for _, key := range []string{"ImageSet", "ImageList", "CustomImageSet", "SharingImageSet", "CompShareImageSet"} {
		projectListKey(dst, src, key, imageProjectionKeys())
	}
}

func projectCommunityImages(dst, src map[string]any) {
	if groups, ok := src["CompshareImageGroup"].([]any); ok {
		projectedGroups := make([]any, 0, minInt(len(groups), projectedListLimit))
		for _, item := range groups {
			group, ok := item.(map[string]any)
			if !ok {
				continue
			}
			out := keepKeys(group, []string{"ImageName", "Name", "Category", "Status"})
			if data, ok := group["Data"].([]any); ok {
				out["Data"] = projectRows(data, imageProjectionKeys(), projectedListLimit)
			}
			if len(out) > 0 {
				projectedGroups = append(projectedGroups, out)
			}
			if len(projectedGroups) >= projectedListLimit {
				break
			}
		}
		dst["CompshareImageGroup"] = projectedGroups
		return
	}
	projectImageLists(dst, src)
}

func projectListKey(dst, src map[string]any, key string, keys []string) {
	rows, ok := src[key].([]any)
	if !ok {
		return
	}
	dst[key] = projectRows(rows, keys, projectedListLimit)
	if len(rows) > projectedListLimit {
		dst[key+"Shown"] = projectedListLimit
		dst[key+"Total"] = len(rows)
		dst[key+"Projected"] = true
	}
}

func projectRows(rows []any, keys []string, limit int) []any {
	out := make([]any, 0, minInt(len(rows), limit))
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		projected := keepKeys(row, keys)
		if len(projected) > 0 {
			out = append(out, projected)
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func keepKeys(row map[string]any, keys []string) map[string]any {
	out := make(map[string]any)
	for _, key := range keys {
		if value, ok := row[key]; ok && keepScalarOrSmallList(value) {
			out[key] = value
		}
	}
	return out
}

func keepScalarOrSmallList(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != "" && len([]rune(typed)) <= 160
	case bool:
		return true
	case float64, float32, int, int64, int32, uint, uint64, uint32:
		return true
	case []any:
		return len(typed) <= 4
	default:
		return false
	}
}

func availabilityProjectionKeys() []string {
	return []string{
		"Zone", "Region", "AvailabilityZone",
		"GPUType", "GpuType", "GPU", "Gpu", "GPUCount", "GpuCount",
		"CPU", "Memory", "InstanceType", "MachineType", "Type", "Name",
		"Status", "StockStatus", "Available", "Price", "ChargeType",
	}
}

func imageProjectionKeys() []string {
	return []string{
		"CompShareImageId", "CompShareImageName", "CompShareImageType",
		"CompShareImageID", "ImageId", "ImageID", "ImageName", "Name",
		"ImageType", "OsType", "OSType", "Platform", "Status", "State",
		"UserEmail", "CreateTime", "UpdateTime", "RepositoryName", "Version",
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
