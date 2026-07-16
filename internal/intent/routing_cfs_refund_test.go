package intent

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefundEstimateRouteResolvesInstanceAndCallsRefundPrice(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"RefundPriceSet": []any{
			map[string]any{
				"UHostId":     "uhost-a",
				"Code":        float64(0),
				"RefundPrice": float64(12.34),
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Slots: Slots{TargetRefs: []TargetRef{{
				Type:  TargetRefName,
				Value: "train-a",
			}}},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareRefundPrice", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-a"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "估算")
	assert.Contains(t, result.Reply, "12.34")
	assert.NotContains(t, result.Reply, "释放已执行")
}

func TestRefundEstimateRouteRequiresTarget(t *testing.T) {
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	assert.Equal(t, HandlerStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "哪台实例")
	assert.Empty(t, exec.calls)
}

func TestRefundEstimateRouteUsesStructuredInstanceReference(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"RefundPriceSet": []any{
			map[string]any{
				"UHostId":     "cpod-known",
				"Code":        float64(0),
				"RefundPrice": float64(6.66),
			},
		},
	}}
	resolver := refundSnapshotWithCPODPrefix(t)
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Slots:         Slots{TargetRefs: []TargetRef{{Type: TargetRefUHostIDUserInput, Value: "cpod-known", Source: SourceUserText}}},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		Resolver: resolver,
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareRefundPrice", exec.calls[0].action)
	assert.Equal(t, []string{"cpod-known"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "6.66")
}

func TestRefundEstimateRouteDoesNotTrustStructuredIDWithoutResolver(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"RefundPriceSet": []any{
			map[string]any{
				"UHostId":     "uhost-a",
				"Code":        float64(0),
				"RefundPrice": float64(7.77),
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Slots:         Slots{TargetRefs: []TargetRef{{Type: TargetRefUHostIDUserInput, Value: "uhost-a", Source: SourceUserText}}},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
	})

	require.Equal(t, HandlerStatusFallbackBeforeTool, result.Status)
	require.Empty(t, exec.calls)
}

func TestRefundEstimateRouteDoesNotTreatGenericHyphenTokenAsInstanceID(t *testing.T) {
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
	})

	assert.Equal(t, HandlerStatusHandled, result.Status)
	assert.Contains(t, result.Reply, "哪台实例")
	assert.Empty(t, exec.calls)
}

func refundSnapshotWithCPODPrefix(t *testing.T) entity.RegistrySnapshot {
	t.Helper()
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet": []any{
			instanceRow("cpod-known", "pod-known"),
		},
	}, "test"))
	return reg.Snapshot()
}

func TestRefundEstimateRouteUsesFallbackInstanceID(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"RefundPriceSet": []any{
			map[string]any{
				"UHostId":     "uhost-b",
				"Code":        float64(0),
				"RefundPrice": float64(8.88),
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		FallbackInstanceID: "uhost-b",
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "GetCompShareRefundPrice", exec.calls[0].action)
	assert.Equal(t, []string{"uhost-b"}, exec.calls[0].args["UHostIds"])
	assert.Contains(t, result.Reply, "train-b")
	assert.Contains(t, result.Reply, "8.88")
}

func TestRefundEstimateRouteStaleFallbackDoesNotCallTool(t *testing.T) {
	exec := &mockHandlerExecutor{}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		FallbackInstanceID: "uhost-deleted-long-ago",
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentRefundEstimate,
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		Resolver: resourceTestSnapshot(t),
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	assert.Empty(t, exec.calls)
	assert.Contains(t, result.Reply, "未找到")
}

func TestCFSInfoRouteListsCFSReadOnly(t *testing.T) {
	exec := &mockHandlerExecutor{result: map[string]any{
		"CFSSet": []any{
			map[string]any{
				"CfsId":       "cfs-test",
				"Name":        "shared-train",
				"Size":        float64(100),
				"ChargeType":  "Month",
				"MountStatus": "Mounted",
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots:         Slots{CFSKind: CFSKindList},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCFS", exec.calls[0].action)
	assert.Contains(t, result.Reply, "shared-train")
	assert.Contains(t, result.Reply, "100GB")
	assert.Contains(t, result.Reply, "只读")
}

func TestCFSInfoRouteCreatePriceCallsCFSPrice(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": {
			"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			},
		},
		"GetCompShareCFSPrice": {
			"PriceDetails": []any{
				map[string]any{"ChargeType": "Month", "Disks": float64(88.5)},
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots: Slots{
				CFSKind: CFSKindCreatePrice,
				SizeGB:  50,
				Zone:    "cn-bj2-03",
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeCompShareSupportZone", exec.calls[0].action)
	assert.Equal(t, "GetCompShareCFSPrice", exec.calls[1].action)
	assert.Equal(t, 50, exec.calls[1].args["Size"])
	assert.Equal(t, "cn-bj2-03", exec.calls[1].args["Zone"])
	assert.Equal(t, uint32(5001), exec.calls[1].args["zone_id"])
	assert.Equal(t, uint32(3003), exec.calls[1].args["az_group"])
	assert.Contains(t, result.Reply, "88.50")
	assert.NotContains(t, result.Reply, "CFS 共享文件存储（只读查询）")
}

func TestCFSInfoRouteCreatePriceUsesSlots(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": {
			"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			},
		},
		"GetCompShareCFSPrice": {
			"PriceDetails": []any{
				map[string]any{"ChargeType": "Year", "Disks": float64(888.5)},
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots: Slots{
				CFSKind:    CFSKindCreatePrice,
				SizeGB:     50,
				Zone:       "cn-bj2-03",
				ChargeType: "Year",
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "GetCompShareCFSPrice", exec.calls[1].action)
	assert.Equal(t, 50, exec.calls[1].args["Size"])
	assert.Equal(t, "cn-bj2-03", exec.calls[1].args["Zone"])
	assert.Equal(t, "Year", exec.calls[1].args["ChargeType"])
	assert.Contains(t, result.Reply, "888.50")
}

func TestCFSInfoRouteCreatePriceRejectsNonPodZone(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": {
			"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			},
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots: Slots{
				CFSKind: CFSKindCreatePrice,
				SizeGB:  50,
				Zone:    "cn-wlcb-01",
			},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCompShareSupportZone", exec.calls[0].action)
	assert.Contains(t, result.Reply, "不是 Pod 区")
}

func TestCFSInfoRouteUpgradePriceCallsCFSUpgradePrice(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeCFS": {
			"CFSSet": []any{
				map[string]any{"CfsId": "cfs-test", "ZoneId": float64(5001)},
			},
		},
		"GetCompShareCFSUpgradePrice": {
			"Price": float64(12.3),
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots: Slots{CFSKind: CFSKindUpgradePrice, SizeGB: 200, TargetRefs: []TargetRef{{
				Type: TargetRefName, Value: "cfs-test", Source: SourceUserText,
			}}},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeCFS", exec.calls[0].action)
	assert.Equal(t, "GetCompShareCFSUpgradePrice", exec.calls[1].action)
	assert.Equal(t, "cfs-test", exec.calls[1].args["CfsId"])
	assert.Equal(t, 200, exec.calls[1].args["Size"])
	assert.Equal(t, uint32(5001), exec.calls[1].args["zone_id"])
	assert.Contains(t, result.Reply, "12.30")
}

func TestCFSInfoRouteRefundCallsCFSRefundPrice(t *testing.T) {
	exec := &routeSequenceExecutor{results: map[string]map[string]any{
		"DescribeCFS": {
			"CFSSet": []any{
				map[string]any{"CfsId": "cfs-test", "ZoneId": float64(5001)},
			},
		},
		"GetCompShareCFSRefundPrice": {
			"RefundPrice": float64(9.87),
		},
	}}
	handler := NewDemoHandler(exec)
	result := handler.DispatchRoute(context.Background(), HandlerRequest{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots: Slots{CFSKind: CFSKindRefund, TargetRefs: []TargetRef{{
				Type: TargetRefName, Value: "cfs-test", Source: SourceUserText,
			}}},
			Retrieval:  Retrieval{Enabled: false},
			Confidence: 0.8,
		},
	})

	require.Equal(t, HandlerStatusHandled, result.Status)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeCFS", exec.calls[0].action)
	assert.Equal(t, "GetCompShareCFSRefundPrice", exec.calls[1].action)
	assert.Equal(t, "cfs-test", exec.calls[1].args["CFSId"])
	assert.Equal(t, uint32(5001), exec.calls[1].args["zone_id"])
	assert.Contains(t, result.Reply, "9.87")
	assert.Contains(t, result.Reply, "不会删除")
}
