package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/observability"
)

// TestCLITraceRecorder_EmitStepAccumulatesSingleWrite proves the B6.2 "EmitStep
// 接真" wiring: the orchestrator saga's per-step traces fold into THIS turn's
// record.Steps[] and persist with the SINGLE Append at Finish — never a
// per-step INSERT (which on the MySQL sink would collide uk_request_uuid, one
// row per turn). *cliTraceRecorder thus satisfies orchestrator.StepSink.
func TestCLITraceRecorder_EmitStepAccumulatesSingleWrite(t *testing.T) {
	w := &captureAppendWriter{}
	rec := newCLITraceRecorder(w, "trace-1", 1, "msg", time.Now())

	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s0", State: observability.StepStateRunning}))
	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s0", State: observability.StepStateSuccess}))
	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s1", State: observability.StepStateSuccess}))

	require.NoError(t, rec.Finish(nil, time.Now()))

	require.Len(t, w.records, 1, "exactly one Append per turn (no per-step INSERT)")
	require.Len(t, w.records[0].Steps, 3, "all emitted steps fold into the single turn record")
	assert.Equal(t, "s0", w.records[0].Steps[0].StepID)
	assert.Equal(t, observability.StepStateSuccess, w.records[0].Steps[2].State)
}

// TestCLITraceRecorder_EmitStepRedactsViaPersist proves the end-to-end secret
// boundary: a StepTrace whose Args carry a secret is redacted by the central
// prepareForPersist choke point (RedactStepDerivedFields) when the turn record
// hits disk — the recorder stores raw, persist redacts.
func TestCLITraceRecorder_EmitStepRedactsViaPersist(t *testing.T) {
	dir := t.TempDir()
	w, err := observability.NewWriter(observability.WriterOptions{Dir: dir})
	require.NoError(t, err)

	rec := newCLITraceRecorder(w, "trace-1", 1, "msg", time.Now())
	require.NoError(t, rec.EmitStep(observability.StepTrace{
		StepID: "s0",
		State:  observability.StepStateSuccess,
		Args:   map[string]any{"password": "hunter2", "instance_id": "i-1"},
	}))
	require.NoError(t, rec.Finish(nil, time.Now()))

	files, err := filepath.Glob(filepath.Join(dir, "agent-trace-*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	line := string(data)

	assert.Contains(t, line, "[REDACTED]", "secret Arg must be redacted on disk")
	assert.NotContains(t, line, "hunter2", "raw secret must never reach disk")
	assert.Contains(t, line, "i-1", "non-secret Arg preserved")
}
