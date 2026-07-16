package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cfsSupportZonesFixture(isPod bool) map[string]any {
	return map[string]any{"ZoneInfo": []any{
		map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": isPod},
	}}
}

// --- required-field contracts (mirror intent's CFS request MissingFields) --------

func TestCFSRequests_MissingFields(t *testing.T) {
	require.Nil(t, CFSListRequest{}.MissingFields())
	require.Equal(t, []platform.MissingField{{Name: "zone", Reason: "required"}, {Name: "target_size_gb", Reason: "required"}},
		CFSCreatePriceRequest{}.MissingFields())
	require.Nil(t, CFSCreatePriceRequest{Zone: "z", TargetSizeGB: 50}.MissingFields())
	require.Equal(t, []platform.MissingField{{Name: "cfs", Reason: "required"}, {Name: "target_size_gb", Reason: "required"}},
		CFSUpgradePriceRequest{}.MissingFields())
	require.Equal(t, []platform.MissingField{{Name: "cfs", Reason: "required"}}, CFSRefundEstimateRequest{}.MissingFields())
}

// extractCFSID preserves the legacy cfsIDFromTargetRefs "cfs-"-prefix contract.
func TestExtractCFSID(t *testing.T) {
	assert.Equal(t, "cfs-test", extractCFSID("cfs-test"))
	assert.Equal(t, "CFS-Abc", extractCFSID("CFS-Abc"))
	assert.Equal(t, "", extractCFSID("foo"), "a non-cfs-prefixed id is rejected")
	assert.Equal(t, "", extractCFSID("cfs-"), "the bare prefix is rejected")
	assert.Equal(t, "", extractCFSID(""))
}

// --- CFS list -------------------------------------------------------------------

func TestCFSListHandle_ListsReadOnly(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeCFS": {"CFSSet": []any{
			map[string]any{"CfsId": "cfs-test", "Name": "shared-train", "Size": float64(100), "ChargeType": "Month", "MountStatus": "Mounted"},
		}},
	}}
	reg := NewReadCapability(cfsListReadSpec())
	result := reg.Run(context.Background(), CFSListRequest{}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "DescribeCFS", result.ToolAction)
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "DescribeCFS", exec.calls[0].action)
	assert.Contains(t, result.Reply, "shared-train")
	assert.Contains(t, result.Reply, "100GB")
	assert.Contains(t, result.Reply, "只读")
}

func TestCFSListHandle_UpstreamError(t *testing.T) {
	reg := NewReadCapability(cfsListReadSpec())
	result := reg.Run(context.Background(), CFSListRequest{}, ReadRuntime{Executor: errReadExec{err: errors.New("boom")}})
	require.Equal(t, platform.ReadStatusFailureAfterTool, result.Status)
	assert.Equal(t, cfsFailureLabel+": "+FriendlyReadFailureReply, result.Reply)
}

// --- CFS create price -----------------------------------------------------------

func TestCFSCreatePriceHandle_PodZone(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZonesFixture(true),
		"GetCompShareCFSPrice":         {"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(88.5)}}},
	}}
	reg := NewReadCapability(cfsCreatePriceReadSpec())
	result := reg.Run(context.Background(), CFSCreatePriceRequest{Zone: "cn-bj2-03", TargetSizeGB: 50}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "GetCompShareCFSPrice", result.ToolAction)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeCompShareSupportZone", exec.calls[0].action)
	assert.Equal(t, "GetCompShareCFSPrice", exec.calls[1].action)
	assert.Equal(t, 50, exec.calls[1].args["Size"])
	assert.Equal(t, "cn-bj2-03", exec.calls[1].args["Zone"])
	assert.Equal(t, "Month", exec.calls[1].args["ChargeType"], "default charge type")
	assert.Equal(t, uint32(5001), exec.calls[1].args["zone_id"])
	assert.Equal(t, uint32(3003), exec.calls[1].args["az_group"])
	assert.Contains(t, result.Reply, "88.50")
	assert.NotContains(t, result.Reply, "CFS 共享文件存储（只读查询）")
}

func TestCFSCreatePriceHandle_RejectsNonPodZone(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZonesFixture(false),
	}}
	reg := NewReadCapability(cfsCreatePriceReadSpec())
	result := reg.Run(context.Background(), CFSCreatePriceRequest{Zone: "cn-bj2-03", TargetSizeGB: 50}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	require.Len(t, exec.calls, 1, "a non-pod zone stops before the price call")
	assert.Contains(t, result.Reply, "不是 Pod 区")
}

// --- CFS upgrade price -----------------------------------------------------------

func TestCFSUpgradePriceHandle_ResolvesZoneAndPrices(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeCFS":                 {"CFSSet": []any{map[string]any{"CfsId": "cfs-test", "ZoneId": float64(5001)}}},
		"GetCompShareCFSUpgradePrice": {"Price": float64(12.3)},
	}}
	reg := NewReadCapability(cfsUpgradePriceReadSpec())
	result := reg.Run(context.Background(), CFSUpgradePriceRequest{CFS: platform.CFSRef{ID: "cfs-test"}, TargetSizeGB: 200}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "GetCompShareCFSUpgradePrice", result.ToolAction)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeCFS", exec.calls[0].action)
	assert.Equal(t, "GetCompShareCFSUpgradePrice", exec.calls[1].action)
	assert.Equal(t, "cfs-test", exec.calls[1].args["CfsId"])
	assert.Equal(t, 200, exec.calls[1].args["Size"])
	assert.Equal(t, uint32(5001), exec.calls[1].args["zone_id"])
	assert.Contains(t, result.Reply, "12.30")
}

// --- CFS refund estimate ---------------------------------------------------------

func TestCFSRefundEstimateHandle_ResolvesZoneAndEstimates(t *testing.T) {
	exec := &mapReadExec{results: map[string]map[string]any{
		"DescribeCFS":                {"CFSSet": []any{map[string]any{"CfsId": "cfs-test", "ZoneId": float64(5001)}}},
		"GetCompShareCFSRefundPrice": {"RefundPrice": float64(9.87)},
	}}
	reg := NewReadCapability(cfsRefundEstimateReadSpec())
	result := reg.Run(context.Background(), CFSRefundEstimateRequest{CFS: platform.CFSRef{ID: "cfs-test"}}, ReadRuntime{Executor: exec})

	require.Equal(t, platform.ReadStatusHandled, result.Status)
	assert.Equal(t, "GetCompShareCFSRefundPrice", result.ToolAction)
	require.Len(t, exec.calls, 2)
	assert.Equal(t, "DescribeCFS", exec.calls[0].action)
	assert.Equal(t, "GetCompShareCFSRefundPrice", exec.calls[1].action)
	assert.Equal(t, "cfs-test", exec.calls[1].args["CFSId"])
	assert.Equal(t, uint32(5001), exec.calls[1].args["zone_id"])
	assert.Contains(t, result.Reply, "9.87")
	assert.Contains(t, result.Reply, "不会删除")
}
