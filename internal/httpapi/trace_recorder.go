package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

type traceEnqueuer interface {
	Enqueue(observability.TenantContext, observability.TraceRecord) error
}

type chatTraceRecorder struct {
	writer                observability.Writer
	tenant                observability.TenantContext
	record                observability.TraceRecord
	start                 time.Time
	totalTokens           int
	pendingByID           map[string][]int
	registryTraceSupplier func(time.Time) observability.EntityRegistryTrace
}

func newChatTraceRecorder(
	writer observability.Writer,
	base BaseRequest,
	sessionID string,
	turnIndex int,
	userMsg string,
	start time.Time,
) *chatTraceRecorder {
	if writer == nil {
		return nil
	}
	userMsgHash, _ := observability.HashTracePayload(userMsg)
	return &chatTraceRecorder{
		writer: writer,
		tenant: observability.TenantContext{
			TopOrgID:     int64(base.Owner.TopOrganizationID),
			OrgID:        int64(base.Owner.OrganizationID),
			ConnectionID: sessionID,
		},
		record: observability.TraceRecord{
			SchemaVersion: observability.SchemaVersion,
			TraceID:       base.RequestUUID,
			TurnID:        fmt.Sprintf("turn-%d", turnIndex),
			TurnIndex:     turnIndex,
			UserMsgHash:   userMsgHash,
		},
		start:       start,
		pendingByID: map[string][]int{},
	}
}

func attachChatTraceObservers(agent *engine.Engine, recorder *chatTraceRecorder) {
	if agent == nil || recorder == nil {
		return
	}
	agent.SetPlannerTraceObserver(recorder.SetPlannerTrace)
	agent.SetRetrievalTraceObserver(recorder.SetRetrievalTrace)
	agent.SetOutcomeTraceObserver(recorder.SetOutcomeTrace)
	agent.SetRendererTraceObserver(recorder.SetRendererTrace)
	agent.SetHardBlockObserver(recorder.SetEngineHardBlock)
	agent.SetRateLimitObserver(recorder.SetRateLimitDecision)
	agent.SetTokenUsageObserver(recorder.AddTokenUsage)
}

func clearChatTraceObservers(agent *engine.Engine) {
	if agent == nil {
		return
	}
	agent.SetPlannerTraceObserver(nil)
	agent.SetRetrievalTraceObserver(nil)
	agent.SetOutcomeTraceObserver(nil)
	agent.SetRendererTraceObserver(nil)
	agent.SetHardBlockObserver(nil)
	agent.SetRateLimitObserver(nil)
	agent.SetTokenUsageObserver(nil)
}

func (r *chatTraceRecorder) SetRegistryTraceSupplier(supplier func(time.Time) observability.EntityRegistryTrace) {
	if r == nil {
		return
	}
	r.registryTraceSupplier = supplier
}

func (r *chatTraceRecorder) SetPlannerTrace(trace observability.PlannerTrace) {
	if r == nil {
		return
	}
	r.record.Planner = trace
	r.totalTokens += trace.InputTokens + trace.OutputTokens
}

func (r *chatTraceRecorder) SetRetrievalTrace(trace observability.RetrievalTrace) {
	if r == nil {
		return
	}
	r.record.Retrieval = trace
}

func (r *chatTraceRecorder) SetOutcomeTrace(trace observability.OutcomeTrace) {
	if r == nil {
		return
	}
	r.record.Outcome.AttemptedHallucinatedCount = trace.AttemptedHallucinatedCount
	r.record.Outcome.EscapedHallucinatedCount = trace.EscapedHallucinatedCount
	r.record.Outcome.KBConflictCount = trace.KBConflictCount
}

func (r *chatTraceRecorder) SetRendererTrace(trace observability.RendererTrace) {
	if r == nil {
		return
	}
	r.record.Renderer = trace
}

func (r *chatTraceRecorder) SetEngineHardBlock(trace observability.EngineHardBlockTrace) {
	if r == nil {
		return
	}
	r.record.EngineHardBlock = trace
}

func (r *chatTraceRecorder) SetRateLimitDecision(decision governance.Decision) {
	if r == nil {
		return
	}
	trace := observability.RateLimitTrace{
		Checked:      true,
		Allowed:      decision.Allowed,
		Class:        string(decision.Class),
		Action:       decision.Action,
		Reason:       string(decision.Reason),
		SubjectHash:  decision.SubjectHash,
		RetryAfterMS: decision.RetryAfter.Milliseconds(),
	}
	current := r.record.RateLimit
	if !current.Checked {
		r.record.RateLimit = trace
		return
	}
	if !current.Allowed {
		return
	}
	if !trace.Allowed {
		r.record.RateLimit = trace
		return
	}
	r.record.RateLimit = trace
}

func (r *chatTraceRecorder) AddTokenUsage(usage llm.TokenUsage) {
	if r == nil {
		return
	}
	r.totalTokens += traceTokenUsageTotal(usage)
}

func (r *chatTraceRecorder) OnStep(ev engine.StepEvent) {
	if r == nil || ev.Action == "" {
		return
	}
	source := ev.Source
	if source == "" {
		source = observability.ToolSourceMainReAct
	}
	key := source + "\x00" + ev.Action
	switch ev.Type {
	case engine.StepToolCall:
		argsHash, _ := observability.HashTracePayload(ev.Args)
		requestedTargets := ev.RequestedTargets
		if requestedTargets == 0 {
			requestedTargets = traceRequestedTargets(ev.Args)
		}
		windowSeconds := ev.WindowSeconds
		if windowSeconds == 0 {
			windowSeconds = traceWindowSeconds(ev.Args)
		}
		r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
			ID:               fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
			TurnIndex:        r.record.TurnIndex,
			Action:           ev.Action,
			Source:           source,
			ArgsHash:         argsHash,
			RequestedTargets: requestedTargets,
			WindowSeconds:    windowSeconds,
		})
		r.pendingByID[key] = append(r.pendingByID[key], len(r.record.ToolCalls)-1)
	case engine.StepToolResult:
		idx := r.matchPending(key, ev.Action, source)
		resultHash, _ := observability.HashTracePayload(ev.TraceResult)
		r.record.ToolCalls[idx].Status = observability.ToolStatusSuccess
		r.record.ToolCalls[idx].ResultHash = resultHash
		r.record.ToolCalls[idx].Attempts = ev.Attempts
		if r.record.ToolCalls[idx].RequestedTargets > 0 && r.record.ToolCalls[idx].ExecutedTargets == 0 {
			r.record.ToolCalls[idx].ExecutedTargets = r.record.ToolCalls[idx].RequestedTargets
		}
		if len(ev.RendererInputToolArgHashes) > 0 {
			r.record.Renderer.InputToolArgHashes = append(r.record.Renderer.InputToolArgHashes, ev.RendererInputToolArgHashes...)
		}
	case engine.StepError:
		idx := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].Status = observability.ToolStatusError
		r.record.ToolCalls[idx].ErrorClass = ev.Message
	case engine.StepBlocked:
		idx := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].Status = observability.ToolStatusError
		r.record.ToolCalls[idx].ErrorClass = "blocked"
		r.applyCapFields(idx, ev)
	}
}

// EmitStep accumulates one agent-tier saga StepTrace into THIS turn's record
// (B6.2). The orchestrator saga runner uses the recorder as its StepSink. Steps
// are folded into record.Steps in memory and persisted ONCE at Finish via
// Enqueue/Append → prepareForPersist (which redacts Args/Result) — never a
// per-step INSERT (a per-step INSERT would collide uk_request_uuid: one
// agent_traces row per turn). This makes *chatTraceRecorder satisfy
// orchestrator.StepSink. Per-step SSE event:step (live UI) is fanned separately
// by the HTTP handler, reusing the existing sw.WriteEvent("step", ...) path.
func (r *chatTraceRecorder) EmitStep(step observability.StepTrace) error {
	if r == nil {
		return nil
	}
	r.record.Steps = append(r.record.Steps, step)
	return nil
}

func (r *chatTraceRecorder) Finish(chatErr error, end time.Time) error {
	if r == nil || r.writer == nil {
		return nil
	}
	if r.record.Timestamp == "" {
		r.record.Timestamp = end.UTC().Format(time.RFC3339Nano)
	}
	if r.registryTraceSupplier != nil {
		r.record.EntityRegistry = r.registryTraceSupplier(end)
	}
	if chatErr != nil && r.record.EngineHardBlock.Category == "" {
		r.record.EngineHardBlock = observability.EngineHardBlockTrace{
			Hit:      true,
			Category: "chat_error",
		}
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	r.record.Outcome.TotalTokens = r.totalTokens
	for _, call := range r.record.ToolCalls {
		if call.TurnIndex == r.record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			r.record.Freshness.MonitorCallInCurrentTurn = true
			break
		}
	}
	r.record.RealizedTier = r.record.DeriveRealizedTier()
	if enqueuer, ok := r.writer.(traceEnqueuer); ok {
		return enqueuer.Enqueue(r.tenant, r.record)
	}
	return r.writer.Append(r.record)
}

func traceTokenUsageTotal(usage llm.TokenUsage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.PromptTokens + usage.CompletionTokens
}

func (r *chatTraceRecorder) applyCapFields(idx int, ev engine.StepEvent) {
	call := &r.record.ToolCalls[idx]
	if call.ArgsHash == "" && ev.Args != nil {
		call.ArgsHash, _ = observability.HashTracePayload(ev.Args)
	}
	if call.RequestedTargets == 0 {
		call.RequestedTargets = traceRequestedTargets(ev.Args)
	}
	if call.WindowSeconds == 0 {
		call.WindowSeconds = traceWindowSeconds(ev.Args)
	}
	call.ExecutedTargets = 0
	if ev.Capped != "" {
		call.Capped = ev.Capped
	}
	if ev.CapReason != "" {
		call.CapReason = ev.CapReason
	}
}

func (r *chatTraceRecorder) matchPending(key, action, source string) int {
	if queue := r.pendingByID[key]; len(queue) > 0 {
		idx := queue[0]
		if len(queue) == 1 {
			delete(r.pendingByID, key)
		} else {
			r.pendingByID[key] = queue[1:]
		}
		return idx
	}
	r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
		ID:        fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
		TurnIndex: r.record.TurnIndex,
		Action:    action,
		Source:    source,
	})
	return len(r.record.ToolCalls) - 1
}

func traceRequestedTargets(args map[string]any) int {
	if args == nil {
		return 0
	}
	if count := traceTargetValueCount(args["UHostIds"]); count > 0 {
		return count
	}
	if value, ok := args["UHostId"].(string); ok && strings.TrimSpace(value) != "" {
		return 1
	}
	return 0
}

func traceTargetValueCount(value any) int {
	switch typed := value.(type) {
	case []string:
		return len(typed)
	case []any:
		return len(typed)
	default:
		return 0
	}
}

func traceWindowSeconds(args map[string]any) int {
	if args == nil {
		return 0
	}
	start, okStart := traceInt64(args["StartTime"])
	end, okEnd := traceInt64(args["EndTime"])
	if !okStart || !okEnd || start < 0 || end < 0 || end <= start {
		return 0
	}
	return int(end - start)
}

func traceInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, false
		}
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case float32:
		f := float64(typed)
		if math.IsNaN(f) || math.IsInf(f, 0) || f < math.MinInt64 || f > math.MaxInt64 {
			return 0, false
		}
		if f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return n, err == nil
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}
