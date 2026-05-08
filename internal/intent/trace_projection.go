package intent

import (
	"time"

	"github.com/compshare-agent/internal/observability"
)

type PlannerTraceOptions struct {
	Enabled bool
	Model   string
	Latency time.Duration
}

type PlannerTraceTargetRef struct {
	Type           string `json:"type"`
	Source         string `json:"source,omitempty"`
	ValueHash      string `json:"value_hash,omitempty"`
	SourceSpanHash string `json:"source_span_hash,omitempty"`
}

type PlannerTraceTimeWindow struct {
	Type      string `json:"type"`
	Value     string `json:"value,omitempty"`
	ValueHash string `json:"value_hash,omitempty"`
}

func ProjectPlannerTrace(result PlannerResult, opts PlannerTraceOptions) observability.PlannerTrace {
	trace := observability.PlannerTrace{
		Slots: observability.PlannerSlots{
			TargetRefs: []any{},
			Metrics:    []string{},
		},
	}
	if !opts.Enabled {
		return trace
	}

	trace.Enabled = true
	trace.Model = opts.Model
	trace.LatencyMS = opts.Latency.Milliseconds()
	trace.SchemaValid = !result.Fallback && result.Plan.SchemaVersion == SchemaVersion && result.Plan.Intent != ""
	trace.Intent = string(result.Plan.Intent)
	trace.Slots = projectPlannerSlots(result.Plan.Slots)
	trace.Confidence = result.Plan.Confidence
	trace.HardBlockHint = result.Plan.HardBlockHint
	if !trace.SchemaValid {
		trace.Intent = string(IntentUnknown)
		trace.Confidence = 0
		if !result.Fallback {
			trace.HardBlockHint = false
		}
	}
	return trace
}

func projectPlannerSlots(slots Slots) observability.PlannerSlots {
	out := observability.PlannerSlots{
		TargetRefs: make([]any, 0, len(slots.TargetRefs)),
		Metrics:    make([]string, 0, len(slots.Metrics)),
	}
	for _, ref := range slots.TargetRefs {
		projected := PlannerTraceTargetRef{
			Type:      string(ref.Type),
			Source:    string(ref.Source),
			ValueHash: hashPlannerTraceValue(ref.Value),
		}
		if ref.SourceSpan != "" {
			projected.SourceSpanHash = hashPlannerTraceValue(ref.SourceSpan)
		}
		out.TargetRefs = append(out.TargetRefs, projected)
	}
	for _, metric := range slots.Metrics {
		out.Metrics = append(out.Metrics, string(metric))
	}
	if slots.TimeWindow != nil {
		out.TimeWindow = projectPlannerTimeWindow(*slots.TimeWindow)
	}
	return out
}

func projectPlannerTimeWindow(window TimeWindow) PlannerTraceTimeWindow {
	out := PlannerTraceTimeWindow{Type: string(window.Type)}
	if isPlannerTraceCanonicalTimeWindow(window.Value) {
		out.Value = window.Value
		return out
	}
	out.ValueHash = hashPlannerTraceValue(window.Value)
	return out
}

func isPlannerTraceCanonicalTimeWindow(value string) bool {
	switch value {
	case "now", "today", "yesterday", "last_1h", "last_24h", "last_7d":
		return true
	default:
		return false
	}
}

func hashPlannerTraceValue(value string) string {
	if value == "" {
		return ""
	}
	hash, err := observability.HashTracePayload(value)
	if err != nil {
		return ""
	}
	return hash
}
