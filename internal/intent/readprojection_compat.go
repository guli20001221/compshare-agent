package intent

import (
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/readprojection"
)

// The Resource/Monitor deterministic read projection now has a single
// implementation in internal/readprojection. These forwarders keep the legacy
// handlers (HandleResourceInfo / HandleMonitorQuery) and their tests compiling;
// new read capabilities call readprojection directly. P6 deletes these wrappers
// together with the legacy handlers — it does not move implementation again.

type ResourceEnvelopeMeta = readprojection.ResourceEnvelopeMeta

type MonitorScalar = readprojection.MonitorScalar

// Canonical label / empty-result strings the legacy handler tests assert on,
// re-exported from the single source in readprojection.
const (
	resourceLabelInstanceID = readprojection.ResourceLabelInstanceID
	resourceLabelName       = readprojection.ResourceLabelName
	noMonitorValuesReply    = readprojection.NoMonitorValuesReply
)

func RenderResourceSummary(instances []entity.InstanceSnapshot, meta ResourceEnvelopeMeta) string {
	return readprojection.RenderResourceSummary(instances, meta)
}

func RenderMonitorSummary(metrics []Metric, payload map[string]any) string {
	return readprojection.RenderMonitorSummary(metrics, payload)
}

func RenderHistoricalMonitorSummary(metrics []Metric, payload map[string]any) string {
	return readprojection.RenderHistoricalMonitorSummary(metrics, payload)
}

func BuildResourceEnvelope(instances []entity.InstanceSnapshot) envelope.Envelope {
	return readprojection.BuildResourceEnvelope(instances)
}

func BuildResourceEnvelopeWithMeta(instances []entity.InstanceSnapshot, meta ResourceEnvelopeMeta) envelope.Envelope {
	return readprojection.BuildResourceEnvelopeWithMeta(instances, meta)
}

func BuildMonitorEnvelope(subjects []entity.InstanceSnapshot, metrics []Metric, payload map[string]any) envelope.Envelope {
	return readprojection.BuildMonitorEnvelope(subjects, metrics, payload)
}

func ExtractMonitorScalars(payload map[string]any, metrics []Metric) []MonitorScalar {
	return readprojection.ExtractMonitorScalars(payload, metrics)
}
