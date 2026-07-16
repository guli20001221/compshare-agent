// Package readprojection owns the single implementation of the CompShare
// Resource/Monitor deterministic read projection: raw API payload → normalized
// facts → evidence envelope → customer-safe display text.
//
// It depends only on entity, platform, envelope and the standard library — never
// on the intent router, the capability catalog or the engine. The intent package
// keeps thin forwarders so the legacy handlers and their tests compile
// unchanged; new read capabilities call this package directly.
package readprojection

import (
	"github.com/compshare-agent/internal/platform"
)

// Metric mirrors the platform metric vocabulary so the relocated projection code
// references Metric / MetricCPU / … unchanged.
type Metric = platform.Metric

const (
	MetricCPU    = platform.MetricCPU
	MetricMemory = platform.MetricMemory
	MetricGPU    = platform.MetricGPU
	MetricVRAM   = platform.MetricVRAM
)

// TargetRef / TargetRefFilter mirror the platform value-object vocabulary so the
// relocated resource-filter code references TargetRef / TargetRefFilter unchanged.
type TargetRef = platform.TargetRef

const TargetRefFilter = platform.TargetRefFilter

// TimeWindow mirrors the platform monitor-window value object so the relocated
// time-window interpreter references TimeWindow / TimeWindowPreset / … unchanged.
type TimeWindow = platform.TimeWindow

const (
	TimeWindowPreset   = platform.TimeWindowPreset
	TimeWindowRelative = platform.TimeWindowRelative
	TimeWindowAbsolute = platform.TimeWindowAbsolute
)

// Exported views of the projection's canonical labels / empty-result replies so
// the intent compatibility layer (and its tests) resolve to a single source.
const (
	ResourceLabelInstanceID = resourceLabelInstanceID
	ResourceLabelName       = resourceLabelName
	NoMonitorValuesReply    = noMonitorValuesReply
)

// safeValue / safeValueMap / mapSliceAt forward to platform so the relocated
// bodies keep their original call sites verbatim.
func safeValue(v any) string { return platform.SafeValue(v) }

func safeValueMap(v map[string]any) map[string]any { return platform.SafeValueMap(v) }

func mapSliceAt(m map[string]any, key string) []any { return platform.MapSliceAt(m, key) }
