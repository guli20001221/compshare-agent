// Package readprojection owns the single implementation of the CompShare
// Resource/Monitor deterministic read projection: raw API payload → normalized
// facts → evidence envelope → customer-safe display text.
//
// It depends only on entity, platform, envelope and the standard library.
package readprojection

import (
	"github.com/compshare-agent/internal/platform"
)

// Metric mirrors the shared platform vocabulary within this package.
type Metric = platform.Metric

const (
	MetricCPU    = platform.MetricCPU
	MetricMemory = platform.MetricMemory
	MetricGPU    = platform.MetricGPU
	MetricVRAM   = platform.MetricVRAM
)

// TargetRef mirrors the shared platform vocabulary within this package.
type TargetRef = platform.TargetRef

const TargetRefFilter = platform.TargetRefFilter

// TimeWindow mirrors the shared platform vocabulary within this package.
type TimeWindow = platform.TimeWindow

const (
	TimeWindowPreset   = platform.TimeWindowPreset
	TimeWindowRelative = platform.TimeWindowRelative
	TimeWindowAbsolute = platform.TimeWindowAbsolute
)

// Exported canonical labels and empty-result replies.
const (
	ResourceLabelInstanceID = resourceLabelInstanceID
	ResourceLabelName       = resourceLabelName
	NoMonitorValuesReply    = noMonitorValuesReply
)

// Small local aliases keep projection code focused on rendering.
func safeValue(v any) string { return platform.SafeValue(v) }

func safeValueMap(v map[string]any) map[string]any { return platform.SafeValueMap(v) }

func mapSliceAt(m map[string]any, key string) []any { return platform.MapSliceAt(m, key) }
