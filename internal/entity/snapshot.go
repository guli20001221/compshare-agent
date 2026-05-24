package entity

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// InstanceSnapshot is the compact, API-grounded representation used by later
// planner/handler code to resolve user references without trusting LLM IDs.
type InstanceSnapshot struct {
	UHostId    string
	Name       string
	State      string
	OsType     string
	GPU        int
	GpuType    string
	ImageType  string
	StartTime  int64
	CPU        int
	Memory     int
	Zone       string
	Region     string
	ChargeType string
	ExpireTime int64
	AutoRenew  string
}

// InstanceFromMap parses a single DescribeCompShareInstance UHostSet[i]
// row into a typed InstanceSnapshot. Exported for use by engine M2
// ToolFact writer (internal/engine/session_state.go), which extracts
// instance_state facts from raw tool results without going through the
// EntityRegistry's full Sync (LLM-driven calls only mark the registry
// invalidated; the registry is not necessarily fresh by the time the
// fact is recorded).
func InstanceFromMap(row map[string]any) InstanceSnapshot {
	return instanceFromMap(row)
}

func instanceFromMap(row map[string]any) InstanceSnapshot {
	return InstanceSnapshot{
		UHostId:    stringField(row, "UHostId"),
		Name:       stringField(row, "Name"),
		State:      stringField(row, "State"),
		OsType:     stringField(row, "OsType"),
		GPU:        intField(row, "GPU"),
		GpuType:    stringField(row, "GpuType"),
		ImageType:  stringField(row, "ImageType"),
		StartTime:  int64Field(row, "StartTime"),
		CPU:        intField(row, "CPU"),
		Memory:     intField(row, "Memory"),
		Zone:       stringField(row, "Zone"),
		Region:     stringField(row, "Region"),
		ChargeType: stringField(row, "ChargeType"),
		ExpireTime: int64Field(row, "ExpireTime"),
		AutoRenew:  stringField(row, "AutoRenew"),
	}
}

func stringField(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	if v, ok := row[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func intField(row map[string]any, key string) int {
	return int(int64Field(row, key))
}

func int64Field(row map[string]any, key string) int64 {
	switch v := row[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(v.String(), 64); err == nil {
			return int64(f)
		}
		return 0
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}
