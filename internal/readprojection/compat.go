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
	"time"

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

// monitorHistoryLoc is the timezone the historical-monitor renderer formats peak
// timestamps in. Defined locally (the intent window parser keeps its own copy);
// both resolve Asia/Shanghai identically.
var monitorHistoryLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()
