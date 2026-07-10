package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/zones"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type spyExecutor struct {
	calls  int
	args   []map[string]any
	result map[string]any
	errs   []error
}

func (s *spyExecutor) Execute(_ context.Context, _ string, args map[string]any) (map[string]any, error) {
	s.calls++
	s.args = append(s.args, args)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.result != nil {
		return s.result, nil
	}
	return map[string]any{"RetCode": float64(0)}, nil
}

func TestDefaultPoliciesCoverRegistryAndSecurityActions(t *testing.T) {
	policies := DefaultToolExecutionPolicies()

	for _, tool := range Registry {
		if tool.Function == nil {
			continue
		}
		action := tool.Function.Name
		_, ok := policies[action]
		assert.Truef(t, ok, "missing policy for registered tool %s", action)
	}
	for action := range security.ActionLevels {
		_, ok := policies[action]
		assert.Truef(t, ok, "missing policy for security action %s", action)
	}
}

func TestDefaultPoliciesClassifyReadExpensiveActionsExplicitly(t *testing.T) {
	policies := DefaultToolExecutionPolicies()

	cases := []struct {
		action string
		class  ActionClass
	}{
		{"DescribeCompShareInstance", ActionClassReadExpensiveDefault},
		{"GetCompShareInstanceMonitor", ActionClassReadExpensivePerTarget},
		{"GetCompShareInstancePrice", ActionClassReadExpensiveDefault},
		{"GetCompShareInstanceUserPrice", ActionClassReadExpensiveDefault},
		{"DescribeAvailableCompShareInstanceTypes", ActionClassReadExpensiveDefault},
		{"DescribeCompShareGpuInventory", ActionClassReadExpensiveDefault},
		{"CheckCompShareResourceCapacity", ActionClassReadExpensiveDefault},
		{"DiagnoseBilling", ActionClassReadExpensiveDefault},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			require.Contains(t, policies, tc.action)
			assert.Equal(t, tc.class, policies[tc.action].Class)
		})
	}

	policy := policyForAction("GetAccountPriceAdjustmentPreview")
	assert.Equal(t, ActionClassReadCheap, policy.Class, "unregistered price-looking actions must not become read-expensive by substring")
}

func TestDescribeCompShareImagesAllowsImageIDFilter(t *testing.T) {
	safe := NewSafeToolExecutor(&spyExecutor{})

	filtered := safe.FilterArgs("DescribeCompShareImages", map[string]any{
		"CompShareImageId": "img-001",
		"Name":             "Ubuntu",
		"Unexpected":       "drop-me",
	})

	assert.Equal(t, "img-001", filtered["CompShareImageId"])
	assert.Equal(t, "Ubuntu", filtered["Name"])
	assert.NotContains(t, filtered, "Unexpected")
}

func TestDescribeCompShareImagesAllowsOffsetForPagination(t *testing.T) {
	safe := NewSafeToolExecutor(&spyExecutor{})

	filtered := safe.FilterArgs("DescribeCompShareImages", map[string]any{
		"Limit":  100,
		"Offset": 100,
	})

	assert.Equal(t, 100, filtered["Limit"])
	assert.Equal(t, 100, filtered["Offset"])
}

func TestReinstallWorkflowAllowsImageNameForLookup(t *testing.T) {
	safe := NewSafeToolExecutor(&spyExecutor{})

	filtered := safe.FilterArgs("ReinstallInstanceWorkflow", map[string]any{
		"UHostId":          "uhost-1",
		"ImageName":        "Ubuntu-nvidia 22.04",
		"ImageSource":      "platform",
		"CompShareImageId": "img-001",
		"az_group":         uint32(3001),
	})

	assert.Equal(t, "uhost-1", filtered["UHostId"])
	assert.Equal(t, "Ubuntu-nvidia 22.04", filtered["ImageName"])
	assert.Equal(t, "platform", filtered["ImageSource"])
	assert.Equal(t, "img-001", filtered["CompShareImageId"])
	assert.NotContains(t, filtered, "az_group")
}

func TestBackendZoneIDIsInternalOnly(t *testing.T) {
	directInner := &spyExecutor{}
	direct := NewSafeToolExecutor(directInner)
	_, err := direct.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "CheckCompShareResourceCapacity",
		Args: map[string]any{
			"Zone":               "cn-pod-01",
			"Region":             "cn-pod",
			"zone_id":            uint32(9001),
			"GpuType":            "4090",
			"MachineType":        "G",
			"MinimalCpuPlatform": "Auto",
			"CompShareImageId":   "img-container",
			"ChargeType":         "Postpay",
			"Disks":              []any{map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": float64(60)}},
		},
		Origin: OriginDirectLLM,
	})
	require.NoError(t, err)
	require.Len(t, directInner.args, 1)
	assert.NotContains(t, directInner.args[0], "zone_id", "model-origin calls must not be able to hand-fill zone_id")

	internalInner := &spyExecutor{}
	internal := NewSafeToolExecutor(internalInner)
	_, err = internal.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "CheckCompShareResourceCapacity",
		Args: map[string]any{
			"Zone":               "cn-pod-01",
			"Region":             "cn-pod",
			"zone_id":            uint32(9001),
			"GpuType":            "4090",
			"MachineType":        "G",
			"MinimalCpuPlatform": "Auto",
			"CompShareImageId":   "img-container",
			"ChargeType":         "Postpay",
			"Disks":              []any{map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": float64(60)}},
		},
		Origin: OriginWorkflowInternal,
	})
	require.NoError(t, err)
	require.Len(t, internalInner.args, 1)
	assert.Equal(t, uint32(9001), internalInner.args[0]["zone_id"], "workflow-derived zone_id must reach upstream")
}

func TestBackendPlacementAndIdentityFieldsAreInternalOnlyForCFSAndNetwork(t *testing.T) {
	cases := []struct {
		action string
		args   map[string]any
	}{
		{
			action: "GetCompShareCFSPrice",
			args: map[string]any{
				"Size":                50,
				"Zone":                "cn-bj2-03",
				"ChargeType":          "Month",
				"Quantity":            1,
				"zone_id":             uint32(5001),
				"az_group":            uint32(3003),
				"top_organization_id": uint32(1001),
				"organization_id":     uint32(1002),
			},
		},
		{
			action: "CheckCompShareNetOptimizer",
			args: map[string]any{
				"az_group":            uint32(3003),
				"top_organization_id": uint32(1001),
				"organization_id":     uint32(1002),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			directInner := &spyExecutor{}
			direct := NewSafeToolExecutor(directInner)
			_, err := direct.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: tc.action,
				Args:   tc.args,
				Origin: OriginDirectLLM,
			})
			require.NoError(t, err)
			require.Len(t, directInner.args, 1)
			for _, forbidden := range []string{"zone_id", "az_group", "top_organization_id", "organization_id"} {
				assert.NotContains(t, directInner.args[0], forbidden, "model-origin calls must not hand-fill internal upstream fields")
			}

			internalInner := &spyExecutor{}
			internal := NewSafeToolExecutor(internalInner)
			_, err = internal.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: tc.action,
				Args:   tc.args,
				Origin: OriginDiagnosisInternal,
			})
			require.NoError(t, err)
			require.Len(t, internalInner.args, 1)
			for _, want := range []string{"top_organization_id", "organization_id"} {
				assert.Equal(t, tc.args[want], internalInner.args[0][want], "backend-injected identity must survive internal calls")
			}
			switch tc.action {
			case "GetCompShareCFSPrice":
				assert.Equal(t, tc.args["zone_id"], internalInner.args[0]["zone_id"])
				assert.Equal(t, tc.args["az_group"], internalInner.args[0]["az_group"])
			case "CheckCompShareNetOptimizer":
				assert.Equal(t, tc.args["az_group"], internalInner.args[0]["az_group"])
			}
		})
	}
}

func TestVisibleRegistryFiltersMutatingWorkflowsByDefault(t *testing.T) {
	visible := VisibleRegistry(false)
	names := map[string]bool{}
	for _, tool := range visible {
		require.NotNil(t, tool.Function)
		names[tool.Function.Name] = true
		assert.False(t, strings.HasSuffix(tool.Function.Name, "Workflow"), "workflow tool %s must not be visible in read-only mode", tool.Function.Name)
	}

	for _, name := range []string{
		"DescribeCompShareInstance",
		"GetCompShareInstanceMonitor",
		"DiagnoseSSH",
		"DiagnoseBilling",
		"GetGPUSpecs",
	} {
		assert.True(t, names[name], "read-only/diagnosis tool %s should remain visible", name)
	}
	for _, name := range []string{
		"CreateInstanceWorkflow",
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"RebootInstanceWorkflow",
		"RenameInstanceWorkflow",
		"ResetPasswordWorkflow",
		"SetStopSchedulerWorkflow",
		"CancelStopSchedulerWorkflow",
		"CreateCustomImageWorkflow",
		"ResizeDiskWorkflow",
	} {
		assert.False(t, names[name], "mutating workflow %s should be hidden by default", name)
	}

	all := VisibleRegistry(true)
	allNames := map[string]bool{}
	for _, tool := range all {
		require.NotNil(t, tool.Function)
		allNames[tool.Function.Name] = true
	}
	assert.True(t, allNames["StopInstanceWorkflow"])
	assert.True(t, allNames["CreateCustomImageWorkflow"])
	// SearchKnowledge (agentic-RAG, P3) is gated behind
	// COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE: absent by default even in mutating
	// mode, so the visible mutating set is the full registry minus that one
	// gated tool. With the flag on it would equal len(Registry) (see
	// TestSearchKnowledgeGatedVisibility).
	assert.False(t, allNames["SearchKnowledge"], "SearchKnowledge is gated off by default")
	assert.Equal(t, len(Registry)-1, len(all))
}

func TestDefaultPoliciesAttachMonitorCaps(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	policy := policies["GetCompShareInstanceMonitor"]

	assert.Equal(t, 20, policy.MaxTargetsPerCall)
	assert.Equal(t, 86400, policy.MaxHistoryWindowSeconds)
	assert.Equal(t, 20, policies["GetCompShareInstancePrice"].MaxTargetsPerCall)
	assert.Equal(t, 20, policies["GetCompShareInstanceUserPrice"].MaxTargetsPerCall)
}

func TestSafeExecutorRejectsMissingPolicy(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner, WithPolicies(map[string]ToolExecutionPolicy{}))

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{Action: "UnknownAction"})

	require.ErrorIs(t, err, ErrPolicyMissing)
	assert.Equal(t, 0, inner.calls)
}

func TestSafeExecutorDoesNotSendMetaToolsToInnerExecutor(t *testing.T) {
	for _, action := range []string{"GetGPUSpecs", "DiagnoseSSH", "StartInstanceWorkflow"} {
		t.Run(action, func(t *testing.T) {
			inner := &spyExecutor{}
			safe := NewSafeToolExecutor(inner)

			_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: action,
				Args:   map[string]any{},
				Origin: OriginDirectLLM,
			})

			require.ErrorIs(t, err, ErrNonExternalAction)
			assert.Equal(t, 0, inner.calls)
		})
	}
}

func TestSafeExecutorRejectsMonitorTargetCapBeforeCallingInner(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner)
	ids := make([]any, 21)
	for i := range ids {
		ids[i] = fmt.Sprintf("uhost-%02d", i)
	}

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "GetCompShareInstanceMonitor",
		Args:   map[string]any{"UHostIds": ids},
		Origin: OriginDirectLLM,
	})

	require.ErrorIs(t, err, ErrToolCapExceeded)
	assert.Equal(t, 0, inner.calls)
}

func TestSafeExecutorRejectsMonitorHistoryWindowCapBeforeCallingInner(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "over 24h json number window",
			args: map[string]any{
				"UHostIds":  []any{"uhost-1"},
				"StartTime": json.Number("1777471200"),
				"EndTime":   json.Number("1777557601"),
			},
		},
		{
			name: "extreme parseable int64 timestamps cannot overflow around cap",
			args: map[string]any{
				"UHostIds":  []any{"uhost-1"},
				"StartTime": json.Number("-9223372036854775808"),
				"EndTime":   json.Number("9223372036854775807"),
			},
		},
		{
			name: "production json float64 timestamps cannot overflow around cap",
			args: mustUnmarshalArgs(t, `{"UHostIds":["uhost-1"],"StartTime":0,"EndTime":1e20}`),
		},
		{
			name: "historical window is single target only",
			args: mustUnmarshalArgs(t, `{"UHostIds":["uhost-1","uhost-2"],"StartTime":1777471200,"EndTime":1777474800}`),
		},
		{
			name: "historical window requires a target",
			args: mustUnmarshalArgs(t, `{"StartTime":1777471200,"EndTime":1777474800}`),
		},
		{
			name: "start time without end time is still historical monitor",
			args: mustUnmarshalArgs(t, `{"UHostIds":["uhost-1"],"StartTime":1777471200}`),
		},
		{
			name: "end time without start time is still historical monitor",
			args: mustUnmarshalArgs(t, `{"UHostIds":["uhost-1"],"EndTime":1777474800}`),
		},
		{
			name: "invalid end before start is still historical monitor",
			args: mustUnmarshalArgs(t, `{"UHostIds":["uhost-1"],"StartTime":1777474800,"EndTime":1777471200}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &spyExecutor{}
			safe := NewSafeToolExecutor(inner)

			_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: "GetCompShareInstanceMonitor",
				Args:   tc.args,
				Origin: OriginDirectLLM,
			})

			require.ErrorIs(t, err, ErrHistoryWindowExceeded)
			assert.Equal(t, 0, inner.calls)
		})
	}
}

func TestSafeExecutorAllowsSingleTargetMonitorHistoryWindow(t *testing.T) {
	inner := &spyExecutor{result: map[string]any{"RetCode": 0}}
	safe := NewSafeToolExecutor(inner)

	result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "GetCompShareInstanceMonitor",
		Args:   mustUnmarshalArgs(t, `{"UHostIds":["uhost-1"],"StartTime":1777471200,"EndTime":1777474800}`),
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, inner.calls)
}

func mustUnmarshalArgs(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

func TestSafeExecutorDoesNotRetryCapErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "tool cap", err: ErrToolCapExceeded},
		{name: "history window cap", err: ErrHistoryWindowExceeded},
		{name: "historical monitor unsupported", err: ErrHistoricalMonitorUnsupported},
		{name: "rate limit", err: governance.ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &spyExecutor{errs: []error{tc.err, nil}}
			policies := DefaultToolExecutionPolicies()
			policy := policies["DescribeCompShareInstance"]
			policy.MaxTargetsPerCall = 1
			policies["DescribeCompShareInstance"] = policy
			safe := NewSafeToolExecutor(inner, WithPolicies(policies))

			_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: "DescribeCompShareInstance",
				Args:   map[string]any{"UHostIds": []any{"uhost-1"}},
				Origin: OriginDirectLLM,
			})

			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, 1, inner.calls)
		})
	}
}

func TestSafeExecutorFiltersArgsAndRedactsResult(t *testing.T) {
	inner := &spyExecutor{result: map[string]any{
		"DataSet": []any{map[string]any{
			"JupyterToken": "raw-jupyter-token",
			"Softwares": []any{map[string]any{
				"Name": "JupyterLab",
				"URL":  "http://1.2.3.4:8888?token=UCloud-CompShare-AbCd1234",
			}},
		}},
		"Nested": map[string]any{"Password": "raw-password"},
	}}
	safe := NewSafeToolExecutor(inner)

	result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args: map[string]any{
			"UHostIds":       []any{"uhost-1"},
			"InjectedParam":  "drop-me",
			"AnotherUnknown": true,
		},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)
	assert.Equal(t, map[string]any{"UHostIds": []any{"uhost-1"}}, inner.args[0])

	dataSet, ok := result.LLMResult["DataSet"].([]any)
	require.True(t, ok)
	first, ok := dataSet[0].(map[string]any)
	require.True(t, ok)
	assert.NotEqual(t, "raw-jupyter-token", first["JupyterToken"])
	softwares, ok := first["Softwares"].([]any)
	require.True(t, ok)
	software, ok := softwares[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "http://1.2.3.4:8888?token=[REDACTED]", software["URL"])

	nested, ok := result.LLMResult["Nested"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "[REDACTED]", nested["Password"])
}

func TestSafeExecutorUsesPolicyForDisplayAndRedaction(t *testing.T) {
	t.Run("extra redaction fields are policy controlled", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{"OneTimeCode": "visible-without-policy"}}
		policies := DefaultToolExecutionPolicies()
		policy := policies["DescribeCompShareInstance"]
		policy.RedactInResult = []string{"OneTimeCode"}
		policies["DescribeCompShareInstance"] = policy
		safe := NewSafeToolExecutor(inner, WithPolicies(policies))

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "DescribeCompShareInstance",
			Args:   map[string]any{"Limit": 1},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		assert.Equal(t, "[REDACTED]", result.LLMResult["OneTimeCode"])
		assert.Equal(t, "[REDACTED]", result.TraceResult["OneTimeCode"])
	})

	t.Run("jupyter token action is explicitly redacted", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{"JupyterToken": "raw-jupyter-token"}}
		safe := NewSafeToolExecutor(inner)

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "DescribeCompShareJupyterToken",
			Args:   map[string]any{"UHostIds": []any{"uhost-1"}},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		assert.Equal(t, "[REDACTED]", result.LLMResult["JupyterToken"])
		assert.Equal(t, "[REDACTED]", result.TraceResult["JupyterToken"])
	})
}

func TestMonitorHistoryGuardMarksNoDataWindow(t *testing.T) {
	raw := map[string]any{
		"Data": []any{
			map[string]any{"UHostId": "uhost-1", "MonitorSet": []any{}},
		},
	}

	result := applyHistoryGuard(DefaultToolExecutionPolicies()["GetCompShareInstanceMonitor"], map[string]any{
		"UHostIds":  []any{"uhost-1"},
		"StartTime": float64(1777471200),
		"EndTime":   float64(1777474800),
	}, raw)

	assert.Equal(t, "NO_DATA_IN_REQUESTED_WINDOW", result["MonitorDataStatus"])
	assert.NotEmpty(t, result["MonitorDataGuidance"])
}

func TestMonitorHistoryGuardDoesNotMarkSamplesOrRealtime(t *testing.T) {
	t.Run("historical samples", func(t *testing.T) {
		raw := map[string]any{
			"Data": []any{
				map[string]any{
					"UHostId": "uhost-1",
					"Metrics": []any{
						map[string]any{"Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(42)}}}}},
					},
				},
			},
		}

		result := applyHistoryGuard(DefaultToolExecutionPolicies()["GetCompShareInstanceMonitor"], map[string]any{
			"UHostIds":  []any{"uhost-1"},
			"StartTime": float64(1777471200),
			"EndTime":   float64(1777474800),
		}, raw)

		assert.NotContains(t, result, "MonitorDataStatus")
	})

	t.Run("realtime snapshot", func(t *testing.T) {
		result := applyHistoryGuard(DefaultToolExecutionPolicies()["GetCompShareInstanceMonitor"], map[string]any{
			"UHostIds": []any{"uhost-1", "uhost-2"},
		}, map[string]any{"Data": []any{}})

		assert.NotContains(t, result, "MonitorDataStatus")
	})
}

func TestSafeExecutorDirectL1RequiresConfirmation(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner, WithConfirmFunc(func(string, map[string]any) bool { return false }))

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "StartCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-1"},
		Origin: OriginDirectLLM,
	})

	require.ErrorIs(t, err, ErrUserDeclined)
	assert.Equal(t, 0, inner.calls)

	safe = NewSafeToolExecutor(inner, WithConfirmFunc(func(string, map[string]any) bool { return true }))
	_, err = safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "StartCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-1"},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	assert.Equal(t, 1, inner.calls)
}

func TestSafeExecutorUnknownOriginRequiresConfirmation(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner)

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "StartCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-1"},
		Origin: ExecutionOrigin("future_origin"),
	})

	require.ErrorIs(t, err, ErrUserDeclined)
	assert.Equal(t, 0, inner.calls)
}

func TestSafeExecutorWorkflowOriginSkipsPerAPIL1Confirmation(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner, WithConfirmFunc(func(string, map[string]any) bool {
		t.Fatal("workflow-internal calls must not trigger per-API confirmation")
		return false
	}))

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "StartCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-1"},
		Origin: OriginWorkflowInternal,
	})

	require.NoError(t, err)
	assert.Equal(t, 1, inner.calls)
}

func TestSafeExecutorWorkflowCreateCFSPreservesInternalArgs(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner, WithConfirmFunc(func(string, map[string]any) bool {
		t.Fatal("workflow-internal CreateCFS must not trigger per-API confirmation")
		return false
	}))

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "CreateCFS",
		Args: map[string]any{
			"Name":                "shared-train",
			"Size":                float64(50),
			"ChargeType":          "Month",
			"Quantity":            float64(1),
			"Zone":                "cn-bj2-03",
			"Region":              "cn-bj2",
			"zone_id":             uint32(5001),
			"az_group":            uint32(9999),
			"top_organization_id": uint32(101),
			"organization_id":     uint32(202),
		},
		Origin: OriginWorkflowInternal,
	})

	require.NoError(t, err)
	require.Len(t, inner.args, 1)
	got := inner.args[0]
	assert.Equal(t, "shared-train", got["Name"])
	assert.Equal(t, float64(50), got["Size"])
	assert.Equal(t, "Month", got["ChargeType"])
	assert.Equal(t, "cn-bj2-03", got["Zone"])
	assert.Equal(t, uint32(5001), got["zone_id"])
	assert.Equal(t, uint32(9999), got["az_group"])
	assert.Equal(t, uint32(101), got["top_organization_id"])
	assert.Equal(t, uint32(202), got["organization_id"])
}

func TestSafeExecutorWorkflowResizePreservesInternalPlacement(t *testing.T) {
	cases := []struct {
		action string
		args   map[string]any
	}{
		{
			action: "GetCompShareInstancePrice",
			args: map[string]any{
				"GpuType":    "4090",
				"Gpu":        float64(1),
				"Cpu":        float64(16),
				"Memory":     float64(65536),
				"ChargeType": "Postpay",
				"Region":     "cn-sh2",
				"zone_id":    uint32(9001),
				"az_group":   uint32(3001),
			},
		},
		{
			action: "GetCompShareInstanceUserPrice",
			args: map[string]any{
				"GpuType":  "4090",
				"GPU":      float64(1),
				"CPU":      float64(16),
				"Memory":   float64(65536),
				"Region":   "cn-sh2",
				"zone_id":  uint32(9001),
				"az_group": uint32(3001),
			},
		},
		{
			action: "GetCompShareInstanceUpgradePrice",
			args: map[string]any{
				"UHostId":  "uhost-test",
				"Zone":     "cn-sh2-02",
				"Region":   "cn-sh2",
				"GPU":      float64(2),
				"zone_id":  uint32(9001),
				"az_group": uint32(3001),
			},
		},
		{
			action: "GetCompShareAttachedDiskUpgradePrice",
			args: map[string]any{
				"UHostId":   "cpod-test",
				"DiskId":    "cvolume-boot",
				"DiskSpace": float64(120),
				"Zone":      "cn-pod-01",
				"Region":    "cn-pod",
				"zone_id":   uint32(9001),
				"az_group":  uint32(3001),
			},
		},
		{
			action: "ResizeCompShareInstance",
			args: map[string]any{
				"UHostId":   "cpod-test",
				"DiskId":    "cvolume-boot",
				"DiskSpace": float64(120),
				"Zone":      "cn-pod-01",
				"Region":    "cn-pod",
				"zone_id":   uint32(9001),
				"az_group":  uint32(3001),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			directInner := &spyExecutor{}
			direct := NewSafeToolExecutor(directInner, WithConfirmFunc(func(string, map[string]any) bool {
				return true
			}))
			_, err := direct.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: tc.action,
				Args:   tc.args,
				Origin: OriginDirectLLM,
			})
			require.NoError(t, err)
			require.Len(t, directInner.args, 1)
			assert.NotContains(t, directInner.args[0], "zone_id")
			assert.NotContains(t, directInner.args[0], "az_group")

			internalInner := &spyExecutor{}
			internal := NewSafeToolExecutor(internalInner, WithConfirmFunc(func(string, map[string]any) bool {
				t.Fatal("workflow-internal resize call must not trigger per-API confirmation")
				return false
			}))
			_, err = internal.ExecuteSafe(context.Background(), SafeToolRequest{
				Action: tc.action,
				Args:   tc.args,
				Origin: OriginWorkflowInternal,
			})
			require.NoError(t, err)
			require.Len(t, internalInner.args, 1)
			assert.Equal(t, tc.args["zone_id"], internalInner.args[0]["zone_id"])
			assert.Equal(t, tc.args["az_group"], internalInner.args[0]["az_group"])
		})
	}
}

func TestSafeExecutorOriginViewImplementsToolExecutor(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner, WithConfirmFunc(func(string, map[string]any) bool {
		t.Fatal("origin view should carry workflow origin and skip per-API confirmation")
		return false
	}))
	var exec ToolExecutor = safe.AsToolExecutor(OriginWorkflowInternal)

	_, err := exec.Execute(context.Background(), "StartCompShareInstance", map[string]any{"UHostId": "uhost-1"})

	require.NoError(t, err)
	assert.Equal(t, 1, inner.calls)
}

func TestSafeExecutorRejectsDestructiveActions(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner)

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "TerminateCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-1"},
		Origin: OriginDirectLLM,
	})

	require.ErrorIs(t, err, ErrDestructiveAction)
	assert.Equal(t, 0, inner.calls)
}

func TestSafeExecutorCanDisableMutatingActions(t *testing.T) {
	inner := &spyExecutor{}
	safe := NewSafeToolExecutor(inner, WithMutatingToolsEnabled(false))

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "StartCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-1"},
		Origin: OriginDirectLLM,
	})

	require.ErrorIs(t, err, ErrMutatingActionDisabled)
	assert.Equal(t, 0, inner.calls)
}

func TestSafeExecutorRetriesReadNetworkErrorsOnly(t *testing.T) {
	networkErr := &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}
	inner := &spyExecutor{errs: []error{networkErr, nil}}
	safe := NewSafeToolExecutor(inner)

	result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 1},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, inner.calls)
	assert.Equal(t, 2, result.Attempts)
}

func TestSafeExecutorDoesNotRetry4xxOrMutatingNetworkErrors(t *testing.T) {
	t.Run("4xx read error", func(t *testing.T) {
		inner := &spyExecutor{errs: []error{fmt.Errorf("status code: 400 bad request")}}
		safe := NewSafeToolExecutor(inner)

		_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "DescribeCompShareInstance",
			Args:   map[string]any{"Limit": 1},
			Origin: OriginDirectLLM,
		})

		require.Error(t, err)
		assert.Equal(t, 1, inner.calls)
	})

	t.Run("mutating eof", func(t *testing.T) {
		inner := &spyExecutor{errs: []error{io.EOF}}
		safe := NewSafeToolExecutor(inner, WithConfirmFunc(func(string, map[string]any) bool { return true }))

		_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "StartCompShareInstance",
			Args:   map[string]any{"UHostId": "uhost-1"},
			Origin: OriginDirectLLM,
		})

		require.Error(t, err)
		assert.True(t, errors.Is(err, io.EOF))
		assert.Equal(t, 1, inner.calls)
	})
}

// TestPolicyDefaults_TimeoutsAndBackoffByClass locks the per-class
// TimeoutMS + BackoffBaseMS contract introduced in PR #5. If a future
// change shifts these defaults the test fails loudly with expected vs
// actual values so the change is reviewed.
func TestPolicyDefaults_TimeoutsAndBackoffByClass(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	cases := []struct {
		action      string
		wantClass   ActionClass
		wantTimeout int
		wantBackoff int
		wantRetries int
	}{
		// read_cheap: cheap describes, gpu specs lookup. The timeout still
		// budgets for a cold STS AssumeRole before the business API call.
		{"DescribeCompShareImages", ActionClassReadCheap, 15000, 300, 1},
		// read_expensive_default: per-instance describes, price calls.
		{"DescribeCompShareInstance", ActionClassReadExpensiveDefault, 15000, 500, 1},
		{"GetCompShareInstancePrice", ActionClassReadExpensiveDefault, 15000, 500, 1},
		// read_expensive_per_target: monitor (bulk).
		{"GetCompShareInstanceMonitor", ActionClassReadExpensivePerTarget, 30000, 500, 1},
		// mutating: L1 lifecycle.
		{"StartCompShareInstance", ActionClassMutating, 30000, 0, 0},
		// destructive: L2 — terminate.
		{"TerminateCompShareInstance", ActionClassDestructive, 30000, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			p, ok := policies[tc.action]
			require.True(t, ok, "policy missing for %s", tc.action)
			assert.Equal(t, tc.wantClass, p.Class, "class")
			assert.Equal(t, tc.wantTimeout, p.TimeoutMS, "TimeoutMS")
			assert.Equal(t, tc.wantBackoff, p.BackoffBaseMS, "BackoffBaseMS")
			assert.Equal(t, tc.wantRetries, p.MaxRetries, "MaxRetries")
		})
	}
}

// slowExecutor blocks until ctx is cancelled, then returns ctx.Err. Used
// to drive the per-attempt-timeout enforcement test.
type slowExecutor struct {
	calls int
}

func (e *slowExecutor) Execute(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
	e.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSafeExecutor_AppliesPerAttemptTimeout verifies policy.TimeoutMS is
// enforced via context.WithTimeout per attempt — a hung backend cannot
// outlast the policy budget. Overrides TimeoutMS to 50ms so the test
// completes in <1s. With MaxRetries=1 + BackoffBaseMS=0, the slow
// executor should hit ctx.DeadlineExceeded twice (initial + 1 retry).
func TestSafeExecutor_AppliesPerAttemptTimeout(t *testing.T) {
	inner := &slowExecutor{}
	policies := DefaultToolExecutionPolicies()
	p := policies["DescribeCompShareInstance"]
	p.TimeoutMS = 50    // tight per-attempt budget
	p.BackoffBaseMS = 0 // remove backoff for test speed
	policies["DescribeCompShareInstance"] = p
	safe := NewSafeToolExecutor(inner, WithPolicies(policies))

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 1},
		Origin: OriginDirectLLM,
	})

	require.Error(t, err, "expected ctx-deadline-exceeded error")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"expected wrapped context.DeadlineExceeded, got %v", err)
	assert.Equal(t, 2, inner.calls, "must hit per-attempt timeout twice (initial + 1 retry)")
}

// TestSafeExecutor_ParentDeadlineDominates verifies that a caller-
// supplied ctx deadline shorter than policy.TimeoutMS still wins —
// the per-attempt context.WithTimeout takes the earlier of the two
// deadlines. Guards against future refactors that might "ignore" the
// parent deadline by replacing instead of deriving.
//
// PR #153 review N2 — surfaced because the existing AppliesPerAttempt
// test used context.Background as parent; this case is the composition
// path engine.go relies on (chatTurnTimeout wraps the whole turn).
func TestSafeExecutor_ParentDeadlineDominates(t *testing.T) {
	inner := &slowExecutor{}
	policies := DefaultToolExecutionPolicies()
	p := policies["DescribeCompShareInstance"]
	p.TimeoutMS = 5000 // policy budget = 5s (would otherwise dominate)
	p.BackoffBaseMS = 0
	policies["DescribeCompShareInstance"] = p
	safe := NewSafeToolExecutor(inner, WithPolicies(policies))

	parentCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := safe.ExecuteSafe(parentCtx, SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 1},
		Origin: OriginDirectLLM,
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"expected context.DeadlineExceeded, got %v", err)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"parent's 50ms deadline must dominate policy's 5s; elapsed=%v", elapsed)
}

// TestSafeExecutor_BackoffSleepsBetweenRetries verifies the linear
// backoff inserted between retries. spyExecutor returns a net.OpError
// (network class — retriable) on attempt 1 then succeeds. Measures the
// wall-clock between start and finish; must be at least BackoffBaseMS.
func TestSafeExecutor_BackoffSleepsBetweenRetries(t *testing.T) {
	inner := &spyExecutor{errs: []error{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, nil}}
	policies := DefaultToolExecutionPolicies()
	p := policies["DescribeCompShareInstance"]
	p.BackoffBaseMS = 200 // tighter than the 500ms default so the test stays fast
	p.TimeoutMS = 0       // disable per-attempt timeout so the success path is unbounded
	policies["DescribeCompShareInstance"] = p
	safe := NewSafeToolExecutor(inner, WithPolicies(policies))

	start := time.Now()
	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "DescribeCompShareInstance",
		Args:   map[string]any{"Limit": 1},
		Origin: OriginDirectLLM,
	})
	elapsed := time.Since(start)

	require.NoError(t, err, "second attempt should succeed")
	assert.Equal(t, 2, inner.calls)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond,
		"executor should have slept ~200ms between attempts; elapsed=%v", elapsed)
	assert.Less(t, elapsed, 1*time.Second,
		"backoff should not exceed ~1s for a single retry; elapsed=%v", elapsed)
}

// routedCall records one Execute invocation for routedExecutor.
type routedCall struct {
	action string
	args   map[string]any
}

// routedExecutor dispatches a canned result per action, so tests can mock a
// multi-step resolution flow (e.g. DescribeCFS -> GetCompShareCFSUpgradePrice
// or DescribeCompShareInstance -> DescribeCompShareJupyterToken) where a
// single spyExecutor's uniform response is not enough.
type routedExecutor struct {
	results map[string]map[string]any
	calls   []routedCall
}

func (r *routedExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	r.calls = append(r.calls, routedCall{action: action, args: args})
	if result, ok := r.results[action]; ok {
		return result, nil
	}
	return map[string]any{"RetCode": float64(0)}, nil
}

// TestGetCompShareCFSUpgradePrice_ResolvesZoneIDFromDescribeCFS proves finding
// #15's fix: a raw ReAct tool call to GetCompShareCFSUpgradePrice (no zone_id
// in the schema, so a direct-LLM caller can never supply one) triggers an
// internal DescribeCFS lookup and the resolved zone_id is attached before the
// price call reaches the inner executor — matching the deterministic CFS
// route's existing describe-then-price pattern instead of the pre-fix
// behavior where zone_id was silently absent (upstream would then return a
// fake RetCode=0/Price=0 "success").
func TestGetCompShareCFSUpgradePrice_ResolvesZoneIDFromDescribeCFS(t *testing.T) {
	inner := &routedExecutor{results: map[string]map[string]any{
		"DescribeCFS": {"CfsId": "cfs-abc", "ZoneId": float64(5001)},
	}}
	safe := NewSafeToolExecutor(inner)

	result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "GetCompShareCFSUpgradePrice",
		Args:   map[string]any{"CfsId": "cfs-abc", "Size": 200},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, inner.calls, 2, "must describe the CFS before pricing it")
	assert.Equal(t, "DescribeCFS", inner.calls[0].action)
	assert.Equal(t, "cfs-abc", inner.calls[0].args["CfsId"])
	assert.Equal(t, "GetCompShareCFSUpgradePrice", inner.calls[1].action)
	assert.Equal(t, uint32(5001), inner.calls[1].args["zone_id"],
		"resolved zone_id must reach the price call, or upstream silently returns a fake ¥0")
}

// TestGetCompShareCFSUpgradePrice_RejectsWhenZoneUnresolved proves the safe
// fallback half of finding #15: when the CFS's zone cannot be resolved (empty
// DescribeCFS result), the price call is refused rather than dispatched
// without zone_id — which is exactly the request shape that upstream answers
// with a misleading RetCode=0/Price=0 "success" instead of an error.
func TestGetCompShareCFSUpgradePrice_RejectsWhenZoneUnresolved(t *testing.T) {
	inner := &routedExecutor{results: map[string]map[string]any{
		"DescribeCFS": {"CfsId": "cfs-abc"}, // no ZoneId/ZoneID/zone_id field
	}}
	safe := NewSafeToolExecutor(inner)

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "GetCompShareCFSUpgradePrice",
		Args:   map[string]any{"CfsId": "cfs-abc", "Size": 200},
		Origin: OriginDirectLLM,
	})

	require.ErrorIs(t, err, ErrCFSZoneUnresolved)
	require.Len(t, inner.calls, 1, "must never dispatch the price call without a resolved zone_id")
	assert.Equal(t, "DescribeCFS", inner.calls[0].action)
}

// TestGetCompShareCFSUpgradePrice_PreResolvedZoneIDSkipsExtraDescribe proves
// the fix does not disturb the already-working deterministic CFS route: when
// a caller (e.g. intent.handleCFSUpgradePrice via OriginDiagnosisInternal)
// already resolved and supplied zone_id, no extra DescribeCFS lookup fires.
func TestGetCompShareCFSUpgradePrice_PreResolvedZoneIDSkipsExtraDescribe(t *testing.T) {
	inner := &routedExecutor{}
	safe := NewSafeToolExecutor(inner)

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "GetCompShareCFSUpgradePrice",
		Args:   map[string]any{"CfsId": "cfs-xyz", "Size": 100, "zone_id": uint32(7777)},
		Origin: OriginDiagnosisInternal,
	})

	require.NoError(t, err)
	require.Len(t, inner.calls, 1, "a pre-resolved zone_id must not trigger a redundant DescribeCFS lookup")
	assert.Equal(t, "GetCompShareCFSUpgradePrice", inner.calls[0].action)
	assert.Equal(t, uint32(7777), inner.calls[0].args["zone_id"])
}

// TestDescribeCompShareJupyterToken_ResolvesZoneIDForPodInstance proves
// finding #9's fix: a raw call naming a Pod (cpod-*) instance triggers an
// internal DescribeCompShareInstance lookup for its Zone STRING, a
// DescribeCompShareSupportZone lookup to resolve that string to its numeric
// ZoneID, and attaches the resolved zone_id — matching the live evidence (no
// zone_id -> RetCode 8433 for Pod; zone_id=5001 -> success).
//
// The mock deliberately does NOT put a ZoneId field on the
// DescribeCompShareInstance response — live-verified 2026-07-10 that field
// never exists there (upstream tags it json:"-"); only the string Zone does.
// An earlier version of this test/fix wrongly assumed ZoneId existed on that
// response, which unit-tested green but was a no-op live; this fixture
// exists specifically so that mistake can't silently reappear.
func TestDescribeCompShareJupyterToken_ResolvesZoneIDForPodInstance(t *testing.T) {
	inner := &routedExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{"UHostId": "cpod-abc", "Zone": "cn-bj2-03"}},
		},
		"DescribeCompShareSupportZone": {
			"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "RegionId": float64(2), "IsPod": true},
			},
		},
	}}
	safe := NewSafeToolExecutor(inner, WithZoneCatalog(zones.NewCatalog(0)))
	ctx := WithUser(context.Background(), UserContext{TopOrganizationID: 66391350, OrganizationID: 64404856})

	_, err := safe.ExecuteSafe(ctx, SafeToolRequest{
		Action: "DescribeCompShareJupyterToken",
		Args:   map[string]any{"UHostIds": []any{"cpod-abc"}},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	require.Len(t, inner.calls, 3, "must describe the Pod instance and resolve its zone before requesting its Jupyter token")
	assert.Equal(t, "DescribeCompShareInstance", inner.calls[0].action)
	assert.Equal(t, []string{"cpod-abc"}, inner.calls[0].args["UHostIds"])
	assert.Equal(t, "DescribeCompShareSupportZone", inner.calls[1].action)
	assert.Equal(t, uint32(66391350), inner.calls[1].args["top_organization_id"])
	assert.Equal(t, uint32(64404856), inner.calls[1].args["organization_id"])
	assert.Equal(t, "DescribeCompShareJupyterToken", inner.calls[2].action)
	assert.Equal(t, uint32(5001), inner.calls[2].args["zone_id"])
}

// TestDescribeCompShareJupyterToken_UCloudInstanceSkipsZoneResolution proves
// finding #9's other half: a uhost-* (UCloud) instance ID does not trigger
// any DescribeCompShareInstance lookup or zone_id attachment — the live
// evidence showed only Pod instances needed it.
func TestDescribeCompShareJupyterToken_UCloudInstanceSkipsZoneResolution(t *testing.T) {
	inner := &routedExecutor{}
	safe := NewSafeToolExecutor(inner)

	_, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "DescribeCompShareJupyterToken",
		Args:   map[string]any{"UHostIds": []any{"uhost-abc"}},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	require.Len(t, inner.calls, 1, "a UCloud instance must not trigger a zone-resolution describe call")
	assert.Equal(t, "DescribeCompShareJupyterToken", inner.calls[0].action)
	assert.NotContains(t, inner.calls[0].args, "zone_id")
}

// TestDedupeModelRepositoryTags proves finding #22's fix: duplicate tag
// tokens leaking from the upstream backend's broken dedup (a "seen" map that
// is checked but never written to — live-verified 2026-07-10, 3 total tokens
// with "AI" appearing twice) are removed client-side for a raw tool call,
// covering both shapes that carry tags: DescribeModelRepositoryModels' per-
// model comma-separated "Tag" field and its sibling DescribeModelRepositoryTags'
// flat "Tags" array.
func TestDedupeModelRepositoryTags(t *testing.T) {
	t.Run("DescribeModelRepositoryModels dedupes each model's CSV Tag field", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{
			"Models": []any{
				map[string]any{"Name": "Qwen2.5-7B", "Tag": "AI,LLM,AI"},
				map[string]any{"Name": "Llama-3", "Tag": "LLM"},
			},
		}}
		safe := NewSafeToolExecutor(inner)

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "DescribeModelRepositoryModels",
			Args:   map[string]any{},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		models, ok := result.RawResult["Models"].([]any)
		require.True(t, ok)
		first, ok := models[0].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "AI,LLM", first["Tag"], "duplicate AI token must be removed, order preserved")
		second, ok := models[1].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "LLM", second["Tag"], "single-tag entries must be left as-is")
	})

	t.Run("DescribeModelRepositoryTags dedupes the flat Tags array", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{
			"Tags": []any{"AI", "LLM", "AI"},
		}}
		safe := NewSafeToolExecutor(inner)

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "DescribeModelRepositoryTags",
			Args:   map[string]any{},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		assert.Equal(t, []any{"AI", "LLM"}, result.RawResult["Tags"])
	})
}
