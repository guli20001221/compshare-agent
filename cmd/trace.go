package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
)

type getenvFunc func(string) string

func traceWriterFromEnv(getenv getenvFunc) (*observability.Writer, bool, error) {
	if getenv("COMPSHARE_TRACE_ENABLED") != "1" {
		return nil, false, nil
	}
	dir := getenv("COMPSHARE_TRACE_DIR")
	writer, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
	if err != nil {
		return nil, false, err
	}
	return writer, true, nil
}

func intentPlannerShadowEnabled(getenv getenvFunc) bool {
	return getenv("USE_INTENT_PLANNER") == "shadow"
}

func intentPlannerCutoverIntentsFromEnv(getenv getenvFunc) ([]intent.Intent, []string) {
	raw := getenv("USE_INTENT_PLANNER_FOR")
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[intent.Intent]struct{}{}
	intents := []intent.Intent{}
	unknown := []string{}
	for _, part := range strings.Split(raw, ",") {
		value := strings.ToLower(strings.TrimSpace(part))
		if value == "" {
			continue
		}
		var enabled intent.Intent
		switch value {
		case "resource":
			enabled = intent.IntentResourceInfo
		case "monitor":
			enabled = intent.IntentMonitorQuery
		default:
			unknown = append(unknown, value)
			continue
		}
		if _, ok := seen[enabled]; ok {
			continue
		}
		seen[enabled] = struct{}{}
		intents = append(intents, enabled)
	}
	return intents, unknown
}

func useSeparateShadowRunner(traceEnabled, shadowEnabled, cutoverEnabled bool) bool {
	return traceEnabled && shadowEnabled && !cutoverEnabled
}

type cliTraceRecorder struct {
	writer                *observability.Writer
	record                observability.TraceRecord
	start                 time.Time
	pendingByID           map[string][]int
	registryTraceSupplier func(time.Time) observability.EntityRegistryTrace
	plannerTraceSupplier  func() observability.PlannerTrace
}

func newCLITraceRecorder(writer *observability.Writer, turnIndex int, userMsg string, start time.Time) *cliTraceRecorder {
	userMsgHash, _ := observability.HashTracePayload(userMsg)
	return &cliTraceRecorder{
		writer: writer,
		record: observability.TraceRecord{
			TraceID:     fmt.Sprintf("trace-%d-%d", turnIndex, start.UnixNano()),
			TurnID:      fmt.Sprintf("turn-%d", turnIndex),
			TurnIndex:   turnIndex,
			UserMsgHash: userMsgHash,
		},
		start:       start,
		pendingByID: map[string][]int{},
	}
}

func (r *cliTraceRecorder) SetRegistryTraceSupplier(supplier func(time.Time) observability.EntityRegistryTrace) {
	if r == nil {
		return
	}
	r.registryTraceSupplier = supplier
}

func (r *cliTraceRecorder) SetPlannerTraceSupplier(supplier func() observability.PlannerTrace) {
	if r == nil {
		return
	}
	r.plannerTraceSupplier = supplier
}

func (r *cliTraceRecorder) SetPlannerTrace(trace observability.PlannerTrace) {
	if r == nil {
		return
	}
	r.record.Planner = trace
	r.plannerTraceSupplier = nil
}

func (r *cliTraceRecorder) SetEngineHardBlock(trace observability.EngineHardBlockTrace) {
	if r == nil {
		return
	}
	r.record.EngineHardBlock = trace
}

func (r *cliTraceRecorder) SetRateLimitDecision(decision governance.Decision) {
	if r == nil {
		return
	}
	// Decision.SubjectHash is expected to be pre-hashed by governance callers.
	// The recorder copies it verbatim and never accepts raw key material.
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
	// Aggregation rule from T-005 trace semantics:
	// first denial wins; if no denial occurs, record the latest allow.
	if !current.Allowed {
		return
	}
	if !trace.Allowed {
		r.record.RateLimit = trace
		return
	}
	r.record.RateLimit = trace
}

func (r *cliTraceRecorder) OnStep(ev engine.StepEvent) {
	if r == nil || r.writer == nil || ev.Action == "" {
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
		r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
			ID:        fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
			TurnIndex: r.record.TurnIndex,
			Action:    ev.Action,
			Source:    source,
			ArgsHash:  argsHash,
		})
		r.pendingByID[key] = append(r.pendingByID[key], len(r.record.ToolCalls)-1)
	case engine.StepToolResult:
		idx := r.matchPending(key, ev.Action, source)
		resultHash, _ := observability.HashTracePayload(ev.TraceResult)
		r.record.ToolCalls[idx].Status = observability.ToolStatusSuccess
		r.record.ToolCalls[idx].ResultHash = resultHash
		r.record.ToolCalls[idx].Attempts = ev.Attempts
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
	}
}

func (r *cliTraceRecorder) Finish(chatErr error, end time.Time) error {
	if r == nil || r.writer == nil {
		return nil
	}
	// TODO(T-006+): use chatErr when trace.v0.1 grows outcome.error_class.
	_ = chatErr
	if r.registryTraceSupplier != nil {
		r.record.EntityRegistry = r.registryTraceSupplier(end)
	}
	if r.plannerTraceSupplier != nil {
		r.record.Planner = r.plannerTraceSupplier()
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	for _, call := range r.record.ToolCalls {
		if call.TurnIndex == r.record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			r.record.Freshness.MonitorCallInCurrentTurn = true
			break
		}
	}
	return r.writer.Append(r.record)
}

func (r *cliTraceRecorder) matchPending(key, action, source string) int {
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
