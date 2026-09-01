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
		"GpuType": "H20", "ChargeType": chargeType, "IsSpot": false,
	}}}
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
		assert.Contains(t, summary["warning"], "实际费用和到期时间以平台账单为准")
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
		"DescribeCompShareInstance", "DescribeCompShareSupportZone", "SwitchChargeType", "DescribeCompShareInstance",
	}, switchChargeTypeActions(executor.calls))
}

func TestSwitchChargeTypePodUsesLivePlacement(t *testing.T) {
	before := switchChargeTypeInstance("cpod-switch", "Postpay")
	after := switchChargeTypeInstance("cpod-switch", "Day")
	beforeHost := before["UHostSet"].([]any)[0].(map[string]any)
	afterHost := after["UHostSet"].([]any)[0].(map[string]any)
	beforeHost["Zone"], beforeHost["Region"] = "cn-wlcb-03", ""
	afterHost["Zone"], afterHost["Region"] = "cn-wlcb-03", ""
	executor := &switchChargeTypeExecutor{
		before: before,
		after:  after,
		supportZones: map[string]any{"ZoneInfo": []any{map[string]any{
			"Zone": "cn-wlcb-03", "Region": "cn-wlcb",
			"ZoneId": float64(8300), "RegionId": float64(1000010),
		}}},
	}
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), SwitchChargeTypeDef(), map[string]any{
		"UHostId": "cpod-switch", "DestChargeType": "Day",
	})

	require.NoError(t, err)
	require.True(t, result.Success)
	write, ok := findExecutorCall(executor.calls, "SwitchChargeType")
	require.True(t, ok)
	assert.Equal(t, uint32(8300), write.args["zone_id"])
	assert.Equal(t, uint32(1000010), write.args["az_group"])
	assert.Equal(t, "cn-wlcb", write.args["Region"])
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

func switchChargeTypeActions(calls []executorCall) []string {
	actions := make([]string, 0, len(calls))
	for _, call := range calls {
		actions = append(actions, call.action)
	}
	return actions
}
