package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refundResolver builds a registry snapshot from (id, name) rows using the same
// instance shape the legacy intent tests use, so resolution parity is anchored
// to identical fixtures.
func refundResolver(t *testing.T, rows ...[2]string) entity.RegistrySnapshot {
	t.Helper()
	set := make([]any, 0, len(rows))
	for _, r := range rows {
		set = append(set, map[string]any{
			"UHostId":   r[0],
			"Name":      r[1],
			"State":     "Running",
			"GpuType":   "4090",
			"GPU":       float64(1),
			"CPU":       float64(8),
			"Memory":    float64(64),
			"ImageType": "Ubuntu",
		})
	}
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(len(set)),
		"UHostSet":   set,
	}, "test"))
	return reg.Snapshot()
}

// coldRegistrySnapshot is a never-synced registry snapshot: it holds no
// instances and cannot assert absence, modelling a fresh HTTP session whose
// registry was never warmed. A user-typed exact id that misses it is
// unverifiable locally, not absent.
func coldRegistrySnapshot() entity.RegistrySnapshot {
	return entity.NewRegistry().Snapshot()
}

func runRefund(t *testing.T, exec ReadExecutor, resolver EntityResolver, req RefundEstimateRequest) ReadResult {
	t.Helper()
	reg := NewReadCapability(refundReadSpec())
	return reg.Run(context.Background(), req, ReadRuntime{Executor: exec, Resolver: resolver})
}

// TestRefundHandle_EmptyEstimate: the upstream returned no refund rows — a
// structured Empty read (issue 1), not a Handled answer that says "no data".
func TestRefundHandle_EmptyEstimate(t *testing.T) {
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})
	exec := &fakeReadExec{result: map[string]any{"RefundPriceSet": []any{}}}

	result := runRefund(t, exec, resolver, RefundEstimateRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-a", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusEmpty, result.Status)
	assert.Contains(t, result.Reply, "未获取到退费估算结果")
}

func refundPriceFixture(id string, code, price float64) map[string]any {
	return map[string]any{"RefundPriceSet": []any{
		map[string]any{"UHostId": id, "Code": code, "RefundPrice": price},
	}}
}

// TestRefundEstimateRequestMissingFields locks the required-targets contract:
// via the read tool an empty target set is needs_input (structured), never a
// Chinese substring, and it is what keeps the legacy FallbackInstanceID branch
// unreachable on this path.
func TestRefundEstimateRequestMissingFields(t *testing.T) {
	require.Equal(t, []platform.MissingField{{Name: "targets", Reason: "required"}}, RefundEstimateRequest{}.MissingFields())
	require.Nil(t, RefundEstimateRequest{Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "x"}}}.MissingFields())
}

// TestRefundHandle_ResolvesNameAndCallsRefundPrice mirrors the legacy
// TestRefundEstimateRouteResolvesInstanceAndCallsRefundPrice through the typed
// path: a name reference resolves to a UHostId, the refund price call carries
// exactly that id, and the reply is an estimate — never a release confirmation.
func TestRefundHandle_ResolvesNameAndCallsRefundPrice(t *testing.T) {
	exec := &fakeReadExec{result: refundPriceFixture("uhost-a", 0, 12.34)}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"}, [2]string{"uhost-b", "train-b"})

	result := runRefund(t, exec, resolver, RefundEstimateRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "GetCompShareRefundPrice", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareRefundPrice", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "估算")
	assert.Contains(t, result.Reply, "12.34")
	assert.NotContains(t, result.Reply, "释放已执行")
}

// TestRefundHandle_StructuredIDResolves mirrors the legacy
// TestRefundEstimateRouteUsesStructuredInstanceReference.
func TestRefundHandle_StructuredIDResolves(t *testing.T) {
	exec := &fakeReadExec{result: refundPriceFixture("cpod-known", 0, 6.66)}
	resolver := refundResolver(t, [2]string{"cpod-known", "pod-known"})

	result := runRefund(t, exec, resolver, RefundEstimateRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "cpod-known", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, []string{"cpod-known"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "6.66")
}

// TestRefundHandle_StructuredIDWithoutResolverFallsBack mirrors the legacy
// TestRefundEstimateRouteDoesNotTrustStructuredIDWithoutResolver: a structured
// id with no resolver is an unresolved-target fallback, and no tool runs.
func TestRefundHandle_StructuredIDWithoutResolverFallsBack(t *testing.T) {
	exec := &fakeReadExec{result: refundPriceFixture("uhost-a", 0, 7.77)}

	result := runRefund(t, exec, nil, RefundEstimateRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-a", Source: platform.SourceUserText}},
	})

	require.Equal(t, platform.ReadStatusFallbackBeforeTool, result.Status)
	assert.Equal(t, platform.ReadFallbackUnresolvedTarget, result.FallbackReason)
	assert.Empty(t, exec.calls)
}

// TestRefundHandle_StalePriorTurnSelectionAnswersInsteadOfRetrying mirrors the
// legacy TestRefundEstimateRouteStaleFallbackDoesNotCallTool: a single
// prior-turn reference that no longer resolves is answered (handled) with the
// stale-selection message, and no refund call is made.
func TestRefundHandle_StalePriorTurnSelectionAnswersInsteadOfRetrying(t *testing.T) {
	exec := &fakeReadExec{}
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runRefund(t, exec, resolver, RefundEstimateRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefUHostIDUserInput, Value: "uhost-deleted-long-ago", Source: platform.SourcePriorTurn}},
	})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "GetCompShareRefundPrice", result.ToolAction)
	assert.Empty(t, exec.calls)
	assert.Contains(t, result.Reply, "未找到")
}

func TestRefundHandle_UpstreamFailure(t *testing.T) {
	resolver := refundResolver(t, [2]string{"uhost-a", "train-a"})

	result := runRefund(t, errReadExec{err: errors.New("boom")}, resolver, RefundEstimateRequest{
		Targets: []platform.TargetRef{{Type: platform.TargetRefName, Value: "train-a"}},
	})

	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, platform.ReadFailureGenericRead, result.FailureClass)
	assert.Equal(t, "GetCompShareRefundPrice", result.ToolAction)
	assert.Equal(t, refundCapabilityLabel+": "+FriendlyReadFailureReply, result.Reply)
}
