package turntrace

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

type traceEnqueuer interface {
	Enqueue(observability.TenantContext, observability.TraceRecord) error
}

// Config identifies one execution attempt. TraceID is attempt-scoped while
// TurnID remains the stable logical turn identifier shared by all retries.
type Config struct {
	Writer      observability.Writer
	Tenant      observability.TenantContext
	TraceID     string
	TurnID      string
	TurnIndex   int
	UserMessage string
	UserMsgHash string
	Start       time.Time
	Continuity  observability.ContinuityTrace
}

// Recorder accumulates every Engine observer into one redacted turn record.
// It is safe for streaming callbacks that arrive from different goroutines.
type Recorder struct {
	mu sync.Mutex

	writer observability.Writer
	tenant observability.TenantContext
	record observability.TraceRecord
	start  time.Time

	totalTokens      int
	promptTokens     int
	completionTokens int
	pendingByID      map[string][]int
	terminalSignals  observability.FinishSignals
	stateTrace       observability.StateTrace
	finished         bool
}

func New(cfg Config) *Recorder {
	if cfg.Writer == nil {
		return nil
	}
	if cfg.Start.IsZero() {
		cfg.Start = time.Now()
	}
	userHash := strings.TrimSpace(cfg.UserMsgHash)
	if userHash == "" && cfg.UserMessage != "" {
		userHash, _ = observability.HashTracePayload(cfg.UserMessage)
	}
	continuity := cfg.Continuity
	sanitizeContinuity(&continuity)
	return &Recorder{
		writer: cfg.Writer,
		tenant: cfg.Tenant,
		record: observability.TraceRecord{
			SchemaVersion: observability.SchemaVersion,
			TraceID:       cfg.TraceID,
			TurnID:        cfg.TurnID,
			TurnIndex:     cfg.TurnIndex,
			UserMsgHash:   userHash,
			Continuity:    continuity,
		},
		start:       cfg.Start,
		pendingByID: make(map[string][]int),
	}
}

func (r *Recorder) Hooks() engine.TraceHooks {
	if r == nil {
		return engine.TraceHooks{}
	}
	return engine.TraceHooks{
		Retrieval: r.SetRetrievalTrace, Freshness: r.SetFreshnessTrace,
		Diagnosis: r.SetDiagnosisTrace, Outcome: r.SetOutcomeTrace,
		Renderer: r.SetRendererTrace, HardBlock: r.SetEngineHardBlock,
		Completion: r.SetTurnCompletionTrace,
		RateLimit:  r.SetRateLimitDecision, TokenUsage: r.AddTokenUsage,
		Authorization: r.AddAuthorizationTrace,
		Confirmation:  r.AddConfirmationTrace,
	}
}

// AddAuthorizationTrace appends one write target's dual-proof audit record. The
// engine calls it once per verified target, so the records accumulate across a
// multi-target write.
func (r *Recorder) AddAuthorizationTrace(trace observability.AuthorizationTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Authorizations = append(r.record.Authorizations, trace)
}

// AddConfirmationTrace appends one closed-set terminal outcome for a human
// confirmation card. The engine deliberately omits card arguments and ids.
func (r *Recorder) AddConfirmationTrace(trace observability.ConfirmationTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Confirmations = append(r.record.Confirmations, trace)
}

func (r *Recorder) SetContinuity(update func(*observability.ContinuityTrace)) {
	if r == nil || update == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	update(&r.record.Continuity)
	sanitizeContinuity(&r.record.Continuity)
}

func (r *Recorder) SetUserMessage(message string) {
	if r == nil || message == "" {
		return
	}
	hash, err := observability.HashTracePayload(message)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.UserMsgHash = hash
}

func (r *Recorder) SetRetrievalTrace(trace observability.RetrievalTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Retrieval = observability.MergeRetrievalTrace(r.record.Retrieval, trace)
}

func (r *Recorder) SetFreshnessTrace(trace observability.FreshnessTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Freshness = observability.MergeFreshnessTrace(r.record.Freshness, trace)
}

func (r *Recorder) SetDiagnosisTrace(trace observability.DiagnosisTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Diagnosis = trace
}

func (r *Recorder) SetOutcomeTrace(trace observability.OutcomeTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Outcome.AttemptedHallucinatedCount = trace.AttemptedHallucinatedCount
	r.record.Outcome.EscapedHallucinatedCount = trace.EscapedHallucinatedCount
	r.record.Outcome.KBConflictCount = trace.KBConflictCount
}

func (r *Recorder) SetRendererTrace(trace observability.RendererTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Renderer = trace
}

func (r *Recorder) SetEngineHardBlock(trace observability.EngineHardBlockTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.EngineHardBlock = trace
}

func (r *Recorder) SetTurnCompletionTrace(trace observability.TurnCompletionTrace) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Completion = trace
}

func (r *Recorder) SetRateLimitDecision(decision governance.Decision) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	trace := observability.RateLimitTrace{
		Checked: true, Allowed: decision.Allowed, Class: string(decision.Class),
		Action: decision.Action, Reason: string(decision.Reason), SubjectHash: decision.SubjectHash,
		RetryAfterMS: decision.RetryAfter.Milliseconds(),
	}
	current := r.record.RateLimit
	if !current.Checked || (current.Allowed && !trace.Allowed) || (current.Allowed && trace.Allowed) {
		r.record.RateLimit = trace
	}
}

func (r *Recorder) AddTokenUsage(usage llm.TokenUsage) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	total := usage.TotalTokens
	if total <= 0 {
		total = usage.PromptTokens + usage.CompletionTokens
	}
	r.totalTokens += total
	r.promptTokens += usage.PromptTokens
	r.completionTokens += usage.CompletionTokens
}

func (r *Recorder) OnStep(ev engine.StepEvent) {
	if r == nil || ev.Action == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	source := ev.Source
	if source == "" {
		source = observability.ToolSourceMainReAct
	}
	key := source + "\x00" + ev.Action
	switch ev.Type {
	case engine.StepToolCall:
		argsHash, _ := observability.HashTracePayload(ev.Args)
		requested := ev.RequestedTargets
		if requested == 0 {
			requested = requestedTargets(ev.Args)
		}
		window := ev.WindowSeconds
		if window == 0 {
			window = windowSeconds(ev.Args)
		}
		r.record.ToolCalls = append(r.record.ToolCalls, observability.ToolCallTrace{
			ID: fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1), TurnIndex: r.record.TurnIndex,
			Action: ev.Action, Source: source, ArgsHash: argsHash,
			RequestedTargets: requested, WindowSeconds: window,
		})
		r.pendingByID[key] = append(r.pendingByID[key], len(r.record.ToolCalls)-1)
	case engine.StepToolResult:
		idx := r.matchPending(key, ev.Action, source)
		resultHash, _ := observability.HashTracePayload(ev.TraceResult)
		call := &r.record.ToolCalls[idx]
		call.Status, call.ResultHash, call.Attempts = observability.ToolStatusSuccess, resultHash, ev.Attempts
		call.Projected = ev.Projected
		if call.RequestedTargets > 0 && call.ExecutedTargets == 0 {
			call.ExecutedTargets = call.RequestedTargets
		}
		if len(ev.RendererInputToolArgHashes) > 0 {
			r.record.Renderer.InputToolArgHashes = append(r.record.Renderer.InputToolArgHashes, ev.RendererInputToolArgHashes...)
		}
	case engine.StepError, engine.StepBlocked:
		idx := r.matchPending(key, ev.Action, source)
		call := &r.record.ToolCalls[idx]
		call.Status = observability.ToolStatusError
		if ev.Type == engine.StepBlocked {
			call.ErrorClass = "blocked"
		} else {
			call.ErrorClass = "tool_error"
		}
		if ev.Args != nil && call.ArgsHash == "" {
			call.ArgsHash, _ = observability.HashTracePayload(ev.Args)
		}
		if call.RequestedTargets == 0 {
			call.RequestedTargets = requestedTargets(ev.Args)
		}
		if call.WindowSeconds == 0 {
			call.WindowSeconds = windowSeconds(ev.Args)
		}
		call.ExecutedTargets = 0
		if ev.Capped != "" {
			call.Capped = ev.Capped
		}
		if ev.CapReason != "" {
			call.CapReason = ev.CapReason
		}
	}
}

// EmitStep implements orchestrator.StepSink. Saga steps are folded into this
// attempt's single row and are never inserted independently.
func (r *Recorder) EmitStep(step observability.StepTrace) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.Steps = append(r.record.Steps, step)
	return nil
}

func (r *Recorder) Finish(chatErr, attemptErr error, reply string, snapshot engine.TraceSnapshot, end time.Time) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return nil
	}
	r.finished = true
	if end.IsZero() {
		end = time.Now()
	}
	r.record.Timestamp = end.UTC().Format(time.RFC3339Nano)
	r.record.EntityRegistry = snapshot.Registry
	if chatErr != nil && r.record.EngineHardBlock.Category == "" {
		r.record.EngineHardBlock = observability.EngineHardBlockTrace{Hit: true, Category: observability.HardBlockCategoryChatError}
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	r.record.Outcome.TotalTokens = r.totalTokens
	r.record.Outcome.PromptTokens = r.promptTokens
	r.record.Outcome.CompletionTokens = r.completionTokens
	r.record.Outcome.ContextSources = append([]string(nil), snapshot.ContextSources...)
	r.record.Outcome.ResponseContract = snapshot.ResponseContract
	r.record.Outcome.PromptSectionIDs = append([]string(nil), snapshot.PromptSectionIDs...)
	r.record.Outcome.MemoryUpdateSource = snapshot.MemoryUpdateSource
	r.record.Outcome.GroundingOutcome = snapshot.GroundingOutcome
	if chatErr != nil || attemptErr != nil {
		r.record.Outcome.ResponseContract = "failure"
	}
	for _, call := range r.record.ToolCalls {
		if call.TurnIndex == r.record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			r.record.Freshness.MonitorCallInCurrentTurn = true
			break
		}
	}
	r.stateTrace.SessionStateHydrated = snapshot.SessionStateHydrated
	r.stateTrace.ResolutionSource = snapshot.ResolutionSource
	r.stateTrace.SelectedInstanceID = snapshot.SessionState.SelectedInstanceID
	r.stateTrace.SelectedInstanceIDAtTurnStart = snapshot.SelectedInstanceIDAtStart
	r.stateTrace.FactCacheOldestAgeBucket = observability.BucketFactCacheAge(snapshot.FactCacheOldestAgeSeconds)
	r.record.State = r.stateTrace
	r.record.ActualExecutionTier = r.record.DeriveActualExecutionTier()
	r.record.ActualExecutionPath = r.record.DeriveActualExecutionPath()
	r.record.Retrieval.RefusalType = r.record.Retrieval.DeriveRefusalType()
	r.terminalSignals.ReplyEmpty = strings.TrimSpace(reply) == ""
	r.terminalSignals.ReactRounds = snapshot.ReactRounds
	r.terminalSignals.RoundCeilingHit = snapshot.RoundCeilingHit
	r.terminalSignals.ActionProposalDisposition = snapshot.ActionProposalDisposition
	r.terminalSignals.ChatErr = attemptErr
	if r.terminalSignals.ChatErr == nil {
		r.terminalSignals.ChatErr = chatErr
	}
	r.record.FinalizeOutcome(r.terminalSignals)
	record := r.record
	writer, tenant := r.writer, r.tenant
	r.mu.Unlock()
	if enqueuer, ok := writer.(traceEnqueuer); ok {
		return enqueuer.Enqueue(tenant, record)
	}
	return writer.Append(record)
}

func (r *Recorder) matchPending(key, action, source string) int {
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
		ID: fmt.Sprintf("tool-%d", len(r.record.ToolCalls)+1), TurnIndex: r.record.TurnIndex,
		Action: action, Source: source,
	})
	return len(r.record.ToolCalls) - 1
}

func requestedTargets(args map[string]any) int {
	if args == nil {
		return 0
	}
	switch value := args["UHostIds"].(type) {
	case []string:
		if len(value) > 0 {
			return len(value)
		}
	case []any:
		if len(value) > 0 {
			return len(value)
		}
	}
	if value, ok := args["UHostId"].(string); ok && strings.TrimSpace(value) != "" {
		return 1
	}
	return 0
}

func windowSeconds(args map[string]any) int {
	if args == nil {
		return 0
	}
	start, okStart := int64Value(args["StartTime"])
	end, okEnd := int64Value(args["EndTime"])
	if !okStart || !okEnd || start < 0 || end <= start {
		return 0
	}
	return int(end - start)
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed < math.MinInt64 || typed > math.MaxInt64 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case float32:
		f := float64(typed)
		if math.IsNaN(f) || math.IsInf(f, 0) || f < math.MinInt64 || f > math.MaxInt64 || f != float64(int64(f)) {
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

func sanitizeContinuity(trace *observability.ContinuityTrace) {
	if trace == nil {
		return
	}
	trace.EnvelopeParseOutcome = closedValue(trace.EnvelopeParseOutcome,
		"not_read", "valid", "execution_envelope_missing", "execution_envelope_unsupported", "execution_envelope_invalid")
	trace.ContextParseOutcome = closedValue(trace.ContextParseOutcome,
		"not_read", "valid", "unknown_schema", "malformed")
	trace.CommitOutcome = closedValue(trace.CommitOutcome,
		"committed", "reconciled_committed", "late_reconciled_committed", "failed_retryable", "failed_final",
		"ambiguous_after_action", "aborted", "settlement_interrupted")
	trace.CommitReason = closedValue(trace.CommitReason,
		"executor_stopped", "turn_reload_failed", "execution_envelope_missing",
		"execution_envelope_unsupported", "execution_envelope_invalid",
		"event_persist_failed", "context_read_failed", "engine_build_failed",
		"trace_engine_unsupported", "action_recovery_read_failed",
		"continuity_read_failed", "execution_failed", "lease_lost",
		"interaction_expired", "action_outcome_uncertain", "action_replay_incomplete",
		"context_snapshot_invalid", "context_encode_failed", "empty_answer", "turn_not_saved")
}

func closedValue(value string, allowed ...string) string {
	if value == "" {
		return ""
	}
	for _, item := range allowed {
		if value == item {
			return value
		}
	}
	return "other"
}
