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
	"github.com/compshare-agent/internal/tools"
)

type traceEnqueuer interface {
	Enqueue(observability.TenantContext, observability.TraceRecord) error
}

type chatTraceRecorder struct {
	writer              observability.Writer
	tenant              observability.TenantContext
	record              observability.TraceRecord
	start               time.Time
	totalTokens         int
	promptTokens        int
	completionTokens    int
	pendingByID         map[string][]pendingToolCall
	now                 func() time.Time
	terminalSignals     observability.FinishSignals
	stateTrace          observability.StateTrace
	firstVisibleEventAt time.Time
}

// pendingToolCall ties an observed completion to the exact call that began it.
// A source/action pair can repeat in one turn, so this remains a FIFO queue
// rather than a map directly to one tool index.
type pendingToolCall struct {
	index     int
	startedAt time.Time
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
		pendingByID: map[string][]pendingToolCall{},
		now:         time.Now,
	}
}

func attachChatTraceObservers(agent *engine.Engine, recorder *chatTraceRecorder) {
	if agent == nil || recorder == nil {
		return
	}
	agent.SetRetrievalTraceObserver(recorder.SetRetrievalTrace)
	agent.SetHardBlockObserver(recorder.SetEngineHardBlock)
	agent.SetTurnCompletionObserver(recorder.SetTurnCompletionTrace)
	agent.SetRateLimitObserver(recorder.SetRateLimitDecision)
	agent.SetTokenUsageObserver(recorder.AddTokenUsage)
	agent.SetAuthorizationTraceObserver(recorder.AddAuthorizationTrace)
	agent.SetConfirmationTraceObserver(recorder.AddConfirmationTrace)
}

func clearChatTraceObservers(agent *engine.Engine) {
	if agent == nil {
		return
	}
	agent.SetRetrievalTraceObserver(nil)
	agent.SetHardBlockObserver(nil)
	agent.SetTurnCompletionObserver(nil)
	agent.SetRateLimitObserver(nil)
	agent.SetTokenUsageObserver(nil)
	agent.SetAuthorizationTraceObserver(nil)
	agent.SetConfirmationTraceObserver(nil)
}

// SetEngineSnapshot is the single turn-end projection from Engine state into
// the root trace. replyEmpty is transport-owned; every other value comes from
// the same immutable engine snapshot so independently-read facts cannot drift.
//
// The snapshot deliberately contains identifiers and closed-set outcomes only;
// it does not add a prompt, reply, tool arguments, or transcript to trace
// storage.
func (r *chatTraceRecorder) SetEngineSnapshot(snapshot engine.TraceSnapshot, replyEmpty bool) {
	if r == nil {
		return
	}
	r.record.EntityRegistry = snapshot.Registry
	r.terminalSignals = observability.FinishSignals{
		ReplyEmpty:                replyEmpty,
		ReactRounds:               snapshot.ReactRounds,
		RoundCeilingHit:           snapshot.RoundCeilingHit,
		ActionProposalDisposition: snapshot.ActionProposalDisposition,
	}
	r.stateTrace = observability.StateTrace{
		SessionStateHydrated:                 snapshot.SessionStateHydrated,
		ResolutionSource:                     snapshot.ResolutionSource,
		SelectedInstanceID:                   snapshot.SessionState.SelectedInstanceID,
		SelectedInstanceIDAtTurnStart:        snapshot.SelectedInstanceIDAtStart,
		SelectedInstanceSource:               snapshot.SessionState.SelectedInstanceSource,
		SelectedInstanceFreshness:            snapshot.SessionState.SelectedInstanceFreshness,
		SelectedInstanceSourceAtTurnStart:    snapshot.SelectedInstanceSourceAtStart,
		SelectedInstanceFreshnessAtTurnStart: snapshot.SelectedInstanceFreshnessAtStart,
	}
	r.record.Outcome.ContextSources = append([]string(nil), snapshot.ContextSources...)
	r.record.Outcome.ResponseContract = snapshot.ResponseContract
	r.record.Outcome.PromptSectionIDs = append([]string(nil), snapshot.PromptSectionIDs...)
	r.record.Outcome.EvidenceUpdateSource = snapshot.EvidenceUpdateSource
	r.record.Outcome.GroundingOutcome = snapshot.GroundingOutcome
	r.record.Outcome.GroundingCitationScope = snapshot.GroundingCitationScope
	r.record.Outcome.PromptMessagesRawPeak = snapshot.PromptMessagesRawPeak
	r.record.Outcome.PromptMessagesAssembledPeak = snapshot.PromptMessagesAssembledPeak
	r.record.Outcome.PromptMessagesCapApplied = snapshot.PromptMessagesCapApplied
}

// ObserveFirstVisibleEvent records the first token/step/confirmation/error that
// the transport actually accepted. Calling it before WriteEvent would turn a
// failed write into a false user-visible latency sample.
func (r *chatTraceRecorder) ObserveFirstVisibleEvent(at time.Time) {
	if r == nil || !r.firstVisibleEventAt.IsZero() || at.Before(r.start) {
		return
	}
	r.firstVisibleEventAt = at
}

func (r *chatTraceRecorder) SetRetrievalTrace(trace observability.RetrievalTrace) {
	if r == nil {
		return
	}
	r.record.Retrieval = observability.MergeRetrievalTrace(r.record.Retrieval, trace)
}

// AddAuthorizationTrace appends one write target's dual-proof audit record; the
// engine calls it once per verified target of a mutating action.
func (r *chatTraceRecorder) AddAuthorizationTrace(trace observability.AuthorizationTrace) {
	if r == nil {
		return
	}
	r.record.Authorizations = append(r.record.Authorizations, trace)
}

// AddConfirmationTrace appends one terminal confirmation outcome. The payload
// is already bounded by engine.recordConfirmationResult and contains no full
// form, broker ids or arbitrary arguments; the only argument-derived data is the
// fixed-field contract from an approved final create card.
func (r *chatTraceRecorder) AddConfirmationTrace(trace observability.ConfirmationTrace) {
	if r == nil {
		return
	}
	r.record.Confirmations = append(r.record.Confirmations, trace)
}

func (r *chatTraceRecorder) SetEngineHardBlock(trace observability.EngineHardBlockTrace) {
	if r == nil {
		return
	}
	r.record.EngineHardBlock = trace
}

func (r *chatTraceRecorder) SetTurnCompletionTrace(trace observability.TurnCompletionTrace) {
	if r == nil {
		return
	}
	r.record.Completion = trace
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
	r.promptTokens += usage.PromptTokens
	r.completionTokens += usage.CompletionTokens
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
			ID:                   fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
			Action:               ev.Action,
			SelectedFunctionName: ev.SelectedFunctionName,
			Source:               source,
			ArgsHash:             argsHash,
			RequestedTargets:     requestedTargets,
			WindowSeconds:        windowSeconds,
		})
		r.pendingByID[key] = append(r.pendingByID[key], pendingToolCall{
			index:     len(r.record.ToolCalls) - 1,
			startedAt: r.clockNow(),
		})
	case engine.StepToolResult:
		idx, startedAt := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].AgentUsage = ev.AgentUsage
		r.applySelectedFunctionName(idx, ev.SelectedFunctionName)
		resultHash, _ := observability.HashTracePayload(ev.TraceResult)
		r.record.ToolCalls[idx].Status = observability.ToolStatusSuccess
		r.applyLatency(idx, startedAt)
		r.record.ToolCalls[idx].ResultHash = resultHash
		r.record.ToolCalls[idx].Attempts = ev.Attempts
		r.record.ToolCalls[idx].Projected = ev.Projected
		r.applyToolResultFormat(idx, ev)
		r.applyErrorCode(idx, ev.ErrorCode)
		if r.record.ToolCalls[idx].RequestedTargets > 0 && r.record.ToolCalls[idx].ExecutedTargets == 0 {
			r.record.ToolCalls[idx].ExecutedTargets = r.record.ToolCalls[idx].RequestedTargets
		}
	case engine.StepError:
		idx, startedAt := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].AgentUsage = ev.AgentUsage
		r.applySelectedFunctionName(idx, ev.SelectedFunctionName)
		r.record.ToolCalls[idx].Status = observability.ToolStatusError
		// Step messages are user-facing diagnostics and may contain upstream
		// detail. Trace records only the closed-set error class.
		r.record.ToolCalls[idx].ErrorClass = "tool_error"
		r.applyErrorCode(idx, ev.ErrorCode)
		r.applyLatency(idx, startedAt)
		r.applyCapFields(idx, ev)
	case engine.StepBlocked:
		idx, startedAt := r.matchPending(key, ev.Action, source)
		r.record.ToolCalls[idx].AgentUsage = ev.AgentUsage
		r.applySelectedFunctionName(idx, ev.SelectedFunctionName)
		r.record.ToolCalls[idx].Status = observability.ToolStatusError
		r.record.ToolCalls[idx].ErrorClass = "blocked"
		r.applyErrorCode(idx, ev.ErrorCode)
		r.applyLatency(idx, startedAt)
		r.applyCapFields(idx, ev)
	case engine.StepUserNotice:
		// Deliberately recorded as nothing. A user notice is not a tool event: no call was made,
		// none failed, and none was blocked, so matchPending would synthesize a ToolCallTrace for
		// a tool that never ran and stamp it error/blocked. That is not a cosmetic difference —
		// tool error counts are what an incident is triaged from, and a turn that merely REPORTS a
		// previous turn's interruption would show up as a turn where a tool was refused.
		//
		// This case is explicit rather than left to the switch having no branch, so that adding a
		// `default:` here later cannot quietly start recording notices.
		return
	}
}

func (r *chatTraceRecorder) applySelectedFunctionName(idx int, name string) {
	if r == nil || name == "" || idx < 0 || idx >= len(r.record.ToolCalls) {
		return
	}
	if r.record.ToolCalls[idx].SelectedFunctionName == "" {
		r.record.ToolCalls[idx].SelectedFunctionName = name
	}
}

func (r *chatTraceRecorder) Finish(chatErr error, end time.Time) error {
	if r == nil || r.writer == nil {
		return nil
	}
	if r.record.Timestamp == "" {
		r.record.Timestamp = end.UTC().Format(time.RFC3339Nano)
	}
	if chatErr != nil && r.record.EngineHardBlock.Category == "" {
		r.record.EngineHardBlock = observability.EngineHardBlockTrace{
			Hit:      true,
			Category: observability.HardBlockCategoryChatError,
		}
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	if !r.firstVisibleEventAt.IsZero() {
		firstVisibleMS := r.firstVisibleEventAt.Sub(r.start).Milliseconds()
		r.record.Outcome.FirstVisibleEventMS = &firstVisibleMS
	}
	r.record.Outcome.TotalTokens = r.totalTokens
	r.record.Outcome.PromptTokens = r.promptTokens
	r.record.Outcome.CompletionTokens = r.completionTokens
	if chatErr != nil {
		// A failed turn did not deliver the normal Agent response contract.
		r.record.Outcome.ResponseContract = "failure"
	}
	r.record.ActualExecutionTier = r.record.DeriveActualExecutionTier()
	r.record.Retrieval.RefusalType = r.record.Retrieval.DeriveRefusalType()
	r.record.State = r.stateTrace
	signals := r.terminalSignals
	signals.ChatErr = chatErr
	r.record.FinalizeOutcome(signals)
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

func (r *chatTraceRecorder) applyErrorCode(idx int, code string) {
	if r == nil || idx < 0 || idx >= len(r.record.ToolCalls) || code == "" {
		return
	}
	code = tools.TraceAgentToolErrorCode(code)
	if code == "" {
		return
	}
	r.record.ToolCalls[idx].ErrorCode = code
	r.record.ToolCalls[idx].Status = observability.ToolStatusError
	if r.record.ToolCalls[idx].ErrorClass == "" {
		r.record.ToolCalls[idx].ErrorClass = "tool_error"
	}
}

func (r *chatTraceRecorder) applyToolResultFormat(idx int, ev engine.StepEvent) {
	if r == nil || idx < 0 || idx >= len(r.record.ToolCalls) {
		return
	}
	r.record.ToolCalls[idx].ToolResultRawRunes = ev.ToolResultRawRunes
	r.record.ToolCalls[idx].ToolResultVisibleRunes = ev.ToolResultVisibleRunes
	r.record.ToolCalls[idx].ToolResultTruncated = ev.ToolResultTruncated
}

func (r *chatTraceRecorder) matchPending(key, action, source string) (int, time.Time) {
	if queue := r.pendingByID[key]; len(queue) > 0 {
		pending := queue[0]
		if len(queue) == 1 {
			delete(r.pendingByID, key)
		} else {
			r.pendingByID[key] = queue[1:]
		}
		return pending.index, pending.startedAt
	}
	r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
		ID:     fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1),
		Action: action,
		Source: source,
	})
	return len(r.record.ToolCalls) - 1, time.Time{}
}

func (r *chatTraceRecorder) clockNow() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *chatTraceRecorder) applyLatency(idx int, startedAt time.Time) {
	if r == nil || startedAt.IsZero() || idx < 0 || idx >= len(r.record.ToolCalls) {
		return
	}
	elapsed := r.clockNow().Sub(startedAt)
	if elapsed < 0 {
		return
	}
	latencyMS := elapsed.Milliseconds()
	r.record.ToolCalls[idx].LatencyMS = &latencyMS
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
