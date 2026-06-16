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

// CreateInstanceWorkflow create-path Region wiring (resolves the PR-β1 gap that a
// prior revision of this file documented as deferred). The create-path read tools'
// schemas in internal/tools/registry.go now declare Region, so
// SafeToolExecutor.filterSafeArgs keeps it; and each create-path step pairs Region
// with a non-default Zone via addZoneRegion. The upstream rejects a Zone without
// its Region (RetCode=230) for any zone but the default cn-wlcb-01 — live-verified
// 2026-06-16: cn-bj2-03 (华北一C) creates only with Region=cn-bj2.
// Cite: project-multi-region-audit-2026-05-25 PR-β1.

func TestAddZoneRegion(t *testing.T) {
	cases := []struct {
		zone, wantRegion string
		wantSet          bool
	}{
		{"cn-bj2-03", "cn-bj2", true},
		{"cn-sh2-02", "cn-sh2", true},
		{"cn-wlcb-01", "cn-wlcb", true},
		{"", "", false},        // no zone → no Region key (API uses its default region)
		{"cn-wlcb", "", false}, // a Region, not a Zone → don't fabricate
	}
	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			args := addZoneRegion(map[string]any{}, c.zone)
			r, ok := args["Region"]
			assert.Equal(t, c.wantSet, ok, "Region presence")
			if c.wantSet {
				assert.Equal(t, c.wantRegion, r)
			}
		})
	}
}

func TestCreateInstance_NonDefaultZone_PairsRegionWithZone(t *testing.T) {
	// Every create-path API call must carry the Region matching a non-default Zone,
	// or the upstream 230s ("Params [Zone] not available"). Guards against anyone
	// dropping addZoneRegion from a create step.
	executor := createMockExecutor()
	onStep, _ := collectEvents()
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, onStep)
	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-bj2-04",
	})
	assert.NoError(t, err)
	assert.True(t, result.Success, "message=%q", result.Message)

	region := map[string]any{}
	for _, call := range executor.calls {
		if r, ok := call.args["Region"]; ok {
			region[call.action] = r
		}
	}
	for _, action := range []string{
		"DescribeAvailableCompShareInstanceTypes",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstanceUserPrice",
		"CreateCompShareInstance",
	} {
		assert.Equal(t, "cn-bj2", region[action], "%s must pair Region=cn-bj2 with Zone=cn-bj2-04", action)
	}
}

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
