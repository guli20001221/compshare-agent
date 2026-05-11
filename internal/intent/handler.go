package intent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/security"
)

const FriendlyToolFailureReply = "\u67e5\u8be2\u6682\u65f6\u5931\u8d25\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5\u3002"

type HandlerStatus string

const (
	HandlerStatusHandled            HandlerStatus = "handled"
	HandlerStatusFallbackBeforeTool HandlerStatus = "fallback_before_tool"
	HandlerStatusFailureAfterTool   HandlerStatus = "failure_after_tool"
)

type FallbackReason string

const (
	FallbackNone             FallbackReason = ""
	FallbackMissingTarget    FallbackReason = "missing_target"
	FallbackUnresolvedTarget FallbackReason = "unresolved_target"
	FallbackAmbiguousTarget  FallbackReason = "ambiguous_target"
	FallbackTimeWindow       FallbackReason = "time_window"
	FallbackValidation       FallbackReason = "validation"
	FallbackActionNotAllowed FallbackReason = "action_not_allowed"
)

type CutoverStatus string

const (
	CutoverStatusNone                      CutoverStatus = ""
	CutoverStatusDispatched                CutoverStatus = "dispatched"
	CutoverStatusFallbackInvalid           CutoverStatus = "fallback_invalid"
	CutoverStatusFallbackLowConfidence     CutoverStatus = "fallback_low_confidence"
	CutoverStatusFallbackHardBlockHint     CutoverStatus = "fallback_hard_block_hint"
	CutoverStatusFallbackIneligible        CutoverStatus = "fallback_ineligible"
	CutoverStatusFallbackUnresolvedTarget  CutoverStatus = "fallback_unresolved_target"
	CutoverStatusFallbackTimeWindow        CutoverStatus = "fallback_time_window"
	CutoverStatusFailureAfterTool          CutoverStatus = "failure_after_tool"
	CutoverStatusDispatchedRetrieval       CutoverStatus = "dispatched_retrieval"
	CutoverStatusFallbackRetrievalMiss     CutoverStatus = "fallback_retrieval_miss"
	CutoverStatusFallbackRetrievalDisabled CutoverStatus = "fallback_retrieval_disabled"
)

type HandlerResult struct {
	Status         HandlerStatus
	Reply          string
	FallbackReason FallbackReason
	CutoverStatus  CutoverStatus
	ToolAction     string
	ToolArgs       map[string]any
	// RendererInputToolArgHashes records tool args consumed by deterministic
	// handler renderers before engine-level tool call ids exist. Phase 1 demo
	// populates this for monitor handler results only.
	RendererInputToolArgHashes []string
}

type HandlerExecutor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

type HandlerRequest struct {
	Plan     Plan
	Resolver EntityResolver
}

type DemoHandler struct {
	executor HandlerExecutor
}

func NewDemoHandler(executor HandlerExecutor) *DemoHandler {
	return &DemoHandler{executor: executor}
}

func HandledResult(reply string) HandlerResult {
	return HandlerResult{
		Status:        HandlerStatusHandled,
		Reply:         reply,
		CutoverStatus: CutoverStatusDispatched,
	}
}

func FallbackBeforeTool(reason FallbackReason) HandlerResult {
	return HandlerResult{
		Status:         HandlerStatusFallbackBeforeTool,
		FallbackReason: reason,
		CutoverStatus:  cutoverStatusForFallback(reason),
	}
}

func FailureAfterTool(label string) HandlerResult {
	reply := FriendlyToolFailureReply
	label = strings.TrimSpace(label)
	if label != "" {
		reply = label + ": " + reply
	}
	return HandlerResult{
		Status:        HandlerStatusFailureAfterTool,
		Reply:         reply,
		CutoverStatus: CutoverStatusFailureAfterTool,
	}
}

func (h *DemoHandler) HandleResourceInfo(ctx context.Context, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareInstance"
	if fallback := RequireAllowedHandlerAction(req.Plan.Intent, action); fallback != nil {
		return *fallback
	}
	if h == nil || h.executor == nil {
		// Defensive only: production wiring must construct the handler with a
		// SafeToolExecutor adapter before enabling demo cutover.
		return FallbackBeforeTool(FallbackValidation)
	}

	ids, fallback := resolveResourceTargets(req.Plan.Slots.TargetRefs, req.Resolver)
	if fallback != nil {
		return *fallback
	}
	args := describeResourceArgs(ids)
	raw, err := h.executor.Execute(ctx, action, args)
	if err != nil {
		return failureAfterToolWithTrace(action, args, "resource_info", err)
	}
	instances, err := instancesFromDescribeResult(raw)
	if err != nil {
		return failureAfterToolWithTrace(action, args, "resource_info", err)
	}
	result := HandledResult(RenderResourceSummary(instances))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func (h *DemoHandler) HandleMonitorQuery(ctx context.Context, req HandlerRequest) HandlerResult {
	const action = "GetCompShareInstanceMonitor"
	if fallback := RequireAllowedHandlerAction(req.Plan.Intent, action); fallback != nil {
		return *fallback
	}
	if h == nil || h.executor == nil {
		// Defensive only: production wiring must construct the handler with a
		// SafeToolExecutor adapter before enabling demo cutover.
		return FallbackBeforeTool(FallbackValidation)
	}
	if !isCurrentMonitorTimeWindow(req.Plan.Slots.TimeWindow) {
		return FallbackBeforeTool(FallbackTimeWindow)
	}
	if len(req.Plan.Slots.TargetRefs) == 0 {
		return FallbackBeforeTool(FallbackMissingTarget)
	}

	ids, fallback := resolveResourceTargets(req.Plan.Slots.TargetRefs, req.Resolver)
	if fallback != nil {
		return *fallback
	}
	args := map[string]any{"UHostIds": append([]string(nil), ids...)}
	raw, err := h.executor.Execute(ctx, action, args)
	if err != nil {
		return failureAfterToolWithTrace(action, args, "monitor_query", err)
	}
	result := HandledResult(RenderMonitorSummary(req.Plan.Slots.Metrics, raw))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	result.RendererInputToolArgHashes = hashArgsForRenderer(args)
	return result
}

func cutoverStatusForFallback(reason FallbackReason) CutoverStatus {
	switch reason {
	case FallbackMissingTarget, FallbackUnresolvedTarget, FallbackAmbiguousTarget:
		return CutoverStatusFallbackUnresolvedTarget
	case FallbackTimeWindow:
		return CutoverStatusFallbackTimeWindow
	case FallbackActionNotAllowed:
		return CutoverStatusFallbackIneligible
	default:
		return CutoverStatusFallbackInvalid
	}
}

var handlerActionWhitelist = map[Intent]map[string]struct{}{
	IntentResourceInfo: {
		"DescribeCompShareInstance": {},
	},
	IntentMonitorQuery: {
		"GetCompShareInstanceMonitor": {},
	},
}

func IsAllowedHandlerAction(intent Intent, action string) bool {
	allowed, ok := handlerActionWhitelist[intent]
	if !ok {
		return false
	}
	_, ok = allowed[action]
	return ok
}

func RequireAllowedHandlerAction(intent Intent, action string) *HandlerResult {
	if IsAllowedHandlerAction(intent, action) {
		return nil
	}
	result := FallbackBeforeTool(FallbackActionNotAllowed)
	return &result
}

func resolveResourceTargets(refs []TargetRef, resolver EntityResolver) ([]string, *HandlerResult) {
	if len(refs) == 0 {
		return nil, nil
	}
	if resolver == nil {
		result := FallbackBeforeTool(FallbackUnresolvedTarget)
		return nil, &result
	}

	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch ref.Type {
		case TargetRefUHostIDUserInput:
			inst, res := resolver.ResolveByID(ref.Value)
			if res.Status != entity.ResolveHit || inst == nil {
				result := FallbackBeforeTool(FallbackUnresolvedTarget)
				return nil, &result
			}
			ids = append(ids, inst.UHostId)
		case TargetRefName:
			matches, res := resolver.ResolveByName(ref.Value)
			if res.Status == entity.ResolveAmbiguous || len(matches) > 1 {
				result := FallbackBeforeTool(FallbackAmbiguousTarget)
				return nil, &result
			}
			if res.Status != entity.ResolveHit || len(matches) == 0 || matches[0] == nil {
				result := FallbackBeforeTool(FallbackUnresolvedTarget)
				return nil, &result
			}
			ids = append(ids, matches[0].UHostId)
		default:
			result := FallbackBeforeTool(FallbackValidation)
			return nil, &result
		}
	}
	ids = dedupeStrings(ids)
	sort.Strings(ids)
	return ids, nil
}

func failureAfterToolWithTrace(action string, args map[string]any, label string, err error) HandlerResult {
	result := FailureAfterTool(label)
	if errors.Is(err, governance.ErrRateLimited) {
		if msg := strings.TrimSpace(err.Error()); msg != "" {
			result.Reply = msg
		}
	}
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func isCurrentMonitorTimeWindow(window *TimeWindow) bool {
	if window == nil {
		return true
	}
	if window.Type != TimeWindowPreset {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(window.Value)) {
	case "now", "current", "realtime", "today":
		return true
	default:
		return false
	}
}

func describeResourceArgs(ids []string) map[string]any {
	if len(ids) == 0 {
		return map[string]any{"Limit": 100}
	}
	return map[string]any{"UHostIds": append([]string(nil), ids...)}
}

func instancesFromDescribeResult(raw map[string]any) ([]entity.InstanceSnapshot, error) {
	reg := entity.NewRegistry()
	if err := reg.SyncFromDescribe(raw, "handler_resource"); err != nil {
		return nil, err
	}
	snap := reg.Snapshot()
	instances := make([]entity.InstanceSnapshot, 0, len(snap.Instances))
	for _, inst := range snap.Instances {
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].UHostId < instances[j].UHostId
	})
	return instances, nil
}

func copyArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		switch typed := value.(type) {
		case []string:
			out[key] = append([]string(nil), typed...)
		default:
			out[key] = typed
		}
	}
	return out
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func hashArgsForRenderer(args map[string]any) []string {
	hash, err := observability.HashTracePayload(args)
	if err != nil {
		panic(fmt.Sprintf("hash monitor handler args: %v", err))
	}
	return []string{hash}
}

const (
	resourceLabelInstanceID = "\u5b9e\u4f8bID"
	resourceLabelName       = "\u540d\u79f0"
	resourceLabelState      = "\u72b6\u6001"
	resourceLabelGPUType    = "GPU\u578b\u53f7"
	resourceLabelGPU        = "GPU\u6570\u91cf"
	resourceLabelCPU        = "CPU"
	resourceLabelMemory     = "\u5185\u5b58"
	resourceLabelImageType  = "\u955c\u50cf\u7c7b\u578b"
	resourceLabelChargeType = "\u8ba1\u8d39\u65b9\u5f0f"

	monitorLabelCPU    = "CPU \u4f7f\u7528\u7387"
	monitorLabelMemory = "\u5185\u5b58\u4f7f\u7528\u7387(Memory)"
	monitorLabelGPU    = "GPU \u4f7f\u7528\u7387"
	monitorLabelVRAM   = "\u663e\u5b58\u4f7f\u7528\u7387(VRAM)"

	noInstancesReply              = "\u672a\u627e\u5230\u5b9e\u4f8b\u3002"
	noMonitorValuesReply          = "\u672a\u8fd4\u56de\u76d1\u63a7\u6570\u636e\u3002"
	noRequestedMonitorValuesReply = "\u672a\u8fd4\u56de\u8bf7\u6c42\u7684\u76d1\u63a7\u6307\u6807\u3002"
)

func RenderResourceSummary(instances []entity.InstanceSnapshot) string {
	copied := append([]entity.InstanceSnapshot(nil), instances...)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].UHostId < copied[j].UHostId
	})
	if len(copied) == 0 {
		return noInstancesReply
	}
	lines := make([]string, 0, len(copied))
	for _, inst := range copied {
		parts := []string{
			resourceLabelInstanceID + ": " + safeValue(inst.UHostId),
			resourceLabelName + ": " + safeValue(inst.Name),
			resourceLabelState + ": " + formatState(inst.State),
			resourceLabelGPUType + ": " + safeValue(inst.GpuType),
			fmt.Sprintf("%s: %d", resourceLabelGPU, inst.GPU),
			fmt.Sprintf("%s: %d", resourceLabelCPU, inst.CPU),
			resourceLabelMemory + ": " + formatMemory(inst.Memory),
		}
		if inst.ImageType != "" {
			parts = append(parts, resourceLabelImageType+": "+safeValue(inst.ImageType))
		}
		if inst.ChargeType != "" {
			parts = append(parts, resourceLabelChargeType+": "+formatChargeType(inst.ChargeType))
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n")
}

func RenderMonitorSummary(metrics []Metric, payload map[string]any) string {
	redacted, _ := security.RedactForLLM(payload).(map[string]any)
	values := extractMonitorValues(redacted)
	if len(values) == 0 {
		values = extractSimpleMonitorValues(redacted)
	}
	if len(values) == 0 {
		return noMonitorValuesReply
	}

	requested := requestedMetricSet(metrics)
	filtered := make([]monitorValue, 0, len(values))
	for _, value := range values {
		if len(requested) == 0 || requested[value.Metric] {
			filtered = append(filtered, value)
		}
	}
	if len(filtered) == 0 {
		return noRequestedMonitorValuesReply
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Order != filtered[j].Order {
			return filtered[i].Order < filtered[j].Order
		}
		if filtered[i].Label != filtered[j].Label {
			return filtered[i].Label < filtered[j].Label
		}
		return filtered[i].Value < filtered[j].Value
	})

	parts := make([]string, 0, len(filtered))
	for _, value := range filtered {
		parts = append(parts, value.Label+": "+value.Value)
	}
	return strings.Join(parts, "\n")
}

type monitorValue struct {
	Metric Metric
	Label  string
	Value  string
	Order  int
}

type monitorMetricSpec struct {
	Metric Metric
	Label  string
	Order  int
}

var monitorMetricSpecs = map[string]monitorMetricSpec{
	"uhost_cpu_used":                 {Metric: MetricCPU, Label: monitorLabelCPU, Order: 1},
	"cpuusagerate":                   {Metric: MetricCPU, Label: monitorLabelCPU, Order: 1},
	"cloudwatch_memory_usage":        {Metric: MetricMemory, Label: monitorLabelMemory, Order: 2},
	"cloudwatch_gpu_util":            {Metric: MetricGPU, Label: monitorLabelGPU, Order: 3},
	"gpuusagerate":                   {Metric: MetricGPU, Label: monitorLabelGPU, Order: 3},
	"cloudwatch_gpu_memory_usage":    {Metric: MetricVRAM, Label: monitorLabelVRAM, Order: 4},
	"cloudwatch_gpu_memory_used_per": {Metric: MetricVRAM, Label: monitorLabelVRAM, Order: 4},
}

func extractMonitorValues(v any) []monitorValue {
	// Phase 1 monitor cutover currently calls the monitor API for one resolved
	// target at a time. If multi-instance monitor cutover is enabled later, this
	// renderer must add instance correlation instead of emitting duplicate labels.
	values := []monitorValue{}
	switch typed := v.(type) {
	case map[string]any:
		if metricKey, ok := monitorMetricKey(typed); ok {
			if spec, ok := monitorMetricSpecs[strings.ToLower(metricKey)]; ok {
				if rendered, ok := latestMetricValue(typed); ok {
					values = append(values, monitorValue{
						Metric: spec.Metric,
						Label:  spec.Label,
						Value:  formatPercentValue(rendered),
						Order:  spec.Order,
					})
				}
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			values = append(values, extractMonitorValues(typed[key])...)
		}
	case []any:
		for _, item := range typed {
			values = append(values, extractMonitorValues(item)...)
		}
	}
	return values
}

func monitorMetricKey(metric map[string]any) (string, bool) {
	for _, key := range []string{"MetricKey", "Name"} {
		if value, ok := metric[key].(string); ok && strings.TrimSpace(value) != "" {
			return value, true
		}
	}
	return "", false
}

func latestMetricValue(metric map[string]any) (any, bool) {
	if value, ok := metric["Value"]; ok && value != nil {
		return value, true
	}
	results, ok := metric["Results"].([]any)
	if !ok {
		return nil, false
	}
	for i := len(results) - 1; i >= 0; i-- {
		result, ok := results[i].(map[string]any)
		if !ok {
			continue
		}
		if value, ok := result["Value"]; ok && value != nil {
			return value, true
		}
		values, ok := result["Values"].([]any)
		if !ok {
			continue
		}
		for j := len(values) - 1; j >= 0; j-- {
			point, ok := values[j].(map[string]any)
			if !ok {
				continue
			}
			if value, ok := point["Value"]; ok && value != nil {
				return value, true
			}
		}
	}
	return nil, false
}

func extractSimpleMonitorValues(payload map[string]any) []monitorValue {
	specs := []struct {
		key    string
		metric Metric
		label  string
		order  int
	}{
		{key: "CPU", metric: MetricCPU, label: monitorLabelCPU, order: 1},
		{key: "Memory", metric: MetricMemory, label: monitorLabelMemory, order: 2},
		{key: "GPU", metric: MetricGPU, label: monitorLabelGPU, order: 3},
		{key: "VRAM", metric: MetricVRAM, label: monitorLabelVRAM, order: 4},
	}
	out := make([]monitorValue, 0, len(specs))
	for _, spec := range specs {
		value, ok := payload[spec.key]
		if !ok || value == nil {
			continue
		}
		out = append(out, monitorValue{
			Metric: spec.metric,
			Label:  spec.label,
			Value:  formatPercentValue(value),
			Order:  spec.order,
		})
	}
	return out
}

func requestedMetricSet(metrics []Metric) map[Metric]bool {
	out := make(map[Metric]bool, len(metrics))
	for _, metric := range metrics {
		out[metric] = true
	}
	return out
}

func formatPercentValue(v any) string {
	switch typed := v.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64) + "%"
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32) + "%"
	case int:
		return fmt.Sprintf("%d%%", typed)
	case int64:
		return fmt.Sprintf("%d%%", typed)
	case string:
		return safeValue(typed)
	default:
		return safeValue(typed)
	}
}

func formatMemory(memory int) string {
	if memory <= 0 {
		return "0 MB"
	}
	if memory >= 1024 {
		gb := float64(memory) / 1024
		if memory%1024 == 0 {
			return fmt.Sprintf("%d GB", memory/1024)
		}
		return strconv.FormatFloat(gb, 'f', 1, 64) + " GB"
	}
	return fmt.Sprintf("%d MB", memory)
}

func formatChargeType(chargeType string) string {
	switch strings.ToLower(strings.TrimSpace(chargeType)) {
	// Canonical CompShare values observed in GT/API traces.
	case "dynamic", "hour", "hourly":
		return "\u6309\u65f6"
	case "day", "daily":
		return "\u6309\u5929"
	case "month", "monthly":
		return "\u5305\u6708"
	case "year", "yearly":
		return "\u5305\u5e74"
	// Defensive aliases for provider-style billing enums.
	case "postpay", "postpaid":
		return "\u6309\u91cf"
	case "prepay", "prepaid":
		return "\u9884\u4ed8\u8d39"
	default:
		return safeValue(chargeType)
	}
}

func formatState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return "\u8fd0\u884c\u4e2d(Running)"
	case "stopped":
		return "\u5173\u673a(Stopped)"
	default:
		return safeValue(state)
	}
}

func safeValue(v any) string {
	return fmt.Sprint(security.RedactForLLM(v))
}
