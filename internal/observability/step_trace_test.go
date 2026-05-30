package observability

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestStepTrace_MarshalKeys verifies a populated StepTrace marshals with the
// expected required keys and omits empty optional ones via omitempty.
func TestStepTrace_MarshalKeys(t *testing.T) {
	st := StepTrace{
		SessionID: "sess-1",
		TurnID:    "turn-1",
		StepID:    "step-1",
		State:     StepStateRunning,
		StartedAt: time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		// SagaID and Args left empty → must be omitted.
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal StepTrace: %v", err)
	}
	got := string(data)

	for _, key := range []string{`"session_id"`, `"step_id"`, `"state"`, `"started_at"`} {
		if !strings.Contains(got, key) {
			t.Errorf("StepTrace JSON missing required key %s: %s", key, got)
		}
	}
	for _, key := range []string{`"saga_id"`, `"args"`} {
		if strings.Contains(got, key) {
			t.Errorf("StepTrace JSON should omit empty optional key %s: %s", key, got)
		}
	}
}

// TestTraceRecord_NoStepsByteIdentity is the reserved-slot byte-identity guard:
// a populated TraceRecord with Steps == nil must marshal to JSON that does NOT
// contain the substring "steps". This proves B6.1 is zero-behavior — adding the
// omitempty Steps field cannot change any existing (non-agent) trace line.
// Mirrors the B1 task_tier reserved-slot precedent (which also did not bump the
// SchemaVersion because the field was omitempty and never populated).
func TestTraceRecord_NoStepsByteIdentity(t *testing.T) {
	rec := TraceRecord{
		SchemaVersion: SchemaVersion,
		TraceID:       "trace-1",
		TurnID:        "turn-1",
		TurnIndex:     0,
		Timestamp:     "2026-05-31T00:00:00Z",
		UserMsgHash:   "sha256:abc",
		Planner:       PlannerTrace{Enabled: true, Intent: "resource"},
		ToolCalls:     []ToolCallTrace{{ID: "tc-1", Action: "DescribeCompShareInstance", Source: ToolSourceMainReAct}},
		Retrieval:     RetrievalTrace{Enabled: true, Hits: 2},
		// Steps intentionally nil — the non-agent turn case.
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal TraceRecord: %v", err)
	}
	if strings.Contains(string(data), "steps") {
		t.Fatalf("TraceRecord with nil Steps must not emit \"steps\" key: %s", data)
	}
}

// TestRedactStepDerivedFields redacts secrets in Args and Result while leaving
// non-secret fields intact. The redaction sentinel is "[REDACTED]" (see
// internal/security/secret_boundary.go).
func TestRedactStepDerivedFields(t *testing.T) {
	steps := []StepTrace{
		{
			StepID: "step-1",
			Args:   map[string]any{"password": "hunter2", "instance_id": "i-1"},
			Result: map[string]any{"api_key": "sk-xyz"},
		},
	}
	RedactStepDerivedFields(steps)

	if got := steps[0].Args["password"]; got != "[REDACTED]" {
		t.Errorf("Args[password] = %v, want [REDACTED]", got)
	}
	if got := steps[0].Args["instance_id"]; got != "i-1" {
		t.Errorf("Args[instance_id] = %v, want i-1 (non-secret preserved)", got)
	}
	resultMap, ok := steps[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("Result is not map[string]any after redaction: %T", steps[0].Result)
	}
	if got := resultMap["api_key"]; got != "[REDACTED]" {
		t.Errorf("Result[api_key] = %v, want [REDACTED]", got)
	}
}

// Compile-time guard: *FileWriter still satisfies the widened Writer interface
// (which now requires EmitStep). The no-op EmitStep returns nil in B6.1.
var _ Writer = (*FileWriter)(nil)

func TestFileWriter_EmitStepNoop(t *testing.T) {
	if err := (&FileWriter{}).EmitStep(StepTrace{}); err != nil {
		t.Fatalf("FileWriter.EmitStep returned err=%v, want nil", err)
	}
}
