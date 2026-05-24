package intent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
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
	// CutoverStatusFallbackHardBlockHint (removed PR #61, 2026-05-21):
	// planner's HardBlockHint is advisory only — no longer routes. Survives
	// in PlannerTrace.HardBlockHint for analytics join with
	// EngineHardBlockTrace. Deterministic refusal comes from keyword
	// PreBlock + IntentMonitorHistory dispatcher.
	CutoverStatusFallbackIneligible        CutoverStatus = "fallback_ineligible"
	CutoverStatusFallbackUnresolvedTarget  CutoverStatus = "fallback_unresolved_target"
	CutoverStatusFallbackTimeWindow        CutoverStatus = "fallback_time_window"
	CutoverStatusFailureAfterTool          CutoverStatus = "failure_after_tool"
	CutoverStatusDispatchedRetrieval       CutoverStatus = "dispatched_retrieval"
	CutoverStatusFallbackRetrievalMiss     CutoverStatus = "fallback_retrieval_miss"
	CutoverStatusFallbackRetrievalDisabled CutoverStatus = "fallback_retrieval_disabled"
	CutoverStatusSelectionRequired         CutoverStatus = "selection_required"
)

type HandlerResult struct {
	Status         HandlerStatus
	Reply          string
	FallbackReason FallbackReason
	CutoverStatus  CutoverStatus
	ToolAction     string
	ToolArgs       map[string]any
	Envelope       *envelope.Envelope
	// RendererInputToolArgHashes records tool args consumed by deterministic
	// handler renderers before engine-level tool call ids exist. Phase 1 demo
	// populates this for monitor handler results only.
	RendererInputToolArgHashes  []string
	RendererInputEnvelopeHashes []string
}

type HandlerExecutor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

type HandlerRequest struct {
	Plan     Plan
	Resolver EntityResolver
	// UserText is the raw user question. Used by capability handlers'
	// deterministic NL filter (e.g. "4090 显存多大" -> filter Name=="4090" out
	// of the API response). Set by engine.go when dispatching to handlers via
	// tryPhase1Cutover / tryResumeResourceSelection. Legacy handlers
	// (HandleResourceInfo / HandleMonitorQuery) ignore this field.
	UserText string
	// Region is the deployment region the agent is calling the upstream API
	// against (`UserFrom(ctx).Region` on HTTP path, falling back to
	// cfg.Agent.Region on CLI). Empty when neither is set.
	//
	// Capability handlers that fan out tool calls across zones returned by a
	// listing API (e.g. stock capacity precheck) must filter zones to those
	// matching this Region prefix — the production gateway rejects mismatched
	// pairs with `RetCode=230 "Params [Zone] not available"`. Empty Region
	// disables the filter (preserves legacy behavior in tests).
	Region string
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

	var ids []string
	var filters ResourceFilterSet
	hasFilters := containsFilterRef(req.Plan.Slots.TargetRefs)
	if hasFilters {
		parsed, err := ParseResourceFilters(req.Plan.Slots.TargetRefs)
		if err != nil {
			return FallbackBeforeTool(FallbackValidation)
		}
		filters = parsed
	} else {
		resolvedIDs, fallback := resolveResourceTargets(req.Plan.Slots.TargetRefs, req.Resolver)
		if fallback != nil {
			return *fallback
		}
		ids = resolvedIDs
	}
	args := describeResourceArgs(ids)
	raw, err := h.executor.Execute(ctx, action, args)
	if err != nil {
		return failureAfterToolForError(action, args, "resource_info", err)
	}
	describeData, err := instancesFromDescribeResult(raw)
	if err != nil {
		return failureAfterToolWithTrace(action, args, "resource_info")
	}
	instances := describeData.Instances
	totalCount := describeData.TotalCount
	if hasFilters {
		instances = applyResourceFilters(instances, filters)
	}
	result := HandledResult(RenderResourceSummary(instances))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	envMeta := ResourceEnvelopeMeta{TotalCount: totalCount}
	if hasFilters && !filters.IsZero() {
		envMeta.FilterApplied = filters.String()
		envMeta.MatchedCount = len(instances)
	}
	env := BuildResourceEnvelopeWithMeta(instances, envMeta)
	result.Envelope = &env
	result.RendererInputEnvelopeHashes = hashEnvelopeForRenderer(env)
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

	instances, ids, fallback := resolveResourceTargetSnapshots(req.Plan.Slots.TargetRefs, req.Resolver)
	if fallback != nil {
		return *fallback
	}
	args := map[string]any{"UHostIds": append([]string(nil), ids...)}
	raw, err := h.executor.Execute(ctx, action, args)
	if err != nil {
		return failureAfterToolForError(action, args, "monitor_query", err)
	}
	result := HandledResult(RenderMonitorSummary(req.Plan.Slots.Metrics, raw))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	result.RendererInputToolArgHashes = hashArgsForRenderer(args)
	env := BuildMonitorEnvelope(instances, req.Plan.Slots.Metrics, raw)
	result.Envelope = &env
	result.RendererInputEnvelopeHashes = hashEnvelopeForRenderer(env)
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

// handlerActionWhitelist gates which (Intent, action) pairs are allowed at the
// SafeToolExecutor boundary. Legacy entries (resource/monitor) are hardcoded;
// capability entries are derived from capabilityRegistry so the registry table
// is the single source of truth (test TestHandlerActionWhitelist_DerivesFromRegistry
// enforces drift prevention).
//
// Computed lazily via sync.Once to break the package-init cycle that would
// otherwise form through:
//
//	var whitelist = derive(capabilityRegistry)  -> takes pointers to handlers ->
//	  handlers call RequireAllowedHandlerAction -> which reads `whitelist`.
//
// Function-call indirection sidesteps Go's init-time cycle check.
var (
	handlerActionWhitelistOnce  sync.Once
	handlerActionWhitelistCache map[Intent]map[string]struct{}
)

func handlerActionWhitelist() map[Intent]map[string]struct{} {
	handlerActionWhitelistOnce.Do(func() {
		m := map[Intent]map[string]struct{}{
			IntentResourceInfo: {
				"DescribeCompShareInstance": {},
			},
			IntentMonitorQuery: {
				"GetCompShareInstanceMonitor": {},
			},
		}
		for _, e := range capabilityRegistry {
			if _, ok := m[e.intent]; !ok {
				m[e.intent] = map[string]struct{}{}
			}
			m[e.intent][e.requiredTool] = struct{}{}
		}
		for intentValue, actions := range extraHandlerActions() {
			if _, ok := m[intentValue]; !ok {
				m[intentValue] = map[string]struct{}{}
			}
			for _, action := range actions {
				m[intentValue][action] = struct{}{}
			}
		}
		handlerActionWhitelistCache = m
	})
	return handlerActionWhitelistCache
}

func IsAllowedHandlerAction(intent Intent, action string) bool {
	allowed, ok := handlerActionWhitelist()[intent]
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
	instances, ids, result := resolveResourceTargetSnapshots(refs, resolver)
	if result != nil {
		return nil, result
	}
	_ = instances
	return ids, nil
}

func resolveResourceTargetSnapshots(refs []TargetRef, resolver EntityResolver) ([]entity.InstanceSnapshot, []string, *HandlerResult) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	if resolver == nil {
		result := FallbackBeforeTool(FallbackUnresolvedTarget)
		return nil, nil, &result
	}

	ids := make([]string, 0, len(refs))
	instances := make([]entity.InstanceSnapshot, 0, len(refs))
	for _, ref := range refs {
		switch ref.Type {
		case TargetRefUHostIDUserInput:
			inst, res := resolver.ResolveByID(ref.Value)
			if res.Status != entity.ResolveHit || inst == nil {
				result := FallbackBeforeTool(FallbackUnresolvedTarget)
				return nil, nil, &result
			}
			ids = append(ids, inst.UHostId)
			instances = append(instances, *inst)
		case TargetRefName:
			matches, res := resolver.ResolveByName(ref.Value)
			if res.Status == entity.ResolveAmbiguous || len(matches) > 1 {
				result := FallbackBeforeTool(FallbackAmbiguousTarget)
				return nil, nil, &result
			}
			if res.Status != entity.ResolveHit || len(matches) == 0 || matches[0] == nil {
				result := FallbackBeforeTool(FallbackUnresolvedTarget)
				return nil, nil, &result
			}
			ids = append(ids, matches[0].UHostId)
			instances = append(instances, *matches[0])
		default:
			result := FallbackBeforeTool(FallbackValidation)
			return nil, nil, &result
		}
	}
	instances = dedupeInstanceSnapshots(instances)
	ids = make([]string, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.UHostId)
	}
	sort.Strings(ids)
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].UHostId < instances[j].UHostId
	})
	return instances, ids, nil
}

func failureAfterToolWithTrace(action string, args map[string]any, label string) HandlerResult {
	result := FailureAfterTool(label)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

type userFacingError interface {
	UserMessage() string
}

func failureAfterToolForError(action string, args map[string]any, label string, err error) HandlerResult {
	var friendly userFacingError
	if errors.As(err, &friendly) {
		result := HandlerResult{
			Status:        HandlerStatusFailureAfterTool,
			Reply:         friendly.UserMessage(),
			CutoverStatus: CutoverStatusFailureAfterTool,
			ToolAction:    action,
			ToolArgs:      copyArgs(args),
		}
		return result
	}
	return failureAfterToolWithTrace(action, args, label)
}

func isCurrentMonitorTimeWindow(window *TimeWindow) bool {
	if window == nil {
		return true
	}
	if window.Type != TimeWindowPreset {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(window.Value)) {
	case "now", "current", "realtime":
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

type resourceDescribeData struct {
	Instances  []entity.InstanceSnapshot
	TotalCount int
	Truncated  bool
}

func instancesFromDescribeResult(raw map[string]any) (resourceDescribeData, error) {
	reg := entity.NewRegistry()
	if err := reg.SyncFromDescribe(raw, "handler_resource"); err != nil {
		return resourceDescribeData{}, err
	}
	snap := reg.Snapshot()
	instances := make([]entity.InstanceSnapshot, 0, len(snap.Instances))
	for _, inst := range snap.Instances {
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].UHostId < instances[j].UHostId
	})
	totalCount := snap.TotalCount
	if totalCount == 0 && len(instances) > 0 {
		totalCount = len(instances)
	}
	return resourceDescribeData{
		Instances:  instances,
		TotalCount: totalCount,
		Truncated:  snap.Truncated,
	}, nil
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

func hashEnvelopeForRenderer(env envelope.Envelope) []string {
	hash, err := envelope.Hash(env)
	if err != nil {
		panic(fmt.Sprintf("hash renderer envelope: %v", err))
	}
	return []string{hash}
}

func dedupeInstanceSnapshots(values []entity.InstanceSnapshot) []entity.InstanceSnapshot {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]entity.InstanceSnapshot, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value.UHostId]; ok {
			continue
		}
		seen[value.UHostId] = struct{}{}
		out = append(out, value)
	}
	return out
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
	resourceLabelStartTime  = "\u542f\u52a8\u65f6\u95f4"
	resourceLabelExpireTime = "\u5230\u671f\u65f6\u95f4"

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
			resourceLabelInstanceID + "=" + safeValue(inst.UHostId),
			resourceLabelName + "=" + safeValue(inst.Name),
			resourceLabelState + "=" + safeValue(inst.State),
			resourceLabelGPUType + "=" + safeValue(inst.GpuType),
			fmt.Sprintf("%s=%d", resourceLabelGPU, inst.GPU),
			fmt.Sprintf("%s=%d", resourceLabelCPU, inst.CPU),
			fmt.Sprintf("%s=%d", resourceLabelMemory, inst.Memory),
		}
		if inst.ImageType != "" {
			parts = append(parts, resourceLabelImageType+"="+safeValue(inst.ImageType))
		}
		if inst.StartTime != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", resourceLabelStartTime, inst.StartTime))
		}
		if inst.ExpireTime != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", resourceLabelExpireTime, inst.ExpireTime))
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n")
}

func RenderMonitorSummary(metrics []Metric, payload map[string]any) string {
	allFacts := monitorScalarFacts(nil, payload)
	if len(allFacts) == 0 {
		return noMonitorValuesReply
	}

	facts := allFacts
	if len(metrics) > 0 {
		facts = monitorScalarFacts(metrics, payload)
	}
	if len(facts) == 0 {
		return noRequestedMonitorValuesReply
	}

	parts := make([]string, 0, len(facts))
	for _, fact := range facts {
		value := fact.Value
		if fact.Unit != "" {
			value += fact.Unit
		}
		label := fact.Label
		if label == "" {
			label = fact.Key
		}
		parts = append(parts, label+"="+value)
	}
	if len(metrics) > 0 {
		present := presentMonitorMetrics(facts)
		for _, metric := range uniqueMonitorMetrics(metrics) {
			if !present[metric] {
				parts = append(parts, monitorMetricReplyLabel(metric)+"未返回数据")
			}
		}
	}
	return strings.Join(parts, "; ")
}

func uniqueMonitorMetrics(metrics []Metric) []Metric {
	seen := map[Metric]struct{}{}
	out := make([]Metric, 0, len(metrics))
	for _, metric := range metrics {
		if metric == "" {
			continue
		}
		if _, ok := seen[metric]; ok {
			continue
		}
		seen[metric] = struct{}{}
		out = append(out, metric)
	}
	return out
}

func presentMonitorMetrics(facts []monitorScalarFact) map[Metric]bool {
	present := map[Metric]bool{}
	for _, fact := range facts {
		key := strings.ToLower(fact.Key)
		switch {
		case strings.Contains(key, "cpu"):
			present[MetricCPU] = true
		case strings.Contains(key, "vram"):
			present[MetricVRAM] = true
		case strings.Contains(key, "gpu"):
			present[MetricGPU] = true
		case strings.Contains(key, "memory"):
			present[MetricMemory] = true
		}
	}
	return present
}

func monitorMetricReplyLabel(metric Metric) string {
	switch metric {
	case MetricCPU:
		return "CPU 使用率"
	case MetricMemory:
		return "内存使用率"
	case MetricGPU:
		return "GPU 使用率"
	case MetricVRAM:
		return "显存使用率"
	default:
		return string(metric)
	}
}

func flattenScalars(prefix string, v any, out map[string]string) {
	switch typed := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flattenScalars(next, typed[key], out)
		}
	case []any:
		for i, item := range typed {
			next := fmt.Sprintf("%s[%d]", prefix, i)
			flattenScalars(next, item, out)
		}
	default:
		if prefix != "" {
			out[prefix] = safeValue(typed)
		}
	}
}

func matchesRequestedMetric(key string, metrics []Metric) bool {
	key = strings.ToLower(key)
	for _, metric := range metrics {
		// Demo cutover intentionally uses substring matching over the rendered
		// monitor field paths. Narrow this only if real smoke traces show noisy
		// API metadata in user-visible replies.
		if strings.Contains(key, string(metric)) {
			return true
		}
	}
	return false
}

func safeValue(v any) string {
	return fmt.Sprint(security.RedactForLLM(v))
}

func safeValueMap(v map[string]any) map[string]any {
	if redacted, ok := security.RedactForLLM(v).(map[string]any); ok {
		return redacted
	}
	return map[string]any{}
}
