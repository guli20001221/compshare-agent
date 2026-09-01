package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type switchChargeTypeExecutor struct {
	before       map[string]any
	after        map[string]any
	supportZones map[string]any
	prices       map[string]any
	calls        []executorCall
	switched     bool
	failReadback bool
}

func (e *switchChargeTypeExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, executorCall{action: action, args: args})
	switch action {
	case "DescribeCompShareInstance":
		if e.switched {
			if e.failReadback {
				return nil, fmt.Errorf("readback unavailable")
			}
			return e.after, nil
		}
		return e.before, nil
	case "DescribeCompShareSupportZone":
		return e.supportZones, nil
	case "GetCompShareInstanceUserPrice":
		if e.prices != nil {
			return e.prices, nil
		}
		return switchChargeTypePrices(), nil
	case "SwitchChargeType":
		e.switched = true
		return map[string]any{"RetCode": float64(0)}, nil
	default:
		return nil, fmt.Errorf("unexpected action %s", action)
	}
}

func switchChargeTypeInstance(id, chargeType string) map[string]any {
	return map[string]any{"UHostSet": []any{map[string]any{
		"UHostId": id, "Name": "training", "State": "Running",
		"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "GPU": float64(1),
		"GpuType": "H20", "CPU": float64(16), "Memory": float64(245760),
		"ChargeType": chargeType, "IsSpot": false,
		"DiskSet": []any{
			map[string]any{"Type": "Boot", "DiskType": "CLOUD_SSD", "Size": float64(200)},
			map[string]any{"Type": "Data", "DiskType": "CLOUD_SSD", "Size": float64(100)},
		},
	}}}
}

func switchChargeTypePrices() map[string]any {
	return map[string]any{"PriceDetails": []any{
		map[string]any{"ChargeType": "Dynamic", "Instance": float64(1.1), "SystemDisks": float64(0.1), "Disks": float64(9.9)},
		map[string]any{"ChargeType": "Day", "Instance": float64(18), "SystemDisks": float64(2), "Disks": float64(99)},
		map[string]any{"ChargeType": "Month", "Instance": float64(500), "SystemDisks": float64(20), "Disks": float64(999)},
		map[string]any{"ChargeType": "Year", "Instance": float64(5000), "SystemDisks": float64(200), "Disks": float64(9999)},
	}}
}

func TestSwitchChargeTypeConfirmsThenExecutesAndReadsBack(t *testing.T) {
	executor := &switchChargeTypeExecutor{
		before: switchChargeTypeInstance("uhost-switch", "Postpay"),
		after:  switchChargeTypeInstance("uhost-switch", "Month"),
		supportZones: map[string]any{"ZoneInfo": []any{map[string]any{
			"Zone": "cn-wlcb-01", "Region": "cn-wlcb",
		}}},
	}
	confirmCalls := 0
	eng := NewEngine(executor, func(action string, summary map[string]any) bool {
		confirmCalls++
		assert.Equal(t, "SwitchChargeTypeWorkflow", action)
		assert.Equal(t, "按量付费（按小时计费） → 包月", summary["计费方式变更"])
		assert.Equal(t, "¥500.00/月（预估）", summary["目标实例价格"])
		assert.Equal(t, "¥20.00/月（预估）", summary["目标系统盘价格"])
		assert.Equal(t, "¥520.00/月（预估）", summary["目标合计价格"])
		assert.Contains(t, summary["warning"], "最终费用和到期时间以平台账单为准")
		assert.Contains(t, summary["warning"], "当前接口及 Agent 不支持直接切回")
		assert.Contains(t, summary["warning"], "以控制台和平台实际支持为准")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
		"UHostId": "uhost-switch", "DestChargeType": "Month",
	})

	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, 1, confirmCalls)
	assert.Equal(t, true, result.Data["ReadbackAvailable"])
	assert.Equal(t, true, result.Data["Verified"])
	assert.Equal(t, "Postpay", result.Data["PreviousChargeType"])
	assert.Equal(t, "Month", result.Data["ObservedChargeType"])

	write, ok := findExecutorCall(executor.calls, "SwitchChargeType")
	require.True(t, ok)
	assert.Equal(t, "uhost-switch", write.args["UHostId"])
	assert.Equal(t, "Month", write.args["DestChargeType"])
	assert.Equal(t, "cn-wlcb-01", write.args["Zone"])
	assert.Equal(t, "cn-wlcb", write.args["Region"])
	assert.Equal(t, []string{
		"DescribeCompShareInstance", "DescribeCompShareSupportZone", "GetCompShareInstanceUserPrice", "SwitchChargeType", "DescribeCompShareInstance",
	}, switchChargeTypeActions(executor.calls))

	price, ok := findExecutorCall(executor.calls, "GetCompShareInstanceUserPrice")
	require.True(t, ok)
	assert.Equal(t, "cn-wlcb-01", price.args["Zone"])
	assert.Equal(t, "cn-wlcb", price.args["Region"])
	assert.Equal(t, "H20", price.args["GpuType"])
	assert.Equal(t, float64(1), price.args["GPU"])
	assert.Equal(t, float64(16), price.args["CPU"])
	assert.Equal(t, float64(245760), price.args["Memory"])
	assert.Equal(t, "Month", price.args["ChargeType"])
	disks, ok := price.args["Disks"].([]any)
	require.True(t, ok)
	assert.Len(t, disks, 2)
}

func TestSwitchChargeTypeRejectsPodBeforePriceOrConfirmation(t *testing.T) {
	executor := &switchChargeTypeExecutor{before: switchChargeTypeInstance("cpod-switch", "Postpay")}
	confirmCalled := false
	eng := NewEngine(executor, func(string, map[string]any) bool {
		confirmCalled = true
		return true
	}, nil)

	result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
		"UHostId": "cpod-switch", "DestChargeType": "Day",
	})

	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Contains(t, result.Message, "Pod 实例当前不支持切换计费方式")
	assert.False(t, confirmCalled)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, switchChargeTypeActions(executor.calls))
}

func TestSwitchChargeTypeDoesNotTreatAContainerImageOnUHostAsPod(t *testing.T) {
	before := switchChargeTypeInstance("uhost-container", "Postpay")
	after := switchChargeTypeInstance("uhost-container", "Day")
	before["UHostSet"].([]any)[0].(map[string]any)["InstanceType"] = "Container"
	after["UHostSet"].([]any)[0].(map[string]any)["InstanceType"] = "Container"
	executor := &switchChargeTypeExecutor{before: before, after: after}
	confirmCalled := false
	eng := NewEngine(executor, func(string, map[string]any) bool {
		confirmCalled = true
		return true
	}, nil)

	result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
		"UHostId": "uhost-container", "DestChargeType": "Day",
	})

	require.NoError(t, err)
	require.True(t, result.Success)
	assert.True(t, confirmCalled)
	_, wrote := findExecutorCall(executor.calls, "SwitchChargeType")
	assert.True(t, wrote)
}

func TestSwitchChargeTypeRejectsUnsupportedSourceBeforeConfirmation(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{name: "not running", edit: func(row map[string]any) { row["State"] = "Stopped" }, want: "运行状态"},
		{name: "spot", edit: func(row map[string]any) { row["IsSpot"] = true }, want: "抢占式"},
		{name: "cpu only", edit: func(row map[string]any) { row["GPU"] = float64(0) }, want: "无卡实例"},
		{name: "already prepaid", edit: func(row map[string]any) { row["ChargeType"] = "Month" }, want: "仅支持从按量后付费"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := switchChargeTypeInstance("uhost-switch", "Postpay")
			tt.edit(before["UHostSet"].([]any)[0].(map[string]any))
			executor := &switchChargeTypeExecutor{before: before}
			confirmCalled := false
			eng := NewEngine(executor, func(string, map[string]any) bool {
				confirmCalled = true
				return true
			}, nil)

			result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
				"UHostId": "uhost-switch", "DestChargeType": "Month",
			})

			require.NoError(t, err)
			require.False(t, result.Success)
			assert.Contains(t, result.Message, tt.want)
			assert.False(t, confirmCalled)
			_, wrote := findExecutorCall(executor.calls, "SwitchChargeType")
			assert.False(t, wrote)
		})
	}
}

func TestSwitchChargeTypeReadbackFailureDoesNotInviteDuplicateSubmission(t *testing.T) {
	executor := &switchChargeTypeExecutor{
		before:       switchChargeTypeInstance("uhost-switch", "Postpay"),
		after:        switchChargeTypeInstance("uhost-switch", "Month"),
		supportZones: map[string]any{"ZoneInfo": []any{}},
		failReadback: true,
	}
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
		"UHostId": "uhost-switch", "DestChargeType": "Month",
	})

	require.NoError(t, err)
	require.True(t, result.Success, "the authorized write succeeded even though optional readback failed")
	assert.Equal(t, false, result.Data["ReadbackAvailable"])
	assert.Equal(t, false, result.Data["Verified"])
}

func TestSwitchChargeTypeRequiresARealTargetQuoteBeforeConfirmation(t *testing.T) {
	executor := &switchChargeTypeExecutor{
		before: switchChargeTypeInstance("uhost-switch", "Postpay"),
		after:  switchChargeTypeInstance("uhost-switch", "Year"),
		prices: map[string]any{"PriceDetails": []any{
			map[string]any{"ChargeType": "Year", "Instance": float64(5000)},
		}},
	}
	confirmCalled := false
	eng := NewEngine(executor, func(string, map[string]any) bool {
		confirmCalled = true
		return true
	}, nil)

	result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
		"UHostId": "uhost-switch", "DestChargeType": "Year",
	})

	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Contains(t, result.Message, "未获取到目标计费方式的实例和系统盘价格")
	assert.False(t, confirmCalled)
	price, priced := findExecutorCall(executor.calls, "GetCompShareInstanceUserPrice")
	require.True(t, priced)
	assert.Equal(t, "Year", price.args["ChargeType"], "Year is queried from upstream rather than rejected by a local price-mode list")
	_, wrote := findExecutorCall(executor.calls, "SwitchChargeType")
	assert.False(t, wrote)
}

func TestSwitchChargeTypeQuoteUsesOnlyInstanceAndSystemDisk(t *testing.T) {
	result := map[string]any{"PriceDetails": []any{
		map[string]any{"ChargeType": "Year", "Instance": float64(5000), "Disks": float64(9999)},
		map[string]any{"ChargeType": "Year", "SystemDisks": float64(200)},
	}}

	instance, systemDisk, ok := switchChargeTypePriceParts(result, "Year")
	require.True(t, ok)
	assert.Equal(t, float64(5000), instance)
	assert.Equal(t, float64(200), systemDisk)
	assert.Equal(t, "¥5200.00/年（预估）", switchChargeTypePriceText(instance+systemDisk, "Year"))
}

func TestSwitchChargeTypeQuoteRejectsMissingSystemDiskPrice(t *testing.T) {
	result := map[string]any{"PriceDetails": []any{
		map[string]any{"ChargeType": "Month", "Instance": float64(500)},
	}}

	instance, systemDisk, ok := switchChargeTypePriceParts(result, "Month")
	require.False(t, ok)
	assert.Equal(t, float64(500), instance)
	assert.Zero(t, systemDisk)

	_, _, ok = switchChargeTypePriceParts(map[string]any{"PriceDetails": []any{
		map[string]any{"ChargeType": "Month", "SystemDisks": float64(20)},
	}}, "Month")
	assert.False(t, ok, "a system-disk component without an instance quote is not a usable switch price")
}

func switchChargeTypeActions(calls []executorCall) []string {
	actions := make([]string, 0, len(calls))
	for _, call := range calls {
		actions = append(actions, call.action)
	}
	return actions
}
