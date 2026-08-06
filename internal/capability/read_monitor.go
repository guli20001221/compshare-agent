package capability

import (
	"context"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

// Monitor read capabilities (migrated from the legacy intent route). Current
// monitoring runs here; historical monitoring is added in a later slice once the
// time-window interpretation is relocated. Both share the target contract, the
// upstream call and the deterministic render/envelope.

const (
	// monitorCurrentCapabilityLabel is both the current-monitor tool label and
	// the failure-reply label for BOTH current and historical monitoring — the
	// legacy handler hardcoded "monitor_query" as the failure label regardless of
	// which monitor intent ran.
	monitorCurrentCapabilityLabel = string(intent.IntentMonitorQuery)
	// monitorHistoryCapabilityLabel is the historical-monitor tool + observation
	// label; the failure reply still uses monitorCurrentCapabilityLabel.
	monitorHistoryCapabilityLabel = string(intent.IntentMonitorHistory)
	monitorAction                 = "GetCompShareInstanceMonitor"
)

// MonitorCurrentRequest is the current-monitor request contract. It carries no
// time window (current is "now"), so unlike the legacy path there is no
// time-window validation to run.
type MonitorCurrentRequest struct {
	Targets []platform.TargetRef `json:"targets,omitempty"`
	Metrics []platform.Metric    `json:"metrics,omitempty"`
}

// MissingFields: none — an empty target set falls back to the selected instance.
func (MonitorCurrentRequest) MissingFields() []platform.MissingField { return nil }

// MonitorResponse is the shared successful monitor result: the resolved subject
// instances, the requested metrics and the raw upstream payload the renderer /
// envelope builder consume. Historical distinguishes the two render paths.
type MonitorResponse struct {
	Instances   []entity.InstanceSnapshot
	Metrics     []platform.Metric
	Raw         map[string]any
	Historical  bool
	WindowStart int64
	WindowEnd   int64
}

func monitorCurrentReadSpec() ReadCapabilitySpec[MonitorCurrentRequest, MonitorResponse] {
	return ReadCapabilitySpec[MonitorCurrentRequest, MonitorResponse]{
		Label:       monitorCurrentCapabilityLabel,
		Description: "查询已有实例当前 CPU、内存、GPU 或显存监控数据。用于实时状态；指定历史时间范围时使用历史监控能力。",
		Params:      objectParam(map[string]schemaNode{"targets": targetRefsParam(), "metrics": metricsParam()}),
		Handle:      monitorCurrentHandle,
		Render:      monitorRender,
	}
}

func monitorCurrentHandle(ctx context.Context, req MonitorCurrentRequest, rt ReadRuntime) (MonitorResponse, ReadResult) {
	instances, ids, terminal := resolveMonitorTargets(ctx, req.Targets, rt)
	if terminal.Status != "" {
		return MonitorResponse{}, terminal
	}
	raw, err := rt.Executor.Execute(ctx, monitorAction, map[string]any{"UHostIds": ids})
	if err != nil {
		return MonitorResponse{}, ReadFailureAfterTool(monitorAction, monitorCurrentCapabilityLabel, err)
	}
	return MonitorResponse{Instances: instances, Metrics: req.Metrics, Raw: raw}, ReadResult{}
}

// MonitorHistoryRequest is the historical-monitor request contract. A time
// window is required (schema required + MissingFields), so an empty one is
// needs_input before the handler.
type MonitorHistoryRequest struct {
	Targets    []platform.TargetRef `json:"targets,omitempty"`
	Metrics    []platform.Metric    `json:"metrics,omitempty"`
	TimeWindow *platform.TimeWindow `json:"time_window,omitempty"`
}

func (r MonitorHistoryRequest) MissingFields() []platform.MissingField {
	if r.TimeWindow == nil {
		return []platform.MissingField{platform.Missing("time_window")}
	}
	return nil
}

func monitorHistoryReadSpec() ReadCapabilitySpec[MonitorHistoryRequest, MonitorResponse] {
	return ReadCapabilitySpec[MonitorHistoryRequest, MonitorResponse]{
		Label:       monitorHistoryCapabilityLabel,
		Description: "查询单个已有实例在明确起止时间内的 CPU、内存、GPU 或显存历史监控。时间范围最多 24 小时；当前值查询使用实时监控能力。",
		Params: objectParam(map[string]schemaNode{
			"targets":     targetRefsParam(),
			"metrics":     metricsParam(),
			"time_window": timeWindowParam(),
		}, "time_window"),
		Handle: monitorHistoryHandle,
		Render: monitorRender,
	}
}

func monitorHistoryHandle(ctx context.Context, req MonitorHistoryRequest, rt ReadRuntime) (MonitorResponse, ReadResult) {
	instances, ids, terminal := resolveMonitorTargets(ctx, req.Targets, rt)
	if terminal.Status != "" {
		return MonitorResponse{}, terminal
	}
	// Historical monitoring is single-instance (the upstream API returns one
	// series set per call, and the renderer aggregates one host).
	if len(ids) != 1 {
		return MonitorResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackValidation)
	}
	start, end, ok := readprojection.ResolveMonitorHistoryWindow(req.TimeWindow)
	if !ok {
		return MonitorResponse{}, ReadFallbackBeforeTool(platform.ReadFallbackTimeWindow)
	}
	args := map[string]any{"UHostIds": ids, "StartTime": start, "EndTime": end}
	raw, err := rt.Executor.Execute(ctx, monitorAction, args)
	if err != nil {
		return MonitorResponse{}, ReadFailureAfterTool(monitorAction, monitorCurrentCapabilityLabel, err)
	}
	return MonitorResponse{Instances: instances, Metrics: req.Metrics, Raw: raw, Historical: true, WindowStart: start, WindowEnd: end}, ReadResult{}
}

// resolveMonitorTargets applies the instance-scoped monitor target contract: an
// empty target set falls back to the session's selected instance, otherwise it
// is a missing-target fallback; a non-empty set is resolved through the shared
// typed resolver. Returns a terminal ReadResult (non-empty Status) on fallback.
func resolveMonitorTargets(ctx context.Context, targets []platform.TargetRef, rt ReadRuntime) ([]entity.InstanceSnapshot, []string, ReadResult) {
	if len(targets) == 0 {
		if rt.FallbackInstanceID != "" {
			targets = []platform.TargetRef{{
				Type:       platform.TargetRefUHostIDUserInput,
				Value:      rt.FallbackInstanceID,
				Source:     platform.SourcePriorTurn,
				SourceSpan: rt.FallbackInstanceID,
			}}
		} else {
			return nil, nil, ReadFallbackBeforeTool(platform.ReadFallbackMissingTarget)
		}
	}
	// A cold exact ID is locally unknown, not absent. Carry it to a point Describe,
	// then build the monitor subject only from that upstream response. This keeps
	// the envelope grounded while avoiding a model-driven ResourceInfo detour.
	instances, ids, reason := resolveReadTargetSnapshots(ctx, targets, rt)
	if reason != nil {
		return nil, nil, readTargetFallbackResult(*reason)
	}
	if monitorTargetsNeedVerification(instances) {
		raw, err := rt.Executor.Execute(ctx, resourceInfoAction, readprojection.DescribeResourceArgs(ids))
		if err != nil {
			return nil, nil, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
		}
		describe, err := readprojection.InstancesFromDescribeResult(raw)
		if err != nil {
			return nil, nil, ReadFailureAfterTool(resourceInfoAction, resourceCapabilityLabel, err)
		}
		instances = filterInstancesByRequestedID(describe.Instances, ids)
		if len(instances) != len(ids) {
			return nil, nil, ReadFallbackBeforeTool(platform.ReadFallbackUnresolvedTarget)
		}
		ids = instanceIDsOf(instances)
	}
	return instances, ids, ReadResult{}
}

func monitorTargetsNeedVerification(instances []entity.InstanceSnapshot) bool {
	for _, instance := range instances {
		if instance.UHostId != "" && instance.Name == "" && instance.State == "" &&
			instance.Zone == "" && instance.Region == "" {
			return true
		}
	}
	return false
}

func monitorRender(resp MonitorResponse) ReadResult {
	reply := readprojection.RenderMonitorSummary(resp.Metrics, resp.Raw)
	env := readprojection.BuildMonitorEnvelope(resp.Instances, resp.Metrics, resp.Raw)
	if resp.Historical {
		reply = readprojection.RenderHistoricalMonitorSummary(resp.Metrics, resp.Raw, resp.WindowStart, resp.WindowEnd)
		env = readprojection.BuildHistoricalMonitorEnvelope(resp.Instances, resp.Metrics, resp.Raw, resp.WindowStart, resp.WindowEnd)
	}
	r := ReadHandled(reply)
	r.ToolAction = monitorAction
	r.Envelope = &env
	return r
}
