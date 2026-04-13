package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// createMockExecutor returns a mock with successful results for all 5 API calls
// in the CreateInstance workflow.
func createMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12"},
		}},
		"CheckCompShareResourceCapacity": {"AvailableSet": []any{
			map[string]any{"Available": true},
		}},
		"GetCompShareInstancePrice": {"PriceSet": []any{
			map[string]any{"ChargeType": "Dynamic", "Price": 1.58},
		}},
		"CreateCompShareInstance": {"UHostIds": []any{"uhost-new001"}},
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-new001", "State": "Running"},
		}},
	}}
}

func TestCreateInstance_HappyPath(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "工作流执行完成", result.Message)

	// All 6 steps completed
	assert.Len(t, result.Steps, 6)
	expectedNames := []string{"查询镜像", "检查库存", "查询价格", "确认创建", "创建实例", "查看状态"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	// 5 API calls in order (confirm step does not call executor)
	assert.Len(t, executor.calls, 5)
	expectedActions := []string{
		"DescribeCompShareImages",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstancePrice",
		"CreateCompShareInstance",
		"DescribeCompShareInstance",
	}
	for i, action := range expectedActions {
		assert.Equal(t, action, executor.calls[i].action)
	}
}

func TestCreateInstance_ConfirmDenied(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认创建", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)

	// Only 3 API calls before the confirm step; CreateCompShareInstance never called
	assert.Len(t, executor.calls, 3)
	for _, call := range executor.calls {
		assert.NotEqual(t, "CreateCompShareInstance", call.action)
	}
}

func TestCreateInstance_Defaults(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
		// No Zone, ChargeType, Gpu, Cpu, Memory — all should use defaults
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)

	// Find the CreateCompShareInstance call
	var createArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "CreateCompShareInstance" {
			createArgs = call.args
			break
		}
	}
	assert.NotNil(t, createArgs)
	assert.Equal(t, "cn-wlcb-01", createArgs["Zone"])
	assert.Equal(t, "Dynamic", createArgs["ChargeType"])
	assert.Equal(t, float64(1), createArgs["GPU"])
	assert.Equal(t, float64(16), createArgs["CPU"])
	assert.Equal(t, float64(65536), createArgs["Memory"])
	// CompShareImageId should come from step 1
	assert.Equal(t, "img-001", createArgs["CompShareImageId"])
}

func TestCreateInstance_UserOverrides(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType":    "A100",
		"Zone":       "cn-bj2-04",
		"Gpu":        float64(2),
		"ChargeType": "Month",
		"Name":       "my-gpu-server",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)

	// Find the CreateCompShareInstance call
	var createArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "CreateCompShareInstance" {
			createArgs = call.args
			break
		}
	}
	assert.NotNil(t, createArgs)
	assert.Equal(t, "A100", createArgs["GpuType"])
	assert.Equal(t, "cn-bj2-04", createArgs["Zone"])
	assert.Equal(t, float64(2), createArgs["GPU"])
	assert.Equal(t, "Month", createArgs["ChargeType"])
	assert.Equal(t, "my-gpu-server", createArgs["Name"])
}

func TestCreateInstance_CapacityCheckFails(t *testing.T) {
	executor := createMockExecutor()
	executor.failOn = "CheckCompShareResourceCapacity"
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "检查库存", result.StoppedAt)
	assert.Contains(t, result.Message, "检查库存")

	// Only 2 API calls: DescribeCompShareImages + failed CheckCompShareResourceCapacity
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareImages", executor.calls[0].action)
	assert.Equal(t, "CheckCompShareResourceCapacity", executor.calls[1].action)
}

func TestCreateInstance_ConfirmArgsContainSummary(t *testing.T) {
	executor := createMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false // deny so we can inspect args without side effects
	}
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	// Verify summary fields are present
	assert.Equal(t, "4090", capturedArgs["GpuType"])
	assert.Equal(t, "Ubuntu 22.04 CUDA 12", capturedArgs["image"])
	assert.Equal(t, "CreateInstanceWorkflow", capturedArgs["workflow"])
	assert.Equal(t, "cn-wlcb-01", capturedArgs["Zone"])
	assert.Equal(t, "Dynamic", capturedArgs["ChargeType"])

	// price should be the full price step result
	priceResult, ok := capturedArgs["price"].(map[string]any)
	assert.True(t, ok)
	assert.NotNil(t, priceResult)
}
