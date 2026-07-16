package intent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/routing"
)

const FriendlyToolFailureReply = "\u67e5\u8be2\u6682\u65f6\u5931\u8d25\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5\u3002"

type HandlerStatus string

const (
	HandlerStatusHandled            HandlerStatus = "handled"
	HandlerStatusNeedsInput         HandlerStatus = "needs_input"
	HandlerStatusFallbackBeforeTool HandlerStatus = "fallback_before_tool"
	HandlerStatusFailureAfterTool   HandlerStatus = "failure_after_tool"
)

// HandlerFailureClass is control-flow metadata. User-facing wording may change
// without changing whether the engine should continue into its context-aware,
// read-only agent lane.
type HandlerFailureClass string

const (
	HandlerFailureNone               HandlerFailureClass = ""
	HandlerFailureGenericRead        HandlerFailureClass = "generic_read"
	HandlerFailureActionableUpstream HandlerFailureClass = "actionable_upstream"
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

type RouteStatus string

const (
	RouteStatusNone       RouteStatus = ""
	RouteStatusDispatched RouteStatus = "dispatched"
	// RouteStatusDispatchedAgent marks a turn the agent-tier dispatch handler
	// owned (B8.3 deploy_model). Distinct from "dispatched" (fast-tier route
	// dispatch) so DeriveActualExecutionTier maps it to the agent tier rather than fast
	// — the deploy handler runs a TierAgent LLM match + the orchestrator saga.
	RouteStatusDispatchedAgent RouteStatus = "dispatched_agent"
	// RouteStatusDispatchedKnowledgeAgentLoop marks a knowledge_qa turn that the
	// knowledge_qa route sent into the shared context-aware
	// ReAct knowledge loop, instead of the terminal-RAG route
	// (dispatched_retrieval). Distinct so mainline reports tell the agent-loop
	// knowledge turn apart from BOTH the terminal-RAG route AND the deploy_model
	// agent-skill dispatch (dispatched_agent): DeriveActualExecutionPath maps it to
	// agent (the turn runs the agent loop) while DeriveActualExecutionTier maps it to
	// knowledge (it answers a knowledge question via retrieval — keeping the realized
	// knowledge-work attribution stable across the terminal→agent-loop migration).
	// Trace-only; emitted by the engine's tryPlannerDispatch, no planner prompt / SHA impact.
	RouteStatusDispatchedKnowledgeAgentLoop RouteStatus = "dispatched_knowledge_agent_loop"
	RouteStatusFallbackInvalid              RouteStatus = "fallback_invalid"
	RouteStatusFallbackLowConfidence        RouteStatus = "fallback_low_confidence"
	// RouteStatusFallbackHardBlockHint (removed PR #61, 2026-05-21):
	// planner's HardBlockHint is advisory only — no longer routes. Survives
	// in RouterTrace.HardBlockHint for analytics join with
	// EngineHardBlockTrace. Deterministic refusal comes from keyword
	// PreBlock + IntentMonitorHistory dispatcher.
	RouteStatusFallbackIneligible        RouteStatus = "fallback_ineligible"
	RouteStatusFallbackUnresolvedTarget  RouteStatus = "fallback_unresolved_target"
	RouteStatusFallbackTimeWindow        RouteStatus = "fallback_time_window"
	RouteStatusFailureAfterTool          RouteStatus = "failure_after_tool"
	RouteStatusDispatchedRetrieval       RouteStatus = "dispatched_retrieval"
	RouteStatusFallbackRetrievalMiss     RouteStatus = "fallback_retrieval_miss"
	RouteStatusFallbackRetrievalDisabled RouteStatus = "fallback_retrieval_disabled"
	RouteStatusSelectionRequired         RouteStatus = "selection_required"
)

type HandlerResult struct {
	Status HandlerStatus
	Reply  string
	// NeedsClarification marks a complete deterministic clarification rather
	// than a factual answer. The engine may let the context-aware Agent resolve
	// it when the current utterance clearly depends on a recent complete turn.
	NeedsClarification bool
	FallbackReason     FallbackReason
	RouteStatus        RouteStatus
	FailureClass       HandlerFailureClass
	ToolAction         string
	ToolArgs           map[string]any
	Envelope           *envelope.Envelope
	// RendererInputToolArgHashes records tool args consumed by deterministic
	// handler renderers before engine-level tool call ids exist. Phase 1 demo
	// populates this for monitor handler results only.
	RendererInputToolArgHashes  []string
	RendererInputEnvelopeHashes []string
	// ResourceSelectionCandidates is the ordered instance list actually surfaced
	// in a resource_info reply. Engine persists this list so a later "第 N 台 /
	// 这台" follow-up can only resolve to an item the user saw.
	ResourceSelectionCandidates []entity.InstanceSnapshot
	// ResolvedStockGpuModel is the single GPU model (API instance-type Name,
	// e.g. "4090") a stock-availability turn resolved to, or "" when the turn
	// was ambiguous / listed all models. engine.go records it into
	// SessionState.LastStockGpuModel so a later subject-eliding stock turn can
	// reuse it as the referent (RC017). Populated by handleStockAvailability only.
	ResolvedStockGpuModel string
}

func ClarificationResult(reply string) HandlerResult {
	result := HandledResult(reply)
	result.NeedsClarification = true
	return result
}

type HandlerExecutor interface {
	Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

type internalHandlerExecutor interface {
	ExecuteInternal(ctx context.Context, action string, args map[string]any) (map[string]any, error)
}

type HandlerRequest struct {
	Plan     IntentRoute
	Resolver EntityResolver
	// FallbackInstanceID is the SelectedInstanceID from SessionState. When
	// TargetRefs is empty and this is non-empty, instance-scoped follow-up
	// handlers such as monitor_query and refund_estimate may use it as the
	// default target instead of triggering resource selection.
	// Set by engine.go from e.sessionState at the tryRouteDispatch call site.
	FallbackInstanceID string
	// FallbackGpuModel is the stock GPU referent derived by engine.go from
	// session context (fresh RecentFacts when enabled, otherwise the legacy
	// LastStockGpuModel). When the current stock turn elides the subject
	// ("现在还有库存吗") and names no GPU-like token, handleStockAvailability
	// reuses this as the referent instead of re-listing every model (RC017).
	FallbackGpuModel string
}

type DemoHandler struct {
	executor HandlerExecutor
}

func NewDemoHandler(executor HandlerExecutor) *DemoHandler {
	return &DemoHandler{executor: executor}
}

func HandledResult(reply string) HandlerResult {
	return HandlerResult{
		Status:      HandlerStatusHandled,
		Reply:       reply,
		RouteStatus: RouteStatusDispatched,
	}
}

func FallbackBeforeTool(reason FallbackReason) HandlerResult {
	return HandlerResult{
		Status:         HandlerStatusFallbackBeforeTool,
		FallbackReason: reason,
		RouteStatus:    routeStatusForFallback(reason),
	}
}

func FailureAfterTool(label string) HandlerResult {
	reply := FriendlyToolFailureReply
	label = strings.TrimSpace(label)
	if label != "" {
		reply = label + ": " + reply
	}
	return HandlerResult{
		Status:       HandlerStatusFailureAfterTool,
		Reply:        reply,
		RouteStatus:  RouteStatusFailureAfterTool,
		FailureClass: HandlerFailureGenericRead,
	}
}

func (h *DemoHandler) HandleResourceInfo(ctx context.Context, req HandlerRequest) HandlerResult {
	const action = "DescribeCompShareInstance"
	if fallback := RequireAllowedHandlerAction(req.Plan.Intent, action); fallback != nil {
		return *fallback
	}
	if h == nil || h.executor == nil {
		// Defensive only: production wiring must construct the handler with a
		// SafeToolExecutor adapter before enabling demo route dispatch.
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
	// Display-side truncation: cap to DefaultMaxInstancesPerDisplay when the
	// caller didn't pin a specific UHostIds set (i.e. a "list my instances"
	// or "list before write op" path). Instances picked by exact UHostIds
	// are not truncated — the user already chose targets.
	envMeta := ResourceEnvelopeMeta{TotalCount: totalCount}
	if hasFilters && !filters.IsZero() {
		envMeta.FilterApplied = filters.String()
		envMeta.MatchedCount = len(instances)
	}
	var selectionCandidates []entity.InstanceSnapshot
	if len(ids) == 0 {
		truncated, shown, isTruncated := TruncateInstancesForDisplay(instances, 0)
		instances = truncated
		selectionCandidates = append([]entity.InstanceSnapshot(nil), instances...)
		envMeta.Shown = shown
		envMeta.Truncated = isTruncated
	}
	result := HandledResult(RenderResourceSummary(instances, envMeta))
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	if len(selectionCandidates) > 0 {
		result.ResourceSelectionCandidates = selectionCandidates
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
		// SafeToolExecutor adapter before enabling demo route dispatch.
		return FallbackBeforeTool(FallbackValidation)
	}
	historical := req.Plan.Intent == IntentMonitorHistory
	if !historical && !isCurrentMonitorTimeWindow(req.Plan.Slots.TimeWindow) {
		return FallbackBeforeTool(FallbackTimeWindow)
	}
	if len(req.Plan.Slots.TargetRefs) == 0 {
		if req.FallbackInstanceID != "" {
			req.Plan.Slots.TargetRefs = []TargetRef{{
				Type:       TargetRefUHostIDUserInput,
				Value:      req.FallbackInstanceID,
				Source:     SourcePriorTurn,
				SourceSpan: req.FallbackInstanceID,
			}}
		} else {
			return FallbackBeforeTool(FallbackMissingTarget)
		}
	}

	instances, ids, fallback := resolveResourceTargetSnapshots(req.Plan.Slots.TargetRefs, req.Resolver)
	if fallback != nil {
		return *fallback
	}
	args := map[string]any{"UHostIds": append([]string(nil), ids...)}
	if historical {
		if len(ids) != 1 {
			return FallbackBeforeTool(FallbackValidation)
		}
		start, end, ok := resolveMonitorHistoryWindow(req.Plan.Slots.TimeWindow)
		if !ok {
			return FallbackBeforeTool(FallbackTimeWindow)
		}
		args["StartTime"] = start
		args["EndTime"] = end
	}
	raw, err := h.executor.Execute(ctx, action, args)
	if err != nil {
		return failureAfterToolForError(action, args, "monitor_query", err)
	}
	reply := RenderMonitorSummary(req.Plan.Slots.Metrics, raw)
	if historical {
		reply = RenderHistoricalMonitorSummary(req.Plan.Slots.Metrics, raw)
	}
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	result.RendererInputToolArgHashes = hashArgsForRenderer(args)
	env := BuildMonitorEnvelope(instances, req.Plan.Slots.Metrics, raw)
	result.Envelope = &env
	result.RendererInputEnvelopeHashes = hashEnvelopeForRenderer(env)
	return result
}

func routeStatusForFallback(reason FallbackReason) RouteStatus {
	switch reason {
	case FallbackMissingTarget, FallbackUnresolvedTarget, FallbackAmbiguousTarget:
		return RouteStatusFallbackUnresolvedTarget
	case FallbackTimeWindow:
		return RouteStatusFallbackTimeWindow
	case FallbackActionNotAllowed:
		return RouteStatusFallbackIneligible
	default:
		return RouteStatusFallbackInvalid
	}
}

// handlerActionWhitelist gates which (Intent, action) pairs are allowed at the
// SafeToolExecutor boundary. The two legacy entries (resource/monitor) are
// hardcoded; route entries are derived from the generated skill registry —
// each route skill contributes ITS required tool (RequiredTools[0]), NOT its
// broader react_tool_subset, so the security whitelist stays narrow. The curated
// extraHandlerActions() map then adds the security-vetted extras (stock_availability
// only). The exact resulting set is pinned by TestHandlerActionWhitelist_ExactGoldenSet
// so a widening (e.g. a tool leaking from react_tool_subset) fails loudly.
//
// Computed lazily via sync.Once so the derivation runs after the skill registry's
// package init; function-call indirection keeps it off the package-init critical
// path.
var (
	handlerActionWhitelistOnce  sync.Once
	handlerActionWhitelistCache map[Intent]map[string]struct{}
)

func handlerActionWhitelist() map[Intent]map[string]struct{} {
	handlerActionWhitelistOnce.Do(func() {
		m := map[Intent]map[string]struct{}{
			IntentResourceInfo:   {"DescribeCompShareInstance": {}},
			IntentMonitorQuery:   {"GetCompShareInstanceMonitor": {}},
			IntentMonitorHistory: {"GetCompShareInstanceMonitor": {}},
		}
		for _, route := range routing.GeneratedRoutes() {
			if route.IntentLabel == "" || len(route.RequiredTools) == 0 {
				continue
			}
			intentValue := Intent(route.IntentLabel)
			if _, ok := m[intentValue]; !ok {
				m[intentValue] = map[string]struct{}{}
			}
			m[intentValue][route.RequiredTools[0]] = struct{}{}
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
		// A typed upstream error (e.g. *tools.UpstreamAPIError on a 230 / 226604
		// from a direct-dispatched read route) carries a user-facing recovery
		// message. Use it ONLY when non-empty: UpstreamAPIError.UserMessage()
		// returns "" for codes without an actionable hint, in which case we fall
		// through to the generic friendly reply rather than answering blank.
		if msg := strings.TrimSpace(friendly.UserMessage()); msg != "" {
			return HandlerResult{
				Status:       HandlerStatusFailureAfterTool,
				Reply:        msg,
				RouteStatus:  RouteStatusFailureAfterTool,
				FailureClass: HandlerFailureActionableUpstream,
				ToolAction:   action,
				ToolArgs:     copyArgs(args),
			}
		}
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

var monitorNowFunc = time.Now

var monitorHistoryLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

var relativeMonitorWindowRE = regexp.MustCompile(`(?i)(?:过去|最近|近|past|last|previous)\s*(\d+)\s*([a-z]+|分钟|分|小时|时)`)

func resolveMonitorHistoryWindow(window *TimeWindow) (int64, int64, bool) {
	if window == nil {
		return 0, 0, false
	}
	now := monitorNowFunc().In(monitorHistoryLoc)
	var start, end time.Time
	switch window.Type {
	case TimeWindowAbsolute:
		s, e, ok := parseAbsoluteMonitorWindow(window.Value)
		if !ok {
			return 0, 0, false
		}
		start, end = s, e
	case TimeWindowPreset:
		switch strings.ToLower(strings.TrimSpace(window.Value)) {
		case "yesterday", "昨天":
			day := startOfDay(now).AddDate(0, 0, -1)
			start, end = day, day.Add(24*time.Hour)
		case "today", "今天":
			start, end = startOfDay(now), now
		default:
			return 0, 0, false
		}
	case TimeWindowRelative:
		s, e, ok := parseRelativeMonitorWindow(window.Value, now)
		if !ok {
			return 0, 0, false
		}
		start, end = s, e
	default:
		return 0, 0, false
	}
	if !end.After(start) || end.Sub(start) > 24*time.Hour {
		return 0, 0, false
	}
	return start.Unix(), end.Unix(), true
}

func atoiDefault(value string, fallback int) int {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func validClock(hour, minute int) bool {
	return hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59
}

func parseAbsoluteMonitorWindow(value string) (time.Time, time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, time.Time{}, false
	}
	for _, sep := range []string{"/", "~", "～", "到", "至"} {
		parts := strings.Split(value, sep)
		if len(parts) != 2 {
			continue
		}
		start, okStart := parseMonitorTime(parts[0])
		end, okEnd := parseMonitorTimeWithDefaultDate(parts[1], start)
		if okStart && okEnd {
			return start, end, true
		}
	}
	return time.Time{}, time.Time{}, false
}

func parseRelativeMonitorWindow(value string, now time.Time) (time.Time, time.Time, bool) {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return time.Time{}, time.Time{}, false
	}
	if strings.Contains(lower, "yesterday") || strings.Contains(lower, "昨天") {
		day := startOfDay(now).AddDate(0, 0, -1)
		return day, day.Add(24 * time.Hour), true
	}
	m := relativeMonitorWindowRE.FindStringSubmatch(lower)
	if len(m) != 3 {
		return time.Time{}, time.Time{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}, time.Time{}, false
	}
	unit := strings.ToLower(strings.TrimSpace(m[2]))
	d := time.Duration(n) * time.Minute
	switch unit {
	case "分钟", "分", "minute", "minutes", "min", "m":
		d = time.Duration(n) * time.Minute
	case "小时", "时", "hour", "hours", "h":
		d = time.Duration(n) * time.Hour
	default:
		return time.Time{}, time.Time{}, false
	}
	return now.Add(-d), now, true
}

func parseMonitorTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if match := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})\s+(\d{1,2}):(\d{1,2})(?::(\d{1,2}))?`).FindStringSubmatch(value); len(match) > 0 {
		second := match[4]
		if second == "" {
			second = "00"
		}
		normalized := fmt.Sprintf("%s %02s:%02s:%02s", match[1], match[2], match[3], second)
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", normalized, monitorHistoryLoc); err == nil {
			return t, true
		}
	}
	if match := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})T(\d{1,2}:\d{2}(?::\d{2})?(?:Z|[+-]\d{2}:\d{2}))`).FindStringSubmatch(value); len(match) > 0 {
		if t, err := time.Parse(time.RFC3339, match[1]+"T"+match[2]); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, value, monitorHistoryLoc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseMonitorTimeWithDefaultDate(value string, defaultDate time.Time) (time.Time, bool) {
	if t, ok := parseMonitorTime(value); ok {
		return t, true
	}
	value = strings.TrimSpace(value)
	m := regexp.MustCompile(`(?:^|\D)(\d{1,2})(?:\s*(?::|点|时)\s*(\d{1,2})?)?`).FindStringSubmatch(value)
	if len(m) == 0 || defaultDate.IsZero() {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, false
	}
	minute := atoiDefault(m[2], 0)
	if !validClock(hour, minute) {
		return time.Time{}, false
	}
	base := defaultDate.In(monitorHistoryLoc)
	y, mon, d := base.Date()
	return time.Date(y, mon, d, hour, minute, 0, 0, monitorHistoryLoc), true
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.In(monitorHistoryLoc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, monitorHistoryLoc)
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
	return platform.CopyArgs(args)
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

func safeValue(v any) string {
	return platform.SafeValue(v)
}
