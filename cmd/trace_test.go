package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/require"
)

func TestTraceWriterFromEnvDisabledByDefault(t *testing.T) {
	writer, enabled, err := traceWriterFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("traceWriterFromEnv: %v", err)
	}
	if enabled {
		t.Fatal("trace should be disabled by default")
	}
	if writer != nil {
		t.Fatalf("writer = %#v, want nil when disabled", writer)
	}
}

func TestTraceWriterFromEnvEnabled(t *testing.T) {
	traceDir := t.TempDir()
	writer, enabled, err := traceWriterFromEnv(func(key string) string {
		switch key {
		case "COMPSHARE_TRACE_ENABLED":
			return "1"
		case "COMPSHARE_TRACE_DIR":
			return traceDir
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("traceWriterFromEnv: %v", err)
	}
	if !enabled {
		t.Fatal("trace should be enabled when COMPSHARE_TRACE_ENABLED=1")
	}
	if writer == nil || writer.Dir() != traceDir {
		t.Fatalf("writer dir = %#v, want %q", writer, traceDir)
	}
}

func TestCleanupTraceWriterDeletesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	writer, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-trace-2026-04-07.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-trace-2026-04-08.jsonl"), []byte("{}\n"), 0o600))

	err = cleanupTraceWriter(writer, time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, "agent-trace-2026-04-07.jsonl"))
	require.FileExists(t, filepath.Join(dir, "agent-trace-2026-04-08.jsonl"))
}

func TestIntentPlannerShadowModeFromEnv(t *testing.T) {
	if intentPlannerShadowEnabled(func(string) string { return "" }) {
		t.Fatal("unset USE_INTENT_PLANNER must not enable shadow planner")
	}
	if !intentPlannerShadowEnabled(func(key string) string {
		if key == "USE_INTENT_PLANNER" {
			return "shadow"
		}
		return ""
	}) {
		t.Fatal("USE_INTENT_PLANNER=shadow should enable shadow planner")
	}
	if intentPlannerShadowEnabled(func(key string) string {
		if key == "USE_INTENT_PLANNER" {
			return "auto"
		}
		return ""
	}) {
		t.Fatal("only explicit shadow mode should enable shadow planner")
	}
}

func TestIntentPlannerCutoverIntentsFromEnv(t *testing.T) {
	intents, unknown := intentPlannerCutoverIntentsFromEnv(func(key string) string {
		if key == "USE_INTENT_PLANNER_FOR" {
			return "resource, monitor, billing, ,RESOURCE"
		}
		return ""
	})
	if len(unknown) != 1 || unknown[0] != "billing" {
		t.Fatalf("unknown values = %#v, want billing", unknown)
	}
	if len(intents) != 2 {
		t.Fatalf("enabled intents = %#v, want resource and monitor", intents)
	}
	if intents[0] != "resource_info" || intents[1] != "monitor_query" {
		t.Fatalf("enabled intents = %#v", intents)
	}
}

func TestSeparateShadowRunnerDisabledWhenCutoverEnabled(t *testing.T) {
	if !useSeparateShadowRunner(true, true, false) {
		t.Fatal("shadow-only tracing should use the existing shadow runner")
	}
	if useSeparateShadowRunner(true, true, true) {
		t.Fatal("shadow + cutover must not create a second planner runner")
	}
	if useSeparateShadowRunner(false, true, false) {
		t.Fatal("trace disabled must not create a shadow runner")
	}
	if useSeparateShadowRunner(true, false, false) {
		t.Fatal("shadow disabled must not create a shadow runner")
	}
}

func TestPlannerRuntimeModeLine(t *testing.T) {
	cutoverIntents, unknown := intentPlannerCutoverIntentsFromEnv(func(key string) string {
		if key == "USE_INTENT_PLANNER_FOR" {
			return "resource,monitor"
		}
		return ""
	})
	require.Empty(t, unknown)

	line := plannerRuntimeModeLine(true, cutoverIntents)
	require.Equal(t, "planner_mode=shadow cutover_intents=[resource,monitor]", line)

	line = plannerRuntimeModeLine(false, nil)
	require.Equal(t, "planner_mode=off cutover_intents=[]", line)
}

func TestPlannerRuntimeTrace(t *testing.T) {
	cutoverIntents, unknown := intentPlannerCutoverIntentsFromEnv(func(key string) string {
		if key == "USE_INTENT_PLANNER_FOR" {
			return "resource,monitor"
		}
		return ""
	})
	require.Empty(t, unknown)

	trace := plannerRuntimeTrace(true, cutoverIntents)
	require.Equal(t, "shadow", trace.PlannerMode)
	require.Equal(t, []string{"resource", "monitor"}, trace.CutoverIntents)

	trace = plannerRuntimeTrace(false, nil)
	require.Equal(t, "off", trace.PlannerMode)
	require.Empty(t, trace.CutoverIntents)
}

func TestGroundedRendererModeFromEnv(t *testing.T) {
	mode, unknown := groundedRendererModeFromEnv(func(key string) string {
		if key == "USE_GROUNDED_RENDERER" {
			return " llm "
		}
		return ""
	})
	require.Equal(t, "llm", mode)
	require.Empty(t, unknown)

	mode, unknown = groundedRendererModeFromEnv(func(string) string { return "weird" })
	require.Empty(t, mode)
	require.Equal(t, "weird", unknown)

	require.Equal(t, "grounded_renderer=llm", groundedRendererRuntimeLine("llm"))
	require.Equal(t, "grounded_renderer=off", groundedRendererRuntimeLine(""))
}

func TestMutatingToolsFromEnvAndRuntimeLine(t *testing.T) {
	enabled, unknown := mutatingToolsEnabledFromEnv(func(string) string { return "" })
	require.False(t, enabled)
	require.Empty(t, unknown)
	require.Equal(t, "mutating=disabled (read-only mode)", mutatingToolsRuntimeLine(enabled))

	enabled, unknown = mutatingToolsEnabledFromEnv(func(key string) string {
		if key == "COMPSHARE_ENABLE_MUTATING_TOOLS" {
			return "1"
		}
		return ""
	})
	require.True(t, enabled)
	require.Empty(t, unknown)
	require.Equal(t, "mutating=enabled", mutatingToolsRuntimeLine(enabled))

	enabled, unknown = mutatingToolsEnabledFromEnv(func(key string) string {
		if key == "COMPSHARE_ENABLE_MUTATING_TOOLS" {
			return "yes"
		}
		return ""
	})
	require.False(t, enabled)
	require.Equal(t, "yes", unknown)
}

func TestKnowledgeRetrievalModeFromEnv(t *testing.T) {
	enabled, unknown := knowledgeRetrievalModeFromEnv(func(string) string { return "" })
	if enabled || unknown != "" {
		t.Fatalf("unset knowledge retrieval = %v/%q, want disabled", enabled, unknown)
	}
	enabled, unknown = knowledgeRetrievalModeFromEnv(func(key string) string {
		if key == "USE_KNOWLEDGE_RETRIEVAL" {
			return " curated "
		}
		return ""
	})
	if !enabled || unknown != "" {
		t.Fatalf("curated mode = %v/%q, want enabled", enabled, unknown)
	}
	enabled, unknown = knowledgeRetrievalModeFromEnv(func(key string) string {
		if key == "USE_KNOWLEDGE_RETRIEVAL" {
			return "raw-chat"
		}
		return ""
	})
	if enabled || unknown != "raw-chat" {
		t.Fatalf("unknown mode = %v/%q, want disabled raw-chat", enabled, unknown)
	}
}

func TestKnowledgeCorpusPathFromEnv(t *testing.T) {
	if got := knowledgeCorpusPathFromEnv(func(string) string { return "" }); got != defaultKnowledgeCorpusPath {
		t.Fatalf("default corpus path = %q, want %q", got, defaultKnowledgeCorpusPath)
	}
	if got := defaultKnowledgeCorpusPath; got != "deploy/kb/stage2b_w0.jsonl" {
		t.Fatalf("default corpus path = %q, want stage2b W0 corpus", got)
	}
	got := knowledgeCorpusPathFromEnv(func(key string) string {
		if key == "COMPSHARE_KNOWLEDGE_CORPUS" {
			return " custom.jsonl "
		}
		return ""
	})
	if got != "custom.jsonl" {
		t.Fatalf("custom corpus path = %q", got)
	}
}

func TestKnowledgeRetrieverFromEnvLoadsCorpus(t *testing.T) {
	retriever, enabled, err := knowledgeRetrieverFromEnv(func(key string) string {
		switch key {
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "curated"
		case "COMPSHARE_KNOWLEDGE_CORPUS":
			return filepath.Join("..", "deploy", "kb", "stage2b_w0.jsonl")
		default:
			return ""
		}
	})

	require.NoError(t, err)
	require.True(t, enabled)
	require.NotNil(t, retriever)
	result := retriever.Retrieve("Windows 远程登录", "windows")
	if result.Empty || len(result.Hits) == 0 || result.KBVersion != "kb.stage2b.w0.2026-05-13" {
		t.Fatalf("retrieval result = %#v", result)
	}
}

func TestKnowledgeRetrieverFromEnvMissingCorpusDisablesWithError(t *testing.T) {
	retriever, enabled, err := knowledgeRetrieverFromEnv(func(key string) string {
		switch key {
		case "USE_KNOWLEDGE_RETRIEVAL":
			return "curated"
		case "COMPSHARE_KNOWLEDGE_CORPUS":
			return filepath.Join(t.TempDir(), "missing.jsonl")
		default:
			return ""
		}
	})
	if err == nil {
		t.Fatal("missing corpus should return an error")
	}
	if enabled || retriever != nil {
		t.Fatalf("missing corpus = enabled %v retriever %#v, want disabled nil", enabled, retriever)
	}
}

func TestCLIShadowPlannerInputUsesRegistrySnapshot(t *testing.T) {
	eng := engine.NewWithDeps(cmdMockLLM{}, cmdRegistryExecutor{}, nil)
	if _, err := eng.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	input := cliShadowPlannerInput(eng, "check uhost-cli monitor")
	if input.Resolver == nil {
		t.Fatal("shadow planner input must include immutable registry resolver")
	}
	got, res := input.Resolver.ResolveByID("uhost-cli")
	if res.Status != entity.ResolveHit || got == nil || got.Name != "cli-host" {
		t.Fatalf("resolver ResolveByID = (%#v, %#v), want uhost-cli hit", got, res)
	}
}

func TestCLITraceRecorderWritesPlannerTrace(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "planner trace", start)
	recorder.SetPlannerTraceSupplier(func() observability.PlannerTrace {
		return observability.PlannerTrace{
			Enabled:     true,
			Model:       "deepseek-v4-flash",
			SchemaValid: true,
			Intent:      "monitor_query",
			Slots: observability.PlannerSlots{
				Metrics: []string{"gpu"},
			},
			Confidence: 0.8,
		}
	})
	recorder.OnStep(engine.StepEvent{
		Type:   engine.StepToolCall,
		Action: "DescribeCompShareInstance",
		Source: observability.ToolSourceMainReAct,
		Args:   map[string]any{"Limit": 10},
	})
	recorder.OnStep(engine.StepEvent{
		Type:        engine.StepToolResult,
		Action:      "DescribeCompShareInstance",
		Source:      observability.ToolSourceMainReAct,
		TraceResult: map[string]any{"RetCode": 0},
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if !record.Planner.Enabled || !record.Planner.SchemaValid ||
		record.Planner.Model != "deepseek-v4-flash" || record.Planner.Intent != "monitor_query" {
		t.Fatalf("planner trace = %#v", record.Planner)
	}
	if len(record.ToolCalls) != 1 || record.ToolCalls[0].Action != "DescribeCompShareInstance" {
		t.Fatalf("tool calls changed by planner trace supplier: %#v", record.ToolCalls)
	}
}

func TestCLITraceRecorderWritesRuntimeTrace(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "runtime", start)
	recorder.SetRuntimeTrace(observability.RuntimeTrace{
		PlannerMode:    "shadow",
		CutoverIntents: []string{"resource", "monitor"},
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	require.Equal(t, "shadow", record.Runtime.PlannerMode)
	require.Equal(t, []string{"resource", "monitor"}, record.Runtime.CutoverIntents)
}

func TestCLITraceRecorderAcceptsEnginePlannerTrace(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "cutover trace", start)
	recorder.SetPlannerTrace(observability.PlannerTrace{
		Enabled:       true,
		Model:         "deepseek-v4-flash",
		SchemaValid:   true,
		Intent:        "resource_info",
		Confidence:    0.9,
		CutoverStatus: "dispatched",
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if !record.Planner.Enabled || record.Planner.Intent != "resource_info" ||
		record.Planner.CutoverStatus != "dispatched" {
		t.Fatalf("planner trace = %#v", record.Planner)
	}
}

func TestCLITraceRecorderAcceptsRetrievalTrace(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "knowledge trace", start)
	recorder.SetRetrievalTrace(observability.RetrievalTrace{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      2,
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if !record.Retrieval.Enabled || record.Retrieval.KBVersion != "kb.v1" || record.Retrieval.Hits != 2 {
		t.Fatalf("retrieval trace = %#v", record.Retrieval)
	}
}

func TestCLITraceRecorderAcceptsOutcomeTrace(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "knowledge trace", start)
	recorder.SetOutcomeTrace(observability.OutcomeTrace{
		AttemptedHallucinatedCount: 1,
		EscapedHallucinatedCount:   1,
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if record.Outcome.AttemptedHallucinatedCount != 1 || record.Outcome.EscapedHallucinatedCount != 1 {
		t.Fatalf("outcome trace = %#v", record.Outcome)
	}
}

func TestCLITraceRecorderWritesRendererInputToolArgHashes(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "monitor", start)
	recorder.OnStep(engine.StepEvent{
		Type:   engine.StepToolCall,
		Action: "GetCompShareInstanceMonitor",
		Source: observability.ToolSourcePlannerHandler,
		Args:   map[string]any{"UHostIds": []string{"uhost-a"}},
	})
	recorder.OnStep(engine.StepEvent{
		Type:                       engine.StepToolResult,
		Action:                     "GetCompShareInstanceMonitor",
		Source:                     observability.ToolSourcePlannerHandler,
		TraceResult:                map[string]any{"RetCode": 0},
		RendererInputToolArgHashes: []string{"sha256:monitor-args"},
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if got := record.Renderer.InputToolArgHashes; len(got) != 1 || got[0] != "sha256:monitor-args" {
		t.Fatalf("renderer.input_tool_args_hashes = %#v", got)
	}
}

func TestCLITraceRecorderWritesRendererTrace(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "resource", start)
	recorder.SetRendererTrace(observability.RendererTrace{
		Enabled:             true,
		Status:              "fallback",
		EnvelopeKind:        "resource_info",
		InputEnvelopeHashes: []string{"sha256:env"},
		FallbackUsed:        true,
		FallbackReason:      "rate_limited",
		Model:               "deepseek-v4-flash",
		LatencyMS:           12,
		AttributionMode:     "envelope",
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if !record.Renderer.Enabled || record.Renderer.Status != "fallback" ||
		record.Renderer.EnvelopeKind != "resource_info" || record.Renderer.FallbackReason != "rate_limited" {
		t.Fatalf("renderer trace = %#v", record.Renderer)
	}
	if got := record.Renderer.InputEnvelopeHashes; len(got) != 1 || got[0] != "sha256:env" {
		t.Fatalf("renderer.input_envelope_hashes = %#v", got)
	}
}

func TestCLITraceRecorderWritesEngineHardBlockWithoutToolStep(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 2, "hard block", start)
	recorder.SetEngineHardBlock(observability.EngineHardBlockTrace{
		Hit:      true,
		Category: "account_billing_unsupported",
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if !record.EngineHardBlock.Hit || record.EngineHardBlock.Category != "account_billing_unsupported" {
		t.Fatalf("engine_hard_block = %#v", record.EngineHardBlock)
	}
	if len(record.ToolCalls) != 0 {
		t.Fatalf("hard block signal must not add tool calls: %#v", record.ToolCalls)
	}
}

func TestCLITraceRecorderPlannerInvalidTraceStillWritesLine(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 3, "planner failure", start)
	recorder.SetPlannerTraceSupplier(func() observability.PlannerTrace {
		return observability.PlannerTrace{
			Enabled:     true,
			Model:       "deepseek-v4-flash",
			SchemaValid: false,
			Intent:      "unknown",
		}
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if !record.Planner.Enabled || record.Planner.SchemaValid || record.Planner.Intent != "unknown" {
		t.Fatalf("planner failure trace = %#v", record.Planner)
	}
}

func TestCLITraceRecorderWritesOneRedactedTraceLine(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	userMsg := "查监控 Bearer " + strings.Repeat("a", 25)
	recorder := newCLITraceRecorder(writer, 2, userMsg, start)

	recorder.OnStep(engine.StepEvent{
		Type:   engine.StepToolCall,
		Action: "GetCompShareInstanceMonitor",
		Source: observability.ToolSourceMainReAct,
		Args: map[string]any{
			"UHostIds": []any{"uhost-1"},
			"PublicIP": "192.168.1.2",
		},
	})
	recorder.OnStep(engine.StepEvent{
		Type:        engine.StepToolResult,
		Action:      "GetCompShareInstanceMonitor",
		Source:      observability.ToolSourceMainReAct,
		Message:     "调用成功",
		Attempts:    2,
		TraceResult: map[string]any{"ChargeAmount": "88.88"},
	})

	if err := recorder.Finish(nil, end); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	tracePath := filepath.Join(writer.Dir(), "agent-trace-2026-05-08.jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	for _, raw := range []string{strings.Repeat("a", 25), "192.168.1.2", "88.88", userMsg} {
		if strings.Contains(line, raw) {
			t.Fatalf("trace leaked raw value %q: %s", raw, line)
		}
	}

	var record observability.TraceRecord
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	if record.TurnIndex != 2 {
		t.Fatalf("turn_index = %d, want 2", record.TurnIndex)
	}
	if record.UserMsgHash == "" || !strings.HasPrefix(record.UserMsgHash, "sha256:") {
		t.Fatalf("user_msg_hash = %q, want sha256 hash", record.UserMsgHash)
	}
	if record.Outcome.TotalLatencyMS != 1500 {
		t.Fatalf("total_latency_ms = %d, want 1500", record.Outcome.TotalLatencyMS)
	}
	if !record.Freshness.MonitorCallInCurrentTurn {
		t.Fatal("monitor_call_in_current_turn = false, want true")
	}
	if len(record.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(record.ToolCalls))
	}
	call := record.ToolCalls[0]
	if call.Action != "GetCompShareInstanceMonitor" || call.Source != observability.ToolSourceMainReAct {
		t.Fatalf("tool call action/source = %s/%s", call.Action, call.Source)
	}
	if call.Status != observability.ToolStatusSuccess {
		t.Fatalf("status = %q, want success", call.Status)
	}
	if call.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", call.Attempts)
	}
	if call.ArgsHash == "" || call.ResultHash == "" {
		t.Fatalf("args/result hash must be populated: %#v", call)
	}
	if strings.Contains(line, `"entity_registry":`) {
		t.Fatalf("empty entity_registry block should be omitted: %s", line)
	}
}

func TestCLITraceRecorderWritesActualTotalTokens(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 1, "tokens", start)
	recorder.AddTokenUsage(llm.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10})
	recorder.SetPlannerTrace(observability.PlannerTrace{
		Enabled:      true,
		InputTokens:  11,
		OutputTokens: 5,
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if record.Outcome.TotalTokens != 26 {
		t.Fatalf("outcome.total_tokens = %d, want 26", record.Outcome.TotalTokens)
	}
}

func TestCLITraceRecorderWritesToolTargetAndWindowFields(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	require.NoError(t, err)
	recorder := newCLITraceRecorder(writer, 2, "monitor success", start)
	recorder.OnStep(engine.StepEvent{
		Type:   engine.StepToolCall,
		Action: "GetCompShareInstanceMonitor",
		Source: observability.ToolSourceMainReAct,
		Args: map[string]any{
			"UHostIds":  []any{"uhost-a", "uhost-b"},
			"StartTime": int64(1000),
			"EndTime":   int64(4600),
		},
	})
	recorder.OnStep(engine.StepEvent{
		Type:        engine.StepToolResult,
		Action:      "GetCompShareInstanceMonitor",
		Source:      observability.ToolSourceMainReAct,
		TraceResult: map[string]any{"RetCode": 0},
	})

	require.NoError(t, recorder.Finish(nil, start))

	record := readSingleTraceRecord(t, writer, start)
	require.Len(t, record.ToolCalls, 1)
	call := record.ToolCalls[0]
	require.Equal(t, 2, call.RequestedTargets)
	require.Equal(t, 2, call.ExecutedTargets)
	require.Equal(t, 3600, call.WindowSeconds)
	require.Empty(t, call.Capped)
}

func TestTraceWindowSecondsAcceptsSafeExecutorNumericShapes(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want int
	}{
		{
			name: "json number and string",
			args: map[string]any{"StartTime": json.Number("1000"), "EndTime": "4600"},
			want: 3600,
		},
		{
			name: "int32 and float32",
			args: map[string]any{"StartTime": int32(1000), "EndTime": float32(4600)},
			want: 3600,
		},
		{
			name: "unsafe float",
			args: map[string]any{"StartTime": float64(0), "EndTime": 1e20},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, traceWindowSeconds(tt.args))
		})
	}
}

func TestCLITraceRecorderWritesBlockedCapFields(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	require.NoError(t, err)
	ids := make([]any, 21)
	for i := range ids {
		ids[i] = "uhost-redacted"
	}
	recorder := newCLITraceRecorder(writer, 3, "monitor cap", start)
	recorder.OnStep(engine.StepEvent{
		Type:      engine.StepBlocked,
		Action:    "GetCompShareInstanceMonitor",
		Source:    observability.ToolSourceMainReAct,
		Args:      map[string]any{"UHostIds": ids, "StartTime": int64(1000), "EndTime": int64(4600)},
		Message:   "too many",
		Capped:    observability.ToolCappedTargets,
		CapReason: "too many targets",
	})

	require.NoError(t, recorder.Finish(nil, start))

	record := readSingleTraceRecord(t, writer, start)
	require.Len(t, record.ToolCalls, 1)
	call := record.ToolCalls[0]
	require.Equal(t, observability.ToolCappedTargets, call.Capped)
	require.Equal(t, "too many targets", call.CapReason)
	require.Equal(t, 21, call.RequestedTargets)
	require.Equal(t, 0, call.ExecutedTargets)
	require.Equal(t, 3600, call.WindowSeconds)
}

func TestCLITraceRecorderPairsRepeatedActionFIFO(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 3, "repeat action", start)

	for i := 0; i < 2; i++ {
		recorder.OnStep(engine.StepEvent{
			Type:   engine.StepToolCall,
			Action: "DescribeCompShareInstance",
			Source: observability.ToolSourceMainReAct,
			Args:   map[string]any{"Limit": i + 1},
		})
	}
	recorder.OnStep(engine.StepEvent{
		Type:        engine.StepToolResult,
		Action:      "DescribeCompShareInstance",
		Source:      observability.ToolSourceMainReAct,
		Attempts:    1,
		TraceResult: map[string]any{"Page": 1},
	})
	recorder.OnStep(engine.StepEvent{
		Type:        engine.StepToolResult,
		Action:      "DescribeCompShareInstance",
		Source:      observability.ToolSourceMainReAct,
		Attempts:    2,
		TraceResult: map[string]any{"Page": 2},
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	record := readSingleTraceRecord(t, writer, start)
	if len(record.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(record.ToolCalls))
	}
	if record.ToolCalls[0].Attempts != 1 || record.ToolCalls[1].Attempts != 2 {
		t.Fatalf("attempts = %d/%d, want 1/2", record.ToolCalls[0].Attempts, record.ToolCalls[1].Attempts)
	}
	for i, call := range record.ToolCalls {
		if call.Status != observability.ToolStatusSuccess {
			t.Fatalf("call %d status = %q, want success", i, call.Status)
		}
		if call.ArgsHash == "" || call.ResultHash == "" {
			t.Fatalf("call %d missing hash: %#v", i, call)
		}
	}
}

func TestCLITraceRecorderWritesCurrentRegistryStateFromSupplier(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 5, "registry state", start)
	state := observability.EntityRegistryTrace{
		SnapshotID: "sha256:old",
		AgeSeconds: 1,
		SyncEvent:  "init",
	}
	recorder.SetRegistryTraceSupplier(func(time.Time) observability.EntityRegistryTrace {
		return state
	})

	state = observability.EntityRegistryTrace{
		SnapshotID: "sha256:0123456789abcdef",
		AgeSeconds: 12,
		SyncEvent:  "init",
	}
	if err := recorder.Finish(nil, start.Add(12*time.Second)); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	record := readSingleTraceRecord(t, writer, start)
	if record.EntityRegistry.SnapshotID != "sha256:0123456789abcdef" ||
		record.EntityRegistry.AgeSeconds != 12 ||
		record.EntityRegistry.SyncEvent != "init" {
		t.Fatalf("entity registry trace = %#v", record.EntityRegistry)
	}
}

func TestCLITraceRecorderWritesFailedRegistryStateWithoutRawError(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	rawBearer := "Bearer " + strings.Repeat("z", 25)
	reg := entity.NewRegistry()
	_ = reg.Refresh(context.Background(), entityExecutorFunc(func(context.Context, string, map[string]any) (map[string]any, error) {
		return nil, errors.New("network down " + rawBearer)
	}), entity.RefreshReasonInit)

	recorder := newCLITraceRecorder(writer, 6, "registry failed", start)
	recorder.SetRegistryTraceSupplier(func(now time.Time) observability.EntityRegistryTrace {
		state := reg.TraceState(now)
		return observability.EntityRegistryTrace{
			SnapshotID: state.SnapshotID,
			AgeSeconds: state.AgeSeconds,
			SyncEvent:  state.SyncEvent,
		}
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	tracePath := filepath.Join(writer.Dir(), "agent-trace-2026-05-08.jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if strings.Contains(line, rawBearer) || strings.Contains(line, strings.Repeat("z", 25)) {
		t.Fatalf("trace leaked raw registry refresh error: %s", line)
	}
	record := readSingleTraceRecord(t, writer, start)
	if record.EntityRegistry.SyncEvent != "failed" {
		t.Fatalf("sync_event = %q, want failed", record.EntityRegistry.SyncEvent)
	}
}

func TestCLITraceRecorderWritesRateLimitDenial(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	rawPublicKey := "public-key-that-must-not-appear"
	subjectHash, ok := governance.SubjectKeyFromPublicKey(rawPublicKey)
	if !ok {
		t.Fatal("SubjectKeyFromPublicKey returned ok=false")
	}
	recorder := newCLITraceRecorder(writer, 7, "rate limited", start)
	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     false,
		Class:       governance.ClassLLM,
		Action:      "shadow_planner",
		Reason:      governance.ReasonQPSExceeded,
		SubjectHash: subjectHash,
		RetryAfter:  200 * time.Millisecond,
		Err:         governance.ErrRateLimited,
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	tracePath := filepath.Join(writer.Dir(), "agent-trace-2026-05-08.jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if strings.Contains(line, rawPublicKey) {
		t.Fatalf("trace leaked raw public key: %s", line)
	}
	record := readSingleTraceRecord(t, writer, start)
	got := record.RateLimit
	if !got.Checked || got.Allowed || got.Class != string(governance.ClassLLM) ||
		got.Action != "shadow_planner" || got.Reason != string(governance.ReasonQPSExceeded) ||
		got.SubjectHash != subjectHash || got.RetryAfterMS != 200 {
		t.Fatalf("rate_limit trace = %#v", got)
	}
}

func TestCLITraceRecorderWritesInitRateLimitDenial(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 0, "init_context", start)
	recorder.SetRuntimeTrace(observability.RuntimeTrace{PlannerMode: "shadow"})
	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     false,
		Class:       governance.ClassReadExpensiveTool,
		Action:      "DescribeCompShareInstance",
		Reason:      governance.ReasonDailyExceeded,
		SubjectHash: "sha256:subject",
		Err:         governance.ErrRateLimited,
	})

	require.True(t, recorder.HasRateLimitDenial())
	require.NoError(t, recorder.Finish(nil, start))

	record := readSingleTraceRecord(t, writer, start)
	require.Equal(t, 0, record.TurnIndex)
	require.Equal(t, "shadow", record.Runtime.PlannerMode)
	require.True(t, record.RateLimit.Checked)
	require.False(t, record.RateLimit.Allowed)
	require.Equal(t, string(governance.ClassReadExpensiveTool), record.RateLimit.Class)
	require.Equal(t, "DescribeCompShareInstance", record.RateLimit.Action)
	require.Equal(t, string(governance.ReasonDailyExceeded), record.RateLimit.Reason)
}

func TestCLITraceRecorderRateLimitDecisionAggregation(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 8, "multi decision", start)

	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     true,
		Class:       governance.ClassLLM,
		Action:      "main_react_chat",
		SubjectHash: "sha256:first",
	})
	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     false,
		Class:       governance.ClassLLM,
		Action:      "shadow_planner",
		Reason:      governance.ReasonDailyExceeded,
		SubjectHash: "sha256:denied",
		RetryAfter:  time.Minute,
		Err:         governance.ErrRateLimited,
	})
	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     true,
		Class:       governance.ClassMutatingTool,
		Action:      "StartCompShareInstance",
		SubjectHash: "sha256:last-allow",
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	record := readSingleTraceRecord(t, writer, start)
	if record.RateLimit.Allowed || record.RateLimit.Action != "shadow_planner" ||
		record.RateLimit.Reason != string(governance.ReasonDailyExceeded) {
		t.Fatalf("first denial should win, got %#v", record.RateLimit)
	}
}

func TestCLITraceRecorderRateLimitLastAllowWinsWhenNoDenial(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 9, "multi allow", start)
	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     true,
		Class:       governance.ClassLLM,
		Action:      "main_react_chat",
		SubjectHash: "sha256:first",
	})
	recorder.SetRateLimitDecision(governance.Decision{
		Allowed:     true,
		Class:       governance.ClassLLM,
		Action:      "shadow_planner",
		SubjectHash: "sha256:last",
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	record := readSingleTraceRecord(t, writer, start)
	if !record.RateLimit.Allowed || record.RateLimit.Action != "shadow_planner" || record.RateLimit.SubjectHash != "sha256:last" {
		t.Fatalf("last allow should win, got %#v", record.RateLimit)
	}
}

func TestCLITraceRecorderTraceWriteFailureDoesNotChangeRateLimitDecision(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: dir,
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove trace dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("replace trace dir with file: %v", err)
	}
	decision := governance.Decision{
		Allowed:     false,
		Class:       governance.ClassLLM,
		Action:      "shadow_planner",
		Reason:      governance.ReasonQPSExceeded,
		SubjectHash: "sha256:subject",
		RetryAfter:  200 * time.Millisecond,
		Err:         governance.ErrRateLimited,
	}
	recorder := newCLITraceRecorder(writer, 10, "write failure", start)
	recorder.SetRateLimitDecision(decision)

	if err := recorder.Finish(nil, start); err == nil {
		t.Fatal("Finish should fail when trace path is not a directory")
	}
	if decision.Allowed || decision.Reason != governance.ReasonQPSExceeded || decision.SubjectHash != "sha256:subject" {
		t.Fatalf("trace write failure mutated original decision: %#v", decision)
	}
	if !recorder.record.RateLimit.Checked || recorder.record.RateLimit.Allowed {
		t.Fatalf("trace write failure changed recorder decision: %#v", recorder.record.RateLimit)
	}
}

func TestCLITraceRecorderPreservesNonMainSources(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	writer, err := observability.NewWriter(observability.WriterOptions{
		Dir: t.TempDir(),
		Now: func() time.Time { return start },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	recorder := newCLITraceRecorder(writer, 4, "workflow and diagnosis", start)

	recorder.OnStep(engine.StepEvent{
		Type:   engine.StepToolCall,
		Action: "DescribeCompShareInstance",
		Source: observability.ToolSourceWorkflowInternal,
		Args:   map[string]any{"Limit": 1},
	})
	recorder.OnStep(engine.StepEvent{
		Type:        engine.StepToolResult,
		Action:      "DescribeCompShareInstance",
		Source:      observability.ToolSourceWorkflowInternal,
		TraceResult: map[string]any{"RetCode": 0},
	})
	recorder.OnStep(engine.StepEvent{
		Type:    engine.StepError,
		Action:  "DescribeCompShareInstance",
		Source:  observability.ToolSourceDiagnosisInternal,
		Message: "diagnosis step failed",
	})

	if err := recorder.Finish(nil, start); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	record := readSingleTraceRecord(t, writer, start)
	if len(record.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(record.ToolCalls))
	}
	if record.ToolCalls[0].Source != observability.ToolSourceWorkflowInternal {
		t.Fatalf("first source = %q, want workflow_internal", record.ToolCalls[0].Source)
	}
	if record.ToolCalls[0].Status != observability.ToolStatusSuccess {
		t.Fatalf("first status = %q, want success", record.ToolCalls[0].Status)
	}
	if record.ToolCalls[1].Source != observability.ToolSourceDiagnosisInternal {
		t.Fatalf("second source = %q, want diagnosis_internal", record.ToolCalls[1].Source)
	}
	if record.ToolCalls[1].Status != observability.ToolStatusError || record.ToolCalls[1].ErrorClass == "" {
		t.Fatalf("second status/error = %q/%q, want error with class", record.ToolCalls[1].Status, record.ToolCalls[1].ErrorClass)
	}
}

type entityExecutorFunc func(context.Context, string, map[string]any) (map[string]any, error)

func (f entityExecutorFunc) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return f(ctx, action, args)
}

type cmdMockLLM struct{}

func (cmdMockLLM) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "ok"}, nil
}

type cmdRegistryExecutor struct{}

func (cmdRegistryExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	if action != "DescribeCompShareInstance" {
		return map[string]any{"RetCode": 0}, nil
	}
	return map[string]any{
		"RetCode":    0,
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-cli",
				"Name":    "cli-host",
				"State":   "Running",
			},
		},
	}, nil
}

func readSingleTraceRecord(t *testing.T, writer *observability.Writer, now time.Time) observability.TraceRecord {
	t.Helper()
	tracePath := filepath.Join(writer.Dir(), "agent-trace-"+now.Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("trace line count = %d, want 1: %s", len(lines), string(data))
	}
	var record observability.TraceRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	return record
}
