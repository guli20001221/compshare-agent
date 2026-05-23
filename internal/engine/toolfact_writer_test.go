package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newEngineForToolFactTest constructs a minimal Engine sufficient to exercise
// the M2 ToolFact writer path.
func newEngineForToolFactTest(t *testing.T) *Engine {
	t.Helper()
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         &mockExecutor{results: map[string]map[string]any{}},
	}
	e := NewSession(deps, SessionOptions{Subject: "test-subject"})
	// Hydrate so the writer is allowed to touch sessionState.
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 0)
	e.userTurn = 1
	return e
}

// ---------------------------------------------------------------------------
// recordInstanceStateFacts unit tests
// ---------------------------------------------------------------------------

// TestRecordInstanceStateFacts_TwoHosts asserts each UHostId in the
// DescribeCompShareInstance UHostSet produces one instance_state fact with
// the documented payload keys, all numerics coerced to float64.
func TestRecordInstanceStateFacts_TwoHosts(t *testing.T) {
	e := newEngineForToolFactTest(t)
	raw := map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":  "uhost-A",
				"Name":     "train-a",
				"State":    "Running",
				"GPU":      2,
				"GpuType":  "RTX4090",
				"CPU":      16,
				"Memory":   65536,
				"Zone":     "cn-bj-01",
				"OsType":   "linux",
				"OtherKey": "irrelevant",
			},
			map[string]any{
				"UHostId": "uhost-B",
				"Name":    "infer-b",
				"State":   "Stopped",
				"GPU":     1,
				"GpuType": "V100S",
				"CPU":     8,
				"Memory":  32768,
				"Zone":    "cn-bj-01",
			},
		},
	}
	e.recordInstanceStateFacts(raw)

	facts := e.sessionState.RecentFacts
	require.Len(t, facts, 2, "one fact per UHostId")

	bySubject := make(map[string]ToolFact)
	for _, f := range facts {
		bySubject[f.SubjectID] = f
	}

	a, ok := bySubject["uhost-A"]
	require.True(t, ok)
	assert.Equal(t, FactKindInstanceState, a.Kind)
	assert.Equal(t, "train-a", a.Payload["name"])
	assert.Equal(t, "Running", a.Payload["state"])
	assert.Equal(t, float64(2), a.Payload["gpu"], "int must be coerced to float64 by toFactNumeric")
	assert.Equal(t, "RTX4090", a.Payload["gpu_type"])
	assert.Equal(t, float64(16), a.Payload["cpu"])
	assert.Equal(t, float64(65536), a.Payload["memory"])
	assert.Equal(t, "cn-bj-01", a.Payload["zone"])
	assert.Equal(t, factTTLSecondsInstanceState, a.TTLSeconds)
	assert.Equal(t, e.userTurn, a.ProducedAtTurn)
	assert.NotZero(t, a.ProducedAtUnix)

	// "OsType" is intentionally not in expectedPayloadKeysForKind for this
	// version; verify it didn't sneak into the payload.
	_, hasOs := a.Payload["os_type"]
	assert.False(t, hasOs, "os_type is not in v1 contract; writer must not emit it")
	_, hasOther := a.Payload["OtherKey"]
	assert.False(t, hasOther, "extraneous keys must not be copied verbatim")
}

// TestRecordInstanceStateFacts_EmptyUHostSet covers the no-fact case: an
// empty / missing UHostSet must not produce any fact.
func TestRecordInstanceStateFacts_EmptyUHostSet(t *testing.T) {
	e := newEngineForToolFactTest(t)
	cases := []map[string]any{
		nil,
		{},
		{"UHostSet": []any{}},
		{"UHostSet": []any{nil}},
		{"UHostSet": []any{map[string]any{"UHostId": ""}}}, // empty ID skipped
	}
	for _, raw := range cases {
		e.sessionState.RecentFacts = nil
		e.recordInstanceStateFacts(raw)
		assert.Empty(t, e.sessionState.RecentFacts, "no fact expected for input %#v", raw)
	}
}

// TestRecordInstanceStateFacts_DedupePerUHostId asserts that two successive
// successful calls for the same UHostId result in ONE fact (newest wins),
// not two.
func TestRecordInstanceStateFacts_DedupePerUHostId(t *testing.T) {
	e := newEngineForToolFactTest(t)
	first := map[string]any{
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-A", "Name": "old", "State": "Running"},
		},
	}
	second := map[string]any{
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-A", "Name": "new", "State": "Stopped"},
		},
	}
	e.recordInstanceStateFacts(first)
	e.recordInstanceStateFacts(second)

	facts := e.sessionState.RecentFacts
	require.Len(t, facts, 1, "dedupe by (kind, subject) — one fact for uhost-A")
	assert.Equal(t, "new", facts[0].Payload["name"], "newest fact wins")
	assert.Equal(t, "Stopped", facts[0].Payload["state"])
}

// ---------------------------------------------------------------------------
// recordMonitorSampleFacts unit tests
// ---------------------------------------------------------------------------

// TestRecordMonitorSampleFacts_OneHost_FourMetrics covers the canonical
// shape: a single host with cpu / memory / gpu / vram metrics produces one
// fact whose Payload contains all four keys.
func TestRecordMonitorSampleFacts_OneHost_FourMetrics(t *testing.T) {
	e := newEngineForToolFactTest(t)
	raw := monitorPayload([]monitorPayloadHost{
		{
			UHostID: "uhost-A",
			Metrics: []monitorPayloadMetric{
				{Key: "uhost_cpu_used", Values: [][2]any{{1716530000, "82.5"}}},
				{Key: "cloudwatch_memory_usage", Values: [][2]any{{1716530000, "44.0"}}},
				{Key: "cloudwatch_gpu_util", Values: [][2]any{{1716530000, "92.5"}}},
				{Key: "cloudwatch_gpu_memory_usage", Values: [][2]any{{1716530000, "70.0"}}},
			},
		},
	})
	e.recordMonitorSampleFacts(raw)

	facts := e.sessionState.RecentFacts
	require.Len(t, facts, 1, "one fact per host even with multiple metrics")
	f := facts[0]
	assert.Equal(t, FactKindMonitorSample, f.Kind)
	assert.Equal(t, "uhost-A", f.SubjectID)
	assert.Equal(t, "82.5", f.Payload["cpu_usage"])
	assert.Equal(t, "44.0", f.Payload["memory_usage"])
	assert.Equal(t, "92.5", f.Payload["gpu_usage"])
	assert.Equal(t, "70.0", f.Payload["vram_usage"])
	assert.Equal(t, factTTLSecondsMonitorSample, f.TTLSeconds)
}

// TestRecordMonitorSampleFacts_EmptyOrUnknownPayload tests the no-fact case.
func TestRecordMonitorSampleFacts_EmptyOrUnknownPayload(t *testing.T) {
	e := newEngineForToolFactTest(t)
	cases := []map[string]any{
		nil,
		{},
		{"Data": map[string]any{}}, // no List
		{"Data": map[string]any{"List": []any{}}},
	}
	for _, raw := range cases {
		e.sessionState.RecentFacts = nil
		e.recordMonitorSampleFacts(raw)
		assert.Empty(t, e.sessionState.RecentFacts, "no fact expected for input %#v", raw)
	}
}

// TestRecordMonitorSampleFacts_TwoHosts_ProducesTwoFacts verifies grouping
// by UHostId — two hosts in one Monitor call → two facts.
func TestRecordMonitorSampleFacts_TwoHosts_ProducesTwoFacts(t *testing.T) {
	e := newEngineForToolFactTest(t)
	raw := monitorPayload([]monitorPayloadHost{
		{
			UHostID: "uhost-A",
			Metrics: []monitorPayloadMetric{
				{Key: "uhost_cpu_used", Values: [][2]any{{1716530000, "10.0"}}},
			},
		},
		{
			UHostID: "uhost-B",
			Metrics: []monitorPayloadMetric{
				{Key: "uhost_cpu_used", Values: [][2]any{{1716530000, "20.0"}}},
			},
		},
	})
	e.recordMonitorSampleFacts(raw)

	require.Len(t, e.sessionState.RecentFacts, 2)
	bySubject := make(map[string]ToolFact)
	for _, f := range e.sessionState.RecentFacts {
		bySubject[f.SubjectID] = f
	}
	assert.Equal(t, "10.0", bySubject["uhost-A"].Payload["cpu_usage"])
	assert.Equal(t, "20.0", bySubject["uhost-B"].Payload["cpu_usage"])
}

// ---------------------------------------------------------------------------
// recordToolFacts gating
// ---------------------------------------------------------------------------

// TestRecordToolFacts_NotHydratedSkipsWrite verifies the M1 contract: an
// engine that is not hydrated must never have its sessionState mutated by
// the writer. This protects the CLI path (no SetSessionState ever called)
// from producing a non-empty RecentFacts slice that nothing consumes.
func TestRecordToolFacts_NotHydratedSkipsWrite(t *testing.T) {
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         &mockExecutor{results: map[string]map[string]any{}},
	}
	e := NewSession(deps, SessionOptions{Subject: "cli-subject"})
	// NOT hydrated.
	require.False(t, e.sessionStateHydrated)

	raw := map[string]any{
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-A", "Name": "x", "State": "Running"},
		},
	}
	e.recordToolFacts("DescribeCompShareInstance", &tools.SafeToolResult{RawResult: raw})

	assert.Empty(t, e.sessionState.RecentFacts,
		"un-hydrated engine must never write RecentFacts — CLI path safety")
}

// TestRecordToolFacts_NilResultSkipsWrite covers the defensive nil checks.
func TestRecordToolFacts_NilResultSkipsWrite(t *testing.T) {
	e := newEngineForToolFactTest(t)
	e.recordToolFacts("DescribeCompShareInstance", nil)
	e.recordToolFacts("DescribeCompShareInstance", &tools.SafeToolResult{RawResult: nil})
	assert.Empty(t, e.sessionState.RecentFacts)
}

// TestRecordToolFacts_UnknownActionSkipsWrite covers v1 supported set —
// non-fact-producing actions must not write facts.
func TestRecordToolFacts_UnknownActionSkipsWrite(t *testing.T) {
	e := newEngineForToolFactTest(t)
	raw := map[string]any{"some": "result"}
	for _, action := range []string{
		"StartCompShareInstance",
		"DescribeCompShareImages",
		"GetCompShareInstancePrice",
		"DescribeAvailableCompShareInstanceTypes",
		"DescribeCommunityImages",
	} {
		e.sessionState.RecentFacts = nil
		e.recordToolFacts(action, &tools.SafeToolResult{RawResult: raw})
		assert.Emptyf(t, e.sessionState.RecentFacts, "action %q must not produce facts in M2 v1", action)
	}
}

// ---------------------------------------------------------------------------
// executeSafeTool integration: OriginDirectLLM filter
// ---------------------------------------------------------------------------

// TestExecuteSafeTool_OriginDirectLLM_RecordsFacts is the integration
// contract: a Direct-LLM-driven DescribeCompShareInstance writes a fact.
// Mutation: removing the recordToolFacts call from executeSafeTool fails
// this test.
func TestExecuteSafeTool_OriginDirectLLM_RecordsFacts(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{
					map[string]any{
						"UHostId": "uhost-A", "Name": "test", "State": "Running",
						"GPU": 1, "GpuType": "RTX4090",
					},
				},
			},
		},
	}
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         exec,
	}
	e := NewSession(deps, SessionOptions{Subject: "test-subject"})
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 0)
	e.userTurn = 1

	_, err := e.executeSafeTool(context.Background(), tools.SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 100},
		Origin: tools.OriginDirectLLM,
	})
	require.NoError(t, err)

	facts := e.sessionState.RecentFacts
	require.Len(t, facts, 1, "OriginDirectLLM call should record one fact")
	assert.Equal(t, "uhost-A", facts[0].SubjectID)
}

// TestExecuteSafeTool_OriginWorkflowInternal_DoesNotRecordFacts verifies
// the ORIGIN filter: an OriginWorkflowInternal call (workflow probing the
// same tool internally) MUST NOT pollute SessionState.RecentFacts.
//
// Why: workflow's internal probes are not user-driven. If they wrote
// facts, "刚才那台" follow-up memory would surface state the user never
// saw or asked about. This is the [[project-context-first-roadmap]]
// rule #1 ("ToolFacts is NOT for saving API calls; it's for 续问定位 +
// 解释刚才结果").
//
// Mutation: removing the `req.Origin == tools.OriginDirectLLM` guard from
// executeSafeTool fails this test.
func TestExecuteSafeTool_OriginWorkflowInternal_DoesNotRecordFacts(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-A", "Name": "x", "State": "Running"},
				},
			},
		},
	}
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         exec,
	}
	e := NewSession(deps, SessionOptions{Subject: "test-subject"})
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 0)
	e.userTurn = 1

	_, err := e.executeSafeTool(context.Background(), tools.SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 100},
		Origin: tools.OriginWorkflowInternal,
	})
	require.NoError(t, err)
	assert.Empty(t, e.sessionState.RecentFacts,
		"OriginWorkflowInternal must NOT pollute RecentFacts")
}

// TestExecuteSafeTool_OriginDiagnosisInternal_DoesNotRecordFacts mirrors
// the workflow-internal test for diagnosis-internal probes.
func TestExecuteSafeTool_OriginDiagnosisInternal_DoesNotRecordFacts(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-A", "Name": "x", "State": "Running"},
				},
			},
		},
	}
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         exec,
	}
	e := NewSession(deps, SessionOptions{Subject: "test-subject"})
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 0)
	e.userTurn = 1

	_, err := e.executeSafeTool(context.Background(), tools.SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 100},
		Origin: tools.OriginDiagnosisInternal,
	})
	require.NoError(t, err)
	assert.Empty(t, e.sessionState.RecentFacts,
		"OriginDiagnosisInternal must NOT pollute RecentFacts")
}

// TestExecuteSafeTool_PayloadRoundTripStable runs the writer + JSON
// round-trip end-to-end: a DescribeCompShareInstance call's resulting
// fact must reflect.DeepEqual after marshal+unmarshal of the whole
// SessionState.
func TestExecuteSafeTool_PayloadRoundTripStable(t *testing.T) {
	exec := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"UHostSet": []any{
					map[string]any{
						"UHostId": "uhost-A", "Name": "test", "State": "Running",
						"GPU": 2, "GpuType": "RTX4090", "CPU": 16, "Memory": 64,
						"Zone": "cn-bj-01",
					},
				},
			},
		},
	}
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         exec,
	}
	e := NewSession(deps, SessionOptions{Subject: "test-subject"})
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 0)
	e.userTurn = 1

	_, err := e.executeSafeTool(context.Background(), tools.SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 100},
		Origin: tools.OriginDirectLLM,
	})
	require.NoError(t, err)
	require.Len(t, e.sessionState.RecentFacts, 1)

	// Round-trip the SessionState.
	state, _, _ := e.SessionStateSnapshot()
	pc := PersistedContext{AgentSessionState: state}
	roundTripState := mustRoundTripPersistedContext(t, pc)

	require.Equal(t, state, roundTripState.AgentSessionState,
		"writer-produced fact must reflect.DeepEqual after JSON round-trip")
}

func mustRoundTripPersistedContext(t *testing.T, pc PersistedContext) PersistedContext {
	t.Helper()
	raw, err := json.Marshal(pc)
	require.NoError(t, err)
	got, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	return got
}

// ---------------------------------------------------------------------------
// recordSelectedInstanceFromEnvelope / recordLastIntentFromPlan unit tests
// ---------------------------------------------------------------------------

// TestRecordSelectedInstanceFromEnvelope_SingleSubject is the canonical
// case: one Subject of type Instance → write SelectedInstanceID/Name.
func TestRecordSelectedInstanceFromEnvelope_SingleSubject(t *testing.T) {
	e := newEngineForToolFactTest(t)
	env := &envelope.Envelope{
		Kind: envelope.KindResourceInfo,
		Subjects: []envelope.Subject{
			{ID: "uhost-pick", Name: "train-a", Type: envelope.SubjectInstance},
		},
	}
	e.recordSelectedInstanceFromEnvelope(env)
	assert.Equal(t, "uhost-pick", e.sessionState.SelectedInstanceID)
	assert.Equal(t, "train-a", e.sessionState.SelectedInstanceName)
}

// TestRecordSelectedInstanceFromEnvelope_NotHydratedSkips guards the CLI-
// path safety: same as the fact writer, no mutation without explicit
// SetSessionState.
func TestRecordSelectedInstanceFromEnvelope_NotHydratedSkips(t *testing.T) {
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         &mockExecutor{results: map[string]map[string]any{}},
	}
	e := NewSession(deps, SessionOptions{Subject: "cli-subject"})
	require.False(t, e.sessionStateHydrated)

	env := &envelope.Envelope{
		Subjects: []envelope.Subject{{ID: "uhost-x", Name: "x", Type: envelope.SubjectInstance}},
	}
	e.recordSelectedInstanceFromEnvelope(env)
	assert.Empty(t, e.sessionState.SelectedInstanceID)
}

// TestRecordSelectedInstanceFromEnvelope_RejectsAmbiguousOrEmpty enumerates
// the cases where the engine MUST NOT write SelectedInstance: multiple
// subjects (ambiguous), zero subjects, wrong type, empty ID, nil envelope.
func TestRecordSelectedInstanceFromEnvelope_RejectsAmbiguousOrEmpty(t *testing.T) {
	cases := []struct {
		name string
		env  *envelope.Envelope
	}{
		{name: "nil envelope", env: nil},
		{name: "zero subjects", env: &envelope.Envelope{Subjects: []envelope.Subject{}}},
		{name: "two subjects (ambiguous)", env: &envelope.Envelope{Subjects: []envelope.Subject{
			{ID: "uhost-a", Type: envelope.SubjectInstance},
			{ID: "uhost-b", Type: envelope.SubjectInstance},
		}}},
		{name: "non-instance subject", env: &envelope.Envelope{Subjects: []envelope.Subject{
			{ID: "rtx-4090", Type: envelope.SubjectGPUModel},
		}}},
		{name: "empty ID", env: &envelope.Envelope{Subjects: []envelope.Subject{
			{ID: "", Type: envelope.SubjectInstance},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngineForToolFactTest(t)
			e.recordSelectedInstanceFromEnvelope(tc.env)
			assert.Empty(t, e.sessionState.SelectedInstanceID,
				"input %q must NOT set SelectedInstance", tc.name)
		})
	}
}

// TestRecordLastIntentFromPlan_AcceptsRuntimeIntents covers the happy path:
// any intent in RuntimeIntents() is written.
func TestRecordLastIntentFromPlan_AcceptsRuntimeIntents(t *testing.T) {
	e := newEngineForToolFactTest(t)
	for _, i := range []intent.Intent{
		intent.IntentResourceInfo,
		intent.IntentMonitorQuery,
		intent.IntentGPUSpecsQuery,
		intent.IntentPricingQuery,
		intent.IntentStockAvailability,
	} {
		e.sessionState.LastIntent = ""
		e.recordLastIntentFromPlan(intent.Plan{Intent: i})
		assert.Equalf(t, string(i), e.sessionState.LastIntent, "intent %s must be written", i)
	}
}

// TestRecordLastIntentFromPlan_RejectsInvalid covers the gate cases:
// empty intent, IntentUnknown, and non-RuntimeIntents short aliases.
func TestRecordLastIntentFromPlan_RejectsInvalid(t *testing.T) {
	e := newEngineForToolFactTest(t)
	// Pre-set a sentinel so we can verify NO write happens.
	e.sessionState.LastIntent = "_sentinel_"

	for _, tc := range []struct {
		name string
		plan intent.Plan
	}{
		{name: "empty intent", plan: intent.Plan{Intent: ""}},
		{name: "IntentUnknown", plan: intent.Plan{Intent: intent.IntentUnknown}},
		{name: "short alias 'monitor'", plan: intent.Plan{Intent: intent.Intent("monitor")}},
		{name: "made-up value", plan: intent.Plan{Intent: intent.Intent("hallucinated_intent")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.recordLastIntentFromPlan(tc.plan)
			assert.Equal(t, "_sentinel_", e.sessionState.LastIntent,
				"invalid intent %q must NOT overwrite existing LastIntent", tc.plan.Intent)
		})
	}
}

// TestRecordLastIntentFromPlan_NotHydratedSkips mirrors the CLI-path
// safety for LastIntent.
func TestRecordLastIntentFromPlan_NotHydratedSkips(t *testing.T) {
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         &mockExecutor{results: map[string]map[string]any{}},
	}
	e := NewSession(deps, SessionOptions{Subject: "cli-subject"})
	require.False(t, e.sessionStateHydrated)

	e.recordLastIntentFromPlan(intent.Plan{Intent: intent.IntentResourceInfo})
	assert.Empty(t, e.sessionState.LastIntent)
}

// TestSessionState_FieldsRoundTripWithSelectedAndIntent verifies the M2
// additions (SelectedInstance / LastIntent) survive JSON round-trip with
// reflect.DeepEqual semantics — the multi-replica preservation contract.
func TestSessionState_FieldsRoundTripWithSelectedAndIntent(t *testing.T) {
	e := newEngineForToolFactTest(t)
	e.recordSelectedInstanceFromEnvelope(&envelope.Envelope{Subjects: []envelope.Subject{
		{ID: "uhost-pick", Name: "train-a", Type: envelope.SubjectInstance},
	}})
	e.recordLastIntentFromPlan(intent.Plan{Intent: intent.IntentMonitorQuery})

	state, _, _ := e.SessionStateSnapshot()
	pc := PersistedContext{AgentSessionState: state}
	roundTripped := mustRoundTripPersistedContext(t, pc)

	assert.Equal(t, "uhost-pick", roundTripped.AgentSessionState.SelectedInstanceID)
	assert.Equal(t, "train-a", roundTripped.AgentSessionState.SelectedInstanceName)
	assert.Equal(t, string(intent.IntentMonitorQuery), roundTripped.AgentSessionState.LastIntent)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type monitorPayloadHost struct {
	UHostID string
	Metrics []monitorPayloadMetric
}

type monitorPayloadMetric struct {
	Key    string
	Values [][2]any // [timestamp, valueString]
}

// monitorPayload constructs a mock GetCompShareInstanceMonitor result
// matching the shape monitorSemanticFacts walks.
func monitorPayload(hosts []monitorPayloadHost) map[string]any {
	listItems := make([]any, 0, len(hosts))
	for _, h := range hosts {
		metrics := make([]any, 0, len(h.Metrics))
		for _, m := range h.Metrics {
			values := make([]any, 0, len(m.Values))
			for _, v := range m.Values {
				values = append(values, map[string]any{
					"Time":  v[0],
					"Value": v[1],
				})
			}
			metrics = append(metrics, map[string]any{
				"MetricKey": m.Key,
				"Results": []any{
					map[string]any{
						"Values": values,
					},
				},
			})
		}
		listItems = append(listItems, map[string]any{
			"UHostId": h.UHostID,
			"Metrics": metrics,
		})
	}
	return map[string]any{
		"Data": map[string]any{
			"List": listItems,
		},
	}
}
