package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRegionFromZone(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"cn-wlcb-01", "cn-wlcb"},
		{"cn-sh2-02", "cn-sh2"},
		{"cn-bj2-04", "cn-bj2"},
		{"cn-gd-01a", "cn-gd"},
		{"  cn-sh2-02  ", "cn-sh2"}, // trims whitespace
		{"", ""},
		{"cn", ""},       // no separator
		{"-01", ""},      // leading-dash zone is malformed; refuse to fabricate Region
		{"cn-wlcb", ""},  // looks like a Region, not a Zone — refuse to derive "cn"
		{"foo-bar", ""},  // single dash; cannot distinguish Region from Zone
		{"a-b-c", "a-b"}, // minimal well-formed zone (two dashes)
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.want, regionFromZone(c.in))
		})
	}
}

func TestExtractInstanceRegion_PrefersExplicitField(t *testing.T) {
	result := map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId": "uhost-x",
			"Region":  "cn-sh2",
			"Zone":    "cn-bj2-04", // intentionally mismatched
		},
	}}
	// Region wins over derive-from-Zone when both present.
	assert.Equal(t, "cn-sh2", extractInstanceRegion(result, "fallback"))
}

func TestExtractInstanceRegion_DerivesFromZone(t *testing.T) {
	result := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-x", "Zone": "cn-sh2-02"},
	}}
	assert.Equal(t, "cn-sh2", extractInstanceRegion(result, "fallback"))
}

func TestExtractInstanceRegion_FallsBackWhenMissing(t *testing.T) {
	assert.Equal(t, "cn-wlcb", extractInstanceRegion(nil, "cn-wlcb"))
	assert.Equal(t, "cn-wlcb", extractInstanceRegion(map[string]any{}, "cn-wlcb"))
	assert.Equal(t, "cn-wlcb", extractInstanceRegion(map[string]any{
		"UHostSet": []any{},
	}, "cn-wlcb"))
	// First entry has neither Region nor Zone → fallback.
	assert.Equal(t, "cn-wlcb", extractInstanceRegion(map[string]any{
		"UHostSet": []any{map[string]any{"UHostId": "uhost-x"}},
	}, "cn-wlcb"))
}

// --- Integration: each mutating workflow must pair Region with Zone ---
//
// Mutation-style guard: if anyone deletes the `"Region": extractInstanceRegion(...)`
// line from a workflow's mutating step, the corresponding assertion below
// surfaces an empty / wrong Region. Audit cite: project-multi-region-audit-2026-05-25 B2.

func runMutatingWorkflowAndCaptureMutatingArgs(
	t *testing.T,
	def *Definition,
	describeResp map[string]any,
	mutatingAction string,
	params map[string]any,
) map[string]any {
	t.Helper()
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": describeResp,
		mutatingAction:              {"RetCode": 0},
	}}
	confirmFn := func(string, map[string]any) bool { return true }
	onStep, _ := collectEvents()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, params)
	assert.NoError(t, err)
	assert.True(t, result.Success, "workflow %s should succeed; message=%q", def.Name, result.Message)
	for _, call := range executor.calls {
		if call.action == mutatingAction {
			return call.args
		}
	}
	t.Fatalf("workflow %s never called mutating action %s", def.Name, mutatingAction)
	return nil
}

func describeRespWithZone(uhostId, zone string) map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":    uhostId,
			"Name":       "test-instance",
			"State":      "Running",
			"Zone":       zone,
			"GpuType":    "4090",
			"GPU":        float64(1),
			"ChargeType": "Dynamic",
		},
	}}
}

func TestStartInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, StartInstanceDef(),
		map[string]any{"UHostSet": []any{
			// Match startMockExecutor: state must be Stopped.
			map[string]any{"UHostId": "uhost-x", "State": "Stopped", "Zone": "cn-sh2-02"},
		}},
		"StartCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-sh2-02", args["Zone"])
	assert.Equal(t, "cn-sh2", args["Region"])
}

func TestStopInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, StopInstanceDef(),
		describeRespWithZone("uhost-x", "cn-bj2-04"),
		"StopCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-bj2-04", args["Zone"])
	assert.Equal(t, "cn-bj2", args["Region"])
}

func TestRebootInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, RebootInstanceDef(),
		describeRespWithZone("uhost-x", "cn-gd-01a"),
		"RebootCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-gd-01a", args["Zone"])
	assert.Equal(t, "cn-gd", args["Region"])
}

func TestRenameInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, RenameInstanceDef(),
		describeRespWithZone("uhost-x", "cn-sh2-02"),
		"ModifyCompShareInstanceName",
		map[string]any{"UHostId": "uhost-x", "Name": "new-name"})
	assert.Equal(t, "cn-sh2-02", args["Zone"])
	assert.Equal(t, "cn-sh2", args["Region"])
}

func TestExtractInstanceRegion_PrefersResponseRegionOverDerived(t *testing.T) {
	// End-to-end: when DescribeCompShareInstance returns an explicit Region
	// field, the workflow must use it as-is (don't override with regionFromZone).
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, StopInstanceDef(),
		map[string]any{"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-x",
				"Name":       "test",
				"State":      "Running",
				"Region":     "cn-sh2",
				"Zone":       "cn-bj2-04", // would derive cn-bj2 — but Region field wins
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"StopCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-sh2", args["Region"])
}

func TestResetPassword_SetsRegion(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":      "uhost-x",
				"Name":         "vm",
				"State":        "Stopped",
				"InstanceType": "Normal",
				"Zone":         "cn-gd-01a",
				"GpuType":      "A100",
				"GPU":          float64(1),
				"ChargeType":   "Month",
			},
		}},
		"ResetCompShareInstancePassword": {"UHostId": "uhost-x", "RetCode": float64(0)},
	}}
	confirmFn := func(string, map[string]any) bool { return true }
	onStep, _ := collectEvents()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), ResetPasswordDef(), map[string]any{
		"UHostId":  "uhost-x",
		"Password": "SecureP@ss1",
	})
	assert.NoError(t, err)
	assert.True(t, result.Success, "ResetPassword should succeed; got %q", result.Message)

	for _, call := range executor.calls {
		if call.action == "ResetCompShareInstancePassword" {
			assert.Equal(t, "cn-gd-01a", call.args["Zone"])
			assert.Equal(t, "cn-gd", call.args["Region"])
			return
		}
	}
	t.Fatalf("ResetCompShareInstancePassword was never called")
}

func TestSetStopScheduler_SetsRegion(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-x",
				"Name":       "gpu",
				"State":      "Running",
				"Zone":       "cn-bj2-04",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
	confirmFn := func(string, map[string]any) bool { return true }
	onStep, _ := collectEvents()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), SetStopSchedulerDef(), map[string]any{
		"UHostId":      "uhost-x",
		"AfterMinutes": float64(60),
	})
	assert.NoError(t, err)
	assert.True(t, result.Success, "SetStopScheduler should succeed; got %q", result.Message)

	for _, call := range executor.calls {
		if call.action == "UpdateCompShareStopScheduler" {
			assert.Equal(t, "cn-bj2-04", call.args["Zone"])
			assert.Equal(t, "cn-bj2", call.args["Region"])
			return
		}
	}
	t.Fatalf("UpdateCompShareStopScheduler was never called")
}

// CreateInstanceWorkflow is intentionally excluded from PR-β Region wiring.
// The 4 read tools it invokes (DescribeAvailableCompShareInstanceTypes,
// CheckCompShareResourceCapacity, GetCompShareInstanceUserPrice,
// DescribeCompShareImages) live in internal/tools/registry.go with schemas
// that do not declare Region; SafeToolExecutor.filterSafeArgs drops any
// args["Region"] that the workflow tries to inject. Until that filter is
// resolved (registry schema extension or workflow-internal allowlist), the
// create path keeps falling back to cfg.Region in external.go. See task
// PR-β1 / project-multi-region-audit-2026-05-25 for follow-up.

func TestStopInstance_FallsBackToDefaultRegionWhenZoneMissing(t *testing.T) {
	// DescribeCompShareInstance returns neither Zone nor Region — workflow
	// falls back to defaultRegion (paired with defaultZone) rather than
	// emitting an empty Region that the upstream signer would reject.
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, StopInstanceDef(),
		map[string]any{"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-x",
				"Name":       "test",
				"State":      "Running",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"StopCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, defaultZone, args["Zone"])
	assert.Equal(t, defaultRegion, args["Region"])
}
