package httpapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
)

// TestChatTraceRecorder_EmitStepAccumulatesSingleEnqueue mirrors the CLI-side
// guard for the HTTP path: orchestrator saga StepTraces fold into THIS turn's
// record.Steps[] and persist with the SINGLE Enqueue at Finish — never a
// per-step INSERT (which would collide uk_request_uuid, one agent_traces row
// per turn). *chatTraceRecorder thus satisfies orchestrator.StepSink, and the
// tenant context is carried on the single Enqueue.
func TestChatTraceRecorder_EmitStepAccumulatesSingleEnqueue(t *testing.T) {
	w := &captureTraceWriter{}
	base := BaseRequest{RequestUUID: "req-1", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", time.Now())
	require.NotNil(t, rec)

	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s0", State: observability.StepStateRunning}))
	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s0", State: observability.StepStateSuccess}))

	require.NoError(t, rec.Finish(nil, time.Now()))

	require.Len(t, w.records, 1, "exactly one Enqueue per turn (no per-step INSERT)")
	require.Len(t, w.records[0].Steps, 2)
	assert.Equal(t, observability.StepStateSuccess, w.records[0].Steps[1].State)
	require.Len(t, w.tenants, 1)
	assert.Equal(t, int64(1), w.tenants[0].TopOrgID, "tenant carried on the single Enqueue (Enqueue path, not Append)")
}
