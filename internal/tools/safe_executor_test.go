package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/compshare-agent/internal/security"
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

func mustUnmarshalArgs(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

func TestSafeExecutorDoesNotRetryCapErrors(t *testing.T) {
	inner := &spyExecutor{errs: []error{ErrToolCapExceeded, nil}}
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

	require.ErrorIs(t, err, ErrToolCapExceeded)
	assert.Equal(t, 1, inner.calls)
}

func TestSafeExecutorFiltersArgsAndRedactsResult(t *testing.T) {
	inner := &spyExecutor{result: map[string]any{
		"DataSet": []any{map[string]any{"JupyterToken": "raw-jupyter-token"}},
		"Nested":  map[string]any{"Password": "raw-password"},
	}}
	safe := NewSafeToolExecutor(inner)

	result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "DescribeCompShareJupyterToken",
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
	assert.Equal(t, "raw-jupyter-token", result.Display.Value)
	assert.Equal(t, "JupyterToken", result.Display.Kind)

	dataSet, ok := result.LLMResult["DataSet"].([]any)
	require.True(t, ok)
	first, ok := dataSet[0].(map[string]any)
	require.True(t, ok)
	assert.NotEqual(t, "raw-jupyter-token", first["JupyterToken"])

	nested, ok := result.LLMResult["Nested"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "[REDACTED]", nested["Password"])
}

func TestSafeExecutorUsesPolicyForDisplayAndRedaction(t *testing.T) {
	t.Run("display is policy controlled", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{
			"DataSet": []any{map[string]any{"JupyterToken": "raw-jupyter-token"}},
		}}
		policies := DefaultToolExecutionPolicies()
		policy := policies["DescribeCompShareJupyterToken"]
		policy.DualChannelDisplay = false
		policies["DescribeCompShareJupyterToken"] = policy
		safe := NewSafeToolExecutor(inner, WithPolicies(policies))

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "DescribeCompShareJupyterToken",
			Args:   map[string]any{"UHostIds": []any{"uhost-1"}},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		assert.Empty(t, result.Display)
	})

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
	})
}

func TestSafeExecutorMonitorHistoryGuardMarksNoDataWindow(t *testing.T) {
	inner := &spyExecutor{result: map[string]any{
		"Data": []any{
			map[string]any{"UHostId": "uhost-1", "MonitorSet": []any{}},
		},
	}}
	safe := NewSafeToolExecutor(inner)

	result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
		Action: "GetCompShareInstanceMonitor",
		Args: map[string]any{
			"UHostIds":  []any{"uhost-1"},
			"StartTime": float64(1777471200),
			"EndTime":   float64(1777474800),
		},
		Origin: OriginDirectLLM,
	})

	require.NoError(t, err)
	assert.Equal(t, "NO_DATA_IN_REQUESTED_WINDOW", result.LLMResult["MonitorDataStatus"])
	assert.Contains(t, result.LLMResult["MonitorDataGuidance"], "不要编造 CPU/内存/GPU 数值")
}

func TestSafeExecutorMonitorHistoryGuardDoesNotMarkSamplesOrRealtime(t *testing.T) {
	t.Run("historical samples", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{
			"Data": []any{
				map[string]any{
					"UHostId": "uhost-1",
					"Metrics": []any{
						map[string]any{"Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(42)}}}}},
					},
				},
			},
		}}
		safe := NewSafeToolExecutor(inner)

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "GetCompShareInstanceMonitor",
			Args: map[string]any{
				"UHostIds":  []any{"uhost-1"},
				"StartTime": float64(1777471200),
				"EndTime":   float64(1777474800),
			},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		assert.NotContains(t, result.LLMResult, "MonitorDataStatus")
	})

	t.Run("realtime snapshot", func(t *testing.T) {
		inner := &spyExecutor{result: map[string]any{"Data": []any{}}}
		safe := NewSafeToolExecutor(inner)

		result, err := safe.ExecuteSafe(context.Background(), SafeToolRequest{
			Action: "GetCompShareInstanceMonitor",
			Args:   map[string]any{"UHostIds": []any{"uhost-1", "uhost-2"}},
			Origin: OriginDirectLLM,
		})

		require.NoError(t, err)
		assert.NotContains(t, result.LLMResult, "MonitorDataStatus")
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
