package entity

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// IsSpot marks a 抢占式 instance. It is a SEPARATE upstream field, not a
	// ChargeType value: a spot instance describes as ChargeType "Postpay" (or, when
	// billed under CHARGE_BY_SPOT, an empty string) PLUS IsSpot=true, and upstream
	// never emits ChargeType "Spot". Carrying only ChargeType made 按量 and 抢占式
	// indistinguishable in every model-visible projection, so on 2026-08-17 the
	// assistant read charge_type=Postpay off a spot instance and told its owner
	// 「所以它不是抢占式实例，平台不会按抢占式规则主动回收」 — while the billing card
	// rendered in the same turn, from the same describe row, said 抢占式/时.
	IsSpot     bool
	ExpireTime int64
	AutoRenew  string
}

// InstanceIsSpot reads the spot flag off a raw DescribeCompShareInstance row.
// It is the single decision point for "is this instance 抢占式": the instance
// projection and the billing chain both call it, so the two can no longer answer
// the same question differently about the same row.
func InstanceIsSpot(row map[string]any) bool {
	return boolField(row, "IsSpot")
}

// InstanceFromMap parses a single DescribeCompShareInstance UHostSet[i]
// row into a typed InstanceSnapshot for selection and workflow validation.
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
		IsSpot:     InstanceIsSpot(row),
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

// boolField is deliberately tolerant of the wire spellings this API mixes: the
// sibling field AutoRenew arrives as "Yes"/"No", so a bool-valued field arriving
// as a string is a shape this upstream already produces. Anything it does not
// recognise is false, which matches the absent-key case.
func boolField(row map[string]any, key string) bool {
	switch v := row[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "1":
			return true
		}
		return false
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case json.Number:
		n, err := v.Float64()
		return err == nil && n != 0
	default:
		return false
	}
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
