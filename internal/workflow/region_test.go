package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func supportZoneResult(zone, region string) map[string]any {
	return map[string]any{"ZoneInfo": []any{map[string]any{
		"Zone": zone, "Region": region, "ZoneId": float64(5001), "RegionId": float64(3001),
	}}}
}

func TestExtractRequiredInstanceLocationUsesLiveCatalog(t *testing.T) {
	result := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-x", "Zone": "cn-sh2-02"},
	}}
	region, zone, err := extractRequiredInstanceLocation(
		result, supportZoneResult("cn-sh2-02", "cn-sh2"))
	assert.NoError(t, err)
	assert.Equal(t, "cn-sh2", region)
	assert.Equal(t, "cn-sh2-02", zone)
}

func TestExtractRequiredInstanceLocationNeverGuessesRegion(t *testing.T) {
	result := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-x", "Zone": "cn-sh2-02"},
	}}
	_, _, err := extractRequiredInstanceLocation(result, map[string]any{"ZoneInfo": []any{}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "真实地域")
}

func TestExtractRequiredInstanceLocationRejectsCatalogConflict(t *testing.T) {
	result := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-x", "Region": "cn-sh2", "Zone": "cn-bj2-04"},
	}}
	_, _, err := extractRequiredInstanceLocation(
		result, supportZoneResult("cn-bj2-04", "cn-bj2"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "不一致")
}

// --- Integration: each mutating workflow must pair the instance Zone with the
// Region returned by the live support-zone catalog. ---
//
// DescribeCompShareInstance is the primary source. Workflows that already query
// the support-zone catalog may use its matching row only when Region is absent.

func runMutatingWorkflowAndCaptureMutatingArgs(
	t *testing.T,
	def *Definition,
	describeResp map[string]any,
	supportZones map[string]any,
	mutatingAction string,
	params map[string]any,
) map[string]any {
	t.Helper()
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    describeResp,
		"DescribeCompShareSupportZone": supportZones,
		mutatingAction:                 {"RetCode": 0},
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
			map[string]any{"UHostId": "uhost-x", "State": "Stopped", "Region": "cn-sh2", "Zone": "cn-sh2-02"},
		}},
		supportZoneResult("cn-sh2-02", "cn-sh2"),
		"StartCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-sh2-02", args["Zone"])
	assert.Equal(t, "cn-sh2", args["Region"])
}

func TestStopInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, StopInstanceDef(),
		describeRespWithZone("uhost-x", "cn-bj2-04"),
		supportZoneResult("cn-bj2-04", "cn-bj2"),
		"StopCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-bj2-04", args["Zone"])
	assert.Equal(t, "cn-bj2", args["Region"])
}

func TestRebootInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, RebootInstanceDef(),
		describeRespWithZone("uhost-x", "cn-gd-01a"),
		supportZoneResult("cn-gd-01a", "cn-gd"),
		"RebootCompShareInstance",
		map[string]any{"UHostId": "uhost-x"})
	assert.Equal(t, "cn-gd-01a", args["Zone"])
	assert.Equal(t, "cn-gd", args["Region"])
}

func TestRenameInstance_SetsRegion(t *testing.T) {
	args := runMutatingWorkflowAndCaptureMutatingArgs(t, RenameInstanceDef(),
		describeRespWithZone("uhost-x", "cn-sh2-02"),
		supportZoneResult("cn-sh2-02", "cn-sh2"),
		"ModifyCompShareInstanceName",
		map[string]any{"UHostId": "uhost-x", "Name": "new-name"})
	assert.Equal(t, "cn-sh2-02", args["Zone"])
	assert.Equal(t, "cn-sh2", args["Region"])
}

func TestMutatingWorkflowRejectsResponseCatalogLocationConflict(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-x",
				"Name":       "test",
				"State":      "Running",
				"Region":     "cn-sh2",
				"Zone":       "cn-bj2-04",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"DescribeCompShareSupportZone": supportZoneResult("cn-bj2-04", "cn-bj2"),
		"StopCompShareInstance":        {"RetCode": 0},
	}}
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)
	result, err := eng.Run(context.Background(), StopInstanceDef(), map[string]any{"UHostId": "uhost-x"})
	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "不一致")
	for _, call := range executor.calls {
		assert.NotEqual(t, "StopCompShareInstance", call.action)
	}
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
		"DescribeCompShareSupportZone":   supportZoneResult("cn-gd-01a", "cn-gd"),
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
				"Region":     "cn-bj2",
				"Zone":       "cn-bj2-04",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"DescribeCompShareSupportZone": supportZoneResult("cn-bj2-04", "cn-bj2"),
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
	confirmFn := func(string, map[string]any) bool { return true }
	onStep, _ := collectEvents()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), SetStopSchedulerDef(), map[string]any{
		"UHostId":  "uhost-x",
		"Schedule": map[string]any{"mode": "after_minutes", "minutes": float64(60)},
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
// with a non-default Zone via addZoneRegionAndID, sourced from the turn's zone
// catalog snapshot. The upstream rejects a Zone without its Region (RetCode=230)
// for any zone but the default cn-wlcb-01 — live-verified 2026-06-16: cn-bj2-03
// (华北一C) creates only with Region=cn-bj2.
// Cite: project-multi-region-audit-2026-05-25 PR-β1.

func TestCreateInstance_NonDefaultZone_PairsRegionWithZone(t *testing.T) {
	// Every create-path API call must carry the Region matching a non-default Zone,
	// or the upstream 230s ("Params [Zone] not available"). Guards against anyone
	// dropping addZoneRegion from a create step.
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = mockInstanceTypesInZone("cn-bj2-04", "4090",
		struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
	)
	onStep, _ := collectEvents()
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, onStep)
	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-bj2-04",
	}, withNormalZone("cn-bj2-04", "cn-bj2", 6004))
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

func TestMutatingWorkflows_RejectMissingLocationBeforeExecute(t *testing.T) {
	cases := []struct {
		name           string
		def            *Definition
		params         map[string]any
		mutatingAction string
		describeResult map[string]any
	}{
		{
			name:           "stop",
			def:            StopInstanceDef(),
			params:         map[string]any{"UHostId": "uhost-x"},
			mutatingAction: "StopCompShareInstance",
			describeResult: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-x", "Name": "test", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			}}},
		},
		{
			name:           "reboot",
			def:            RebootInstanceDef(),
			params:         map[string]any{"UHostId": "uhost-x"},
			mutatingAction: "RebootCompShareInstance",
			describeResult: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-x", "Name": "test", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			}}},
		},
		{
			name:           "rename",
			def:            RenameInstanceDef(),
			params:         map[string]any{"UHostId": "uhost-x", "Name": "new-name"},
			mutatingAction: "ModifyCompShareInstanceName",
			describeResult: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-x", "Name": "test", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			}}},
		},
		{
			name:           "reset_password",
			def:            ResetPasswordDef(),
			params:         map[string]any{"UHostId": "uhost-x", "Password": "SecureP@ss1"},
			mutatingAction: "ResetCompShareInstancePassword",
			describeResult: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-x", "Name": "test", "State": "Stopped", "InstanceType": "Normal", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			}}},
		},
		{
			name:           "create_disk",
			def:            CreateDiskDef(),
			params:         map[string]any{"UHostId": "uhost-x", "Size": float64(100)},
			mutatingAction: "CreateAndAttachCompshareDisk",
			describeResult: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-x", "Name": "test", "State": "Stopped", "InstanceType": "Normal", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			}}},
		},
		{
			name:           "reinstall",
			def:            ReinstallInstanceDef(),
			params:         map[string]any{"UHostId": "uhost-x", "CompShareImageId": "img-001"},
			mutatingAction: "ReinstallCompShareInstance",
			describeResult: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-x", "Name": "test", "State": "Stopped", "InstanceType": "Normal", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic",
			}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := map[string]map[string]any{
				"DescribeCompShareInstance": tc.describeResult,
				tc.mutatingAction:           {"RetCode": 0},
				"GetCompShareInstancePrice": {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
			}
			if tc.def.Name == "ReinstallInstanceWorkflow" {
				results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
					map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu"},
				}}
			}
			executor := &mockExecutor{results: results}
			onStep, _ := collectEvents()
			eng := NewEngine(executor, func(string, map[string]any) bool { return true }, onStep)

			result, err := eng.Run(context.Background(), tc.def, tc.params)

			assert.NoError(t, err)
			assert.False(t, result.Success, "workflow should reject missing location before execute")
			assert.Contains(t, result.Message, "可用区")
			for _, call := range executor.calls {
				assert.NotEqual(t, tc.mutatingAction, call.action, "must not call mutating API without real Zone/Region")
			}
		})
	}
}
