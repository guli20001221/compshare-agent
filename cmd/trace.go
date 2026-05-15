package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
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

func cleanupTraceWriter(writer *observability.Writer, now time.Time) error {
	if writer == nil {
		return nil
	}
	return observability.Cleanup(writer.Dir(), observability.DefaultTraceRetentionDays, now)
}

func intentPlannerShadowEnabled(getenv getenvFunc) bool {
	return getenv("USE_INTENT_PLANNER") == "shadow"
}

func plannerRuntimeModeLine(shadowEnabled bool, cutoverIntents []intent.Intent) string {
	mode := "off"
	if shadowEnabled {
		mode = "shadow"
	}
	return fmt.Sprintf("planner_mode=%s cutover_intents=%s", mode, formatCutoverIntents(cutoverIntents))
}

func groundedRendererRuntimeLine(mode string) string {
	if mode == "" {
		mode = "off"
	}
	return fmt.Sprintf("grounded_renderer=%s", mode)
}

func mutatingToolsEnabledFromEnv(getenv getenvFunc) (bool, string) {
	value := strings.TrimSpace(getenv("COMPSHARE_ENABLE_MUTATING_TOOLS"))
	switch value {
	case "":
		return false, ""
	case "1":
		return true, ""
	default:
		return false, value
	}
}

func mutatingToolsRuntimeLine(enabled bool) string {
	if enabled {
		return "mutating=enabled"
	}
	return "mutating=disabled (read-only mode)"
}

func plannerRuntimeTrace(shadowEnabled bool, cutoverIntents []intent.Intent) observability.RuntimeTrace {
	mode := "off"
	if shadowEnabled {
		mode = "shadow"
	}
	return observability.RuntimeTrace{
		PlannerMode:    mode,
		CutoverIntents: cutoverIntentLabels(cutoverIntents),
	}
}

func formatCutoverIntents(cutoverIntents []intent.Intent) string {
	labels := cutoverIntentLabels(cutoverIntents)
	if len(labels) == 0 {
		return "[]"
	}
	return "[" + strings.Join(labels, ",") + "]"
}

func cutoverIntentLabels(cutoverIntents []intent.Intent) []string {
	if len(cutoverIntents) == 0 {
		return nil
	}
	labels := make([]string, 0, len(cutoverIntents))
	for _, enabled := range cutoverIntents {
		switch enabled {
		case intent.IntentResourceInfo:
			labels = append(labels, "resource")
		case intent.IntentMonitorQuery:
			labels = append(labels, "monitor")
		default:
			labels = append(labels, string(enabled))
		}
	}
	return labels
}

const defaultKnowledgeCorpusPath = "deploy/kb/stage2b_w0.jsonl"

func knowledgeRetrievalModeFromEnv(getenv getenvFunc) (bool, string) {
	raw := strings.ToLower(strings.TrimSpace(getenv("USE_KNOWLEDGE_RETRIEVAL")))
	switch raw {
	case "":
		return false, ""
	case "curated":
		return true, ""
	default:
		return false, raw
	}
}

func knowledgeCorpusPathFromEnv(getenv getenvFunc) string {
	path := strings.TrimSpace(getenv("COMPSHARE_KNOWLEDGE_CORPUS"))
	if path == "" {
		return defaultKnowledgeCorpusPath
	}
	return path
}

func knowledgeRetrieverFromEnv(getenv getenvFunc) (*knowledge.Retriever, bool, error) {
	enabled, unknown := knowledgeRetrievalModeFromEnv(getenv)
	if unknown != "" || !enabled {
		return nil, false, nil
	}
	corpus, err := knowledge.LoadPinnedCorpus(knowledgeCorpusPathFromEnv(getenv))
	if err != nil {
		return nil, false, err
	}
	return knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{}), true, nil
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
	totalTokens           int
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

func (r *cliTraceRecorder) SetRuntimeTrace(trace observability.RuntimeTrace) {
	if r == nil {
		return
	}
	r.record.Runtime = trace
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
	r.addPlannerTokens(trace)
	r.plannerTraceSupplier = nil
}

func (r *cliTraceRecorder) SetRetrievalTrace(trace observability.RetrievalTrace) {
	if r == nil {
		return
	}
	r.record.Retrieval = trace
}

func (r *cliTraceRecorder) SetOutcomeTrace(trace observability.OutcomeTrace) {
	if r == nil {
		return
	}
	r.record.Outcome.AttemptedHallucinatedCount = trace.AttemptedHallucinatedCount
	r.record.Outcome.EscapedHallucinatedCount = trace.EscapedHallucinatedCount
	r.record.Outcome.KBConflictCount = trace.KBConflictCount
}

func groundedRendererModeFromEnv(getenv getenvFunc) (string, string) {
	raw := strings.ToLower(strings.TrimSpace(getenv("USE_GROUNDED_RENDERER")))
	switch raw {
	case "":
		return "", ""
	case "llm":
		return "llm", ""
	default:
		return "", raw
	}
}

func (r *cliTraceRecorder) SetRendererTrace(trace observability.RendererTrace) {
	if r == nil {
		return
	}
	r.record.Renderer = trace
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

func (r *cliTraceRecorder) HasRateLimitDenial() bool {
	return r != nil && r.record.RateLimit.Checked && !r.record.RateLimit.Allowed
}

func (r *cliTraceRecorder) AddTokenUsage(usage llm.TokenUsage) {
	if r == nil {
		return
	}
	r.totalTokens += llmTokenUsageTotal(usage)
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

func (r *cliTraceRecorder) Finish(chatErr error, end time.Time) error {
	if r == nil || r.writer == nil {
		return nil
	}
	// TODO(T-006+): use chatErr when trace schema grows outcome.error_class.
	_ = chatErr
	if r.registryTraceSupplier != nil {
		r.record.EntityRegistry = r.registryTraceSupplier(end)
	}
	if r.plannerTraceSupplier != nil {
		r.record.Planner = r.plannerTraceSupplier()
		r.addPlannerTokens(r.record.Planner)
	}
	r.record.Outcome.TotalLatencyMS = end.Sub(r.start).Milliseconds()
	r.record.Outcome.TotalTokens = r.totalTokens
	for _, call := range r.record.ToolCalls {
		if call.TurnIndex == r.record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			r.record.Freshness.MonitorCallInCurrentTurn = true
			break
		}
	}
	return r.writer.Append(r.record)
}

func (r *cliTraceRecorder) addPlannerTokens(trace observability.PlannerTrace) {
	r.totalTokens += trace.InputTokens + trace.OutputTokens
}

func llmTokenUsageTotal(usage llm.TokenUsage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.PromptTokens + usage.CompletionTokens
}

func (r *cliTraceRecorder) applyCapFields(idx int, ev engine.StepEvent) {
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
