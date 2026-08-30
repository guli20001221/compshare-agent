package entity

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// InstanceSnapshot is the compact, API-grounded representation used by later
// planner/handler code to resolve user references without trusting LLM IDs.
type InstanceSnapshot struct {
	UHostId           string
	Name              string
	State             string
	OsType            string
	GPU               int
	GpuType           string
	ImageName         string
	ImageType         string
	InstanceType      string
	StartTime         int64
	SchedulerStopTime int64
	StopTime          int64
	ReleaseTime       int64
	CPU               int
	Memory            int
	Zone              string
	Region            string
	ChargeType        string
	// IsSpot is independent of ChargeType: spot instances describe as Postpay (or
	// an empty ChargeType) plus IsSpot=true.
	IsSpot     bool
	ExpireTime int64
	AutoRenew  string
}

// InstanceIsSpot is the shared reader used by the instance projection and
// billing diagnosis. DescribeCompShareInstance defines IsSpot as a JSON boolean.
func InstanceIsSpot(row map[string]any) bool {
	v, _ := row["IsSpot"].(bool)
	return v
}

// InstanceFromMap parses a single DescribeCompShareInstance UHostSet[i]
// row into a typed InstanceSnapshot for selection and workflow validation.
func InstanceFromMap(row map[string]any) InstanceSnapshot {
	return instanceFromMap(row)
}

func instanceFromMap(row map[string]any) InstanceSnapshot {
	return InstanceSnapshot{
		UHostId:           stringField(row, "UHostId"),
		Name:              stringField(row, "Name"),
		State:             stringField(row, "State"),
		OsType:            stringField(row, "OsType"),
		GPU:               intField(row, "GPU"),
		GpuType:           stringField(row, "GpuType"),
		ImageName:         stringField(row, "CompShareImageName"),
		ImageType:         stringField(row, "CompShareImageType"),
		InstanceType:      stringField(row, "InstanceType"),
		StartTime:         int64Field(row, "StartTime"),
		SchedulerStopTime: int64Field(row, "SchedulerStopTime"),
		StopTime:          int64Field(row, "StopTime"),
		ReleaseTime:       int64Field(row, "ReleaseTime"),
		CPU:               intField(row, "CPU"),
		Memory:            intField(row, "Memory"),
		Zone:              stringField(row, "Zone"),
		Region:            stringField(row, "Region"),
		ChargeType:        stringField(row, "ChargeType"),
		IsSpot:            InstanceIsSpot(row),
		ExpireTime:        int64Field(row, "ExpireTime"),
		AutoRenew:         stringField(row, "AutoRenew"),
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
