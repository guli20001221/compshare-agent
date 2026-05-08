package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
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
