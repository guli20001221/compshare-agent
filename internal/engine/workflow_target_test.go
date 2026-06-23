package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowRequiresInstanceTarget(t *testing.T) {
	for _, action := range []string{
		"CreateInstanceWorkflow",
		"EnableNetOptimizerWorkflow",
		"CreateCFSWorkflow",
		"ResizeCFSWorkflow",
	} {
		assert.False(t, workflowRequiresInstanceTarget(action), action)
	}

	for _, action := range []string{
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"RebootInstanceWorkflow",
		"ResizeDiskWorkflow",
		"CreateDiskWorkflow",
		"CreateCustomImageWorkflow",
	} {
		assert.True(t, workflowRequiresInstanceTarget(action), action)
	}
}

func TestExecuteWorkflowCreateCFSResolvesPodZone(t *testing.T) {
	var priceArgs map[string]any
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "GetCompShareCFSPrice":
			priceArgs = args
			return map[string]any{"Price": float64(99)}, nil
		case "CreateCFS":
			createArgs = args
			return map[string]any{"CfsId": "cfs-new"}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我在华北一C创建一个 50GB 的 CFS"

	_ = eng.executeWorkflow(zoneUserCtx(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-wlcb-01",
	}, noopStep)

	require.NotNil(t, priceArgs)
	require.NotNil(t, createArgs)
	assert.Equal(t, uint32(5001), priceArgs["zone_id"])
	assert.Equal(t, uint32(5001), createArgs["zone_id"])
	assert.Equal(t, uint32(3003), priceArgs["az_group"])
	assert.Equal(t, uint32(3003), createArgs["az_group"])
}

func TestExecuteWorkflowCreateCFSRejectsNonPodZoneDeterministically(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我在 cn-wlcb-01 创建一个 50GB 的 CFS"

	reply := eng.executeWorkflow(zoneUserCtx(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-wlcb-01",
	}, noopStep)

	assert.Contains(t, reply, "CFS 创建没有成功")
	assert.Contains(t, reply, "只支持 Pod")
	assert.NotContains(t, reply, "cn-pod-01")
}

func TestExecuteWorkflowEnableNetOptimizerResolvesAzGroup(t *testing.T) {
	var syncArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "CheckCompShareNetOptimizer":
			return map[string]any{"Optimized": false}, nil
		case "SyncCompShareNetOptimizer":
			syncArgs = args
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我开启华北一C网络加速"

	_ = eng.executeWorkflow(zoneUserCtx(), "EnableNetOptimizerWorkflow", map[string]any{
		"Zone": "cn-bj2-03",
	}, noopStep)

	require.NotNil(t, syncArgs)
	assert.Equal(t, uint32(3003), syncArgs["az_group"])
	assert.Equal(t, uint32(1), syncArgs["top_organization_id"])
	assert.Equal(t, uint32(2), syncArgs["organization_id"])
}
