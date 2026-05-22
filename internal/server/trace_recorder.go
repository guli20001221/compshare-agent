package server

import (
	"fmt"
	"log"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

// serverTraceRecorder buffers observer callbacks from a single Chat turn
// into a TraceRecord and writes it to the configured sink at turn end.
// One recorder per turn — engine observers are attached in attach() and
// detached after finish().
//
// The CLI counterpart is cmd.newCLITraceRecorder (cmd/trace.go). We
// duplicate the recorder rather than import it because:
//   - the CLI package is in cmd/ and can't be imported from internal/
//   - the server's recorder owns extra fields (tenant + connection_id)
//     that the file-only CLI path doesn't need.
//
// trace_id passthrough (plan §10 / A7): the recorder pins
// TraceRecord.TraceID = ClientMessage.RequestUUID so the row's inner
// trace_id column matches the request_uuid column exactly. Dashboard
// joins on either column return the same record.
type serverTraceRecorder struct {
	sink         observability.Writer
	tenant       TenantCtx
	connectionID string
	turnIndex    int
	requestUUID  string
	model        string
	start        time.Time

	record observability.TraceRecord
	// totalTokens accumulates LLM tokens from BOTH the regular Chat
	// token-usage observer AND the planner trace observer. Mirrors the
	// CLI cliTraceRecorder.totalTokens accounting (cmd/trace.go); pre-fix
	// the server path only added prompt+completion from emitTokenUsage
	// and missed planner tokens entirely, so MySQL's
	// agent_traces.total_tokens under-counted planner-handled turns.
	totalTokens int
}

// newServerTraceRecorder constructs a recorder if a sink is configured;
// returns nil when sink == nil so the call site can skip observer wiring
// cheaply. Caller MUST invoke attach + finish.
//
// trace_id passthrough (plan §10 / A7): the recorder pins
// TraceRecord.TraceID = ClientMessage.RequestUUID so the trace row's
// inner trace_id matches the row's request_uuid column exactly. Dashboard
// joins on either column return the same record. When RequestUUID is
// empty (legacy clients pre-A7), the recorder falls back to an auto-
// generated trace-{turn}-{ns} form so trace rows still get a unique key.
func newServerTraceRecorder(
	sink observability.Writer,
	tenant TenantCtx,
	connectionID string,
	turnIndex int,
	msg ClientMessage,
	model string,
) *serverTraceRecorder {
	if sink == nil {
		return nil
	}
	now := time.Now().UTC()
	traceID := msg.RequestUUID
	if traceID == "" {
		traceID = fmt.Sprintf("trace-%d-%d", turnIndex, now.UnixNano())
	}
	return &serverTraceRecorder{
		sink:         sink,
		tenant:       tenant,
		connectionID: connectionID,
		turnIndex:    turnIndex,
		requestUUID:  msg.RequestUUID,
		model:        model,
		start:        now,
		record: observability.TraceRecord{
			SchemaVersion: observability.SchemaVersion,
			TraceID:       traceID,
			TurnID:        fmt.Sprintf("turn-%d", turnIndex),
			TurnIndex:     turnIndex,
			Timestamp:     now.Format(time.RFC3339Nano),
		},
	}
}

// attach wires the engine observers to record callback streams into the
// in-progress TraceRecord. The Engine resets observers each turn (per the
// CLI pattern in cmd/agent.go) so we don't need to detach explicitly —
// finish() simply stops mutating the record after writing it out.
func (r *serverTraceRecorder) attach(sess *engine.Engine) {
	sess.SetPlannerTraceObserver(func(t observability.PlannerTrace) {
		r.record.Planner = t
		// Planner LLM tokens count toward the per-turn total just like
		// every other LLM call. Without this, agent_traces.total_tokens
		// on planner-handled turns underreports by the planner cost —
		// engine.turnTokensConsumed (the budget enforcer) already counts
		// planner usage via accumulateTokenUsage in callPlannerOnce, so
		// the trace was diverging from the gate.
		r.totalTokens += t.InputTokens + t.OutputTokens
		r.record.Outcome.TotalTokens = r.totalTokens
	})
	sess.SetRetrievalTraceObserver(func(t observability.RetrievalTrace) { r.record.Retrieval = t })
	sess.SetRendererTraceObserver(func(t observability.RendererTrace) { r.record.Renderer = t })
	sess.SetOutcomeTraceObserver(func(t observability.OutcomeTrace) {
		// Outcome observer fires AFTER token accumulation in some paths
		// (e.g. tryStage2BRetrieval emits OutcomeTrace with its own
		// counters). Merge fields without clobbering totalTokens, which
		// is owned by this recorder via the planner + token-usage
		// observers below.
		t.TotalTokens = r.totalTokens
		r.record.Outcome = t
	})
	sess.SetHardBlockObserver(func(t observability.EngineHardBlockTrace) { r.record.EngineHardBlock = t })
	sess.SetRateLimitObserver(func(d governance.Decision) {
		r.record.RateLimit = observability.RateLimitTrace{
			Checked:      true,
			Allowed:      d.Allowed,
			Class:        string(d.Class),
			Action:       d.Action,
			Reason:       string(d.Reason),
			SubjectHash:  d.SubjectHash,
			RetryAfterMS: d.RetryAfter.Milliseconds(),
		}
	})
	sess.SetTokenUsageObserver(func(u llm.TokenUsage) {
		// Use the same helper as CLI so Usage.TotalTokens-only responses
		// (some providers omit prompt/completion split) still count.
		r.totalTokens += llmTokenUsageTotal(u)
		r.record.Outcome.TotalTokens = r.totalTokens
	})
}

// llmTokenUsageTotal mirrors the CLI helper of the same name
// (cmd/trace.go). Returns Usage.TotalTokens when set; otherwise the sum
// of PromptTokens + CompletionTokens. Some providers (ds-v4-flash in
// streaming mode) only report TotalTokens; falling back to the sum
// covers the other shape.
func llmTokenUsageTotal(usage llm.TokenUsage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.PromptTokens + usage.CompletionTokens
}

// OnStep records tool-call events into the trace's ToolCalls slice. Each
// StepToolCall begins a tool entry; the matching StepToolResult /
// StepError / StepBlocked closes it. We track by Action name + index —
// adequate for the single-tool-call-at-a-time engine pattern; multi-tool
// concurrency would need a per-call ID (engine.StepEvent doesn't carry
// one today so we live with this scoping).
func (r *serverTraceRecorder) OnStep(ev engine.StepEvent) {
	switch ev.Type {
	case engine.StepToolCall:
		r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
			Action: ev.Action,
			Status: observability.ToolStatusSuccess, // optimistic; downgraded below
			Source: observability.ToolSourceMainReAct,
		})
	case engine.StepToolResult:
		r.markLastTool(ev.Action, observability.ToolStatusSuccess, "")
	case engine.StepBlocked:
		r.markLastTool(ev.Action, observability.ToolStatusError, ev.Message)
	case engine.StepError:
		r.markLastTool(ev.Action, observability.ToolStatusError, ev.Message)
	}
}

func (r *serverTraceRecorder) markLastTool(action, status, errMsg string) {
	for i := len(r.record.ToolCalls) - 1; i >= 0; i-- {
		if r.record.ToolCalls[i].Action == action {
			r.record.ToolCalls[i].Status = status
			if errMsg != "" {
				// ToolCallTrace exposes ErrorClass + CapReason but no free-form
				// error text — surface the message as the ErrorClass tag.
				r.record.ToolCalls[i].ErrorClass = errMsg
			}
			return
		}
	}
}

// finish stamps end-of-turn fields and writes the trace to the sink.
// chatErr is folded into the trace via the Outcome.EscapedHallucinatedCount
// channel only — the structured "blocked / error / success" decision lives
// in DeriveStatus and is consumed by MessageRecorder; the trace row itself
// does not carry a top-level status column (it sits inside trace_json).
//
// For MySQLWriter, the sink Enqueue path expects tenant context; we cast
// through that helper when available so the row carries the tenant
// columns. Other writers receive a plain Append.
// finish returns the finalized TraceRecord so the caller can feed it to
// DeriveStatus + MessageRecorder. Even when the sink is nil we still
// return the record (the recorder was constructed by the caller's
// choice, so they probably want the record to derive status from).
func (r *serverTraceRecorder) finish(chatErr error) observability.TraceRecord {
	r.record.Outcome.TotalLatencyMS = time.Since(r.start).Milliseconds()
	if chatErr != nil {
		// Surface engine failure as a hard-block trace entry so dashboards
		// querying engine_hard_block.hit see the row. Category is best-effort.
		if r.record.EngineHardBlock.Category == "" {
			r.record.EngineHardBlock = observability.EngineHardBlockTrace{
				Hit:      true,
				Category: "chat_error",
			}
		}
	}

	// Tenant-aware write for MySQL; plain Append for file/multi.
	if mw, ok := r.sink.(*observability.MySQLWriter); ok {
		_ = mw.Enqueue(observability.TenantContext{
			TopOrgID:     r.tenant.TopOrgID,
			OrgID:        r.tenant.OrgID,
			ConnectionID: r.connectionID,
		}, r.record)
		return r.record
	}
	if err := r.sink.Append(r.record); err != nil {
		log.Printf("trace sink append failed: %v", err)
	}
	return r.record
}
