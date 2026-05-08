package main

import (
	"fmt"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
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

type cliTraceRecorder struct {
	writer                *observability.Writer
	record                observability.TraceRecord
	start                 time.Time
	pendingByID           map[string][]int
	registryTraceSupplier func(time.Time) observability.EntityRegistryTrace
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

func (r *cliTraceRecorder) SetRateLimitDecision(decision governance.Decision) {
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
