package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// mockInstanceTypes builds a DescribeAvailableCompShareInstanceTypes result
// for the given GPU type with one size entry per (gpuCount, cpu, memGB) tuple.
func mockInstanceTypes(gpuType string, sizes ...struct{ Gpu, Cpu, MemGB float64 }) map[string]any {
	machineSizes := make([]any, 0, len(sizes))
	for _, s := range sizes {
		machineSizes = append(machineSizes, map[string]any{
			"Gpu": s.Gpu,
			"Collection": []any{
				map[string]any{"Cpu": s.Cpu, "Memory": []any{s.MemGB}},
			},
		})
	}
	return map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{"Name": gpuType, "MachineSizes": machineSizes},
		},
	}
}

// createMockExecutor returns a mock with successful results for all API calls
// in the CreateInstance workflow. Default spec: 4090 × 1 / 16C / 64GB.
func createMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12"},
		}},
		"DescribeAvailableCompShareInstanceTypes": mockInstanceTypes("4090",
			struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
		),
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}},
		"GetCompShareInstanceUserPrice": {"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Price": 1.58},
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

	// All 7 steps completed
	assert.Len(t, result.Steps, 7)
	expectedNames := []string{"查询镜像", "查询可用配比", "检查库存", "查询价格", "确认创建", "创建实例", "查看状态"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	// 6 API calls in order (confirm step does not call executor)
	assert.Len(t, executor.calls, 6)
	expectedActions := []string{
		"DescribeCompShareImages",
		"DescribeAvailableCompShareInstanceTypes",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstanceUserPrice",
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

	// Only 4 API calls before the confirm step; CreateCompShareInstance never called
	assert.Len(t, executor.calls, 4)
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

	// Verify price step uses UserPrice API with correct params
	var priceArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "GetCompShareInstanceUserPrice" {
			priceArgs = call.args
			break
		}
	}
	assert.NotNil(t, priceArgs, "should call GetCompShareInstanceUserPrice")
	assert.Equal(t, float64(1), priceArgs["GPU"], "UserPrice API uses uppercase GPU")
	assert.Equal(t, float64(16), priceArgs["CPU"], "UserPrice API uses uppercase CPU")
	assert.Equal(t, "Postpay", priceArgs["ChargeType"], "Dynamic should map to Postpay for UserPrice API")
}

func TestCreateInstance_UserOverrides(t *testing.T) {
	executor := createMockExecutor()
	// Override instance types and capacity to match A100 × 2 / 32C / 128GB
	executor.results["DescribeAvailableCompShareInstanceTypes"] = mockInstanceTypes("A100",
		struct{ Gpu, Cpu, MemGB float64 }{2, 32, 128},
	)
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(2), "Cpu": float64(32), "Mem": float64(128), "ResourceEnough": true},
	}}
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
	assert.Equal(t, float64(32), createArgs["CPU"])
	assert.Equal(t, float64(128*1024), createArgs["Memory"])
	assert.Equal(t, "Month", createArgs["ChargeType"])
	assert.Equal(t, "my-gpu-server", createArgs["Name"])

	// Verify price step maps Month ChargeType as-is (not converted to Postpay)
	var priceArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "GetCompShareInstanceUserPrice" {
			priceArgs = call.args
			break
		}
	}
	assert.NotNil(t, priceArgs)
	assert.Equal(t, "Month", priceArgs["ChargeType"], "Month should pass through unchanged")
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

	// 3 API calls: DescribeCompShareImages + DescribeAvailableCompShareInstanceTypes + failed CheckCompShareResourceCapacity
	assert.Len(t, executor.calls, 3)
	assert.Equal(t, "DescribeCompShareImages", executor.calls[0].action)
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", executor.calls[1].action)
	assert.Equal(t, "CheckCompShareResourceCapacity", executor.calls[2].action)
}

// --- Community image path tests ---

func communityMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCommunityImages": {"CompshareImageGroup": []any{
			map[string]any{
				"ImageName": "Stable Diffusion WebUI",
				"Data": []any{
					map[string]any{
						"CompShareImageId": "cimg-sd-001",
						"Name":             "SD WebUI v1.9",
					},
				},
			},
		}},
		"DescribeAvailableCompShareInstanceTypes": mockInstanceTypes("4090",
			struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
		),
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}},
		"GetCompShareInstanceUserPrice": {"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Price": 1.58},
		}},
		"CreateCompShareInstance": {"UHostIds": []any{"uhost-new002"}},
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-new002", "State": "Running"},
		}},
	}}
}

func TestCreateInstance_CommunityImage_HappyPath(t *testing.T) {
	executor := communityMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType":     "4090",
		"ImageSource": "community",
		"ImageName":   "stable diffusion",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)

	// Should call DescribeCommunityImages (not DescribeCompShareImages)
	assert.Equal(t, "DescribeCommunityImages", executor.calls[0].action)
	assert.Equal(t, "stable diffusion", executor.calls[0].args["FuzzySearch"])

	// stepGetPrice should include CompShareImageId for community images
	var priceArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "GetCompShareInstanceUserPrice" {
			priceArgs = call.args
			break
		}
	}
	assert.NotNil(t, priceArgs)
	assert.Equal(t, "cimg-sd-001", priceArgs["CompShareImageId"])

	// CreateCompShareInstance should use community image ID
	var createArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "CreateCompShareInstance" {
			createArgs = call.args
			break
		}
	}
	assert.NotNil(t, createArgs)
	assert.Equal(t, "cimg-sd-001", createArgs["CompShareImageId"])
}

func TestCreateInstance_CommunityImage_ConfirmShowsImageName(t *testing.T) {
	executor := communityMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType":     "4090",
		"ImageSource": "community",
		"ImageName":   "stable diffusion",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, "Stable Diffusion WebUI", capturedArgs["image"])
}

func TestPickFirstCommunityImageId_MalformedData(t *testing.T) {
	assert.Equal(t, "", pickFirstCommunityImageId(nil))
	assert.Equal(t, "", pickFirstCommunityImageId(map[string]any{}))
	assert.Equal(t, "", pickFirstCommunityImageId(map[string]any{
		"CompshareImageGroup": []any{},
	}))
	assert.Equal(t, "", pickFirstCommunityImageId(map[string]any{
		"CompshareImageGroup": []any{map[string]any{"Data": []any{}}},
	}))
}

func TestCreateInstance_CommunityImage_NoName_Rejected(t *testing.T) {
	executor := communityMockExecutor()
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType":     "4090",
		"ImageSource": "community",
		// ImageName deliberately omitted
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询镜像", result.StoppedAt)
	assert.Contains(t, result.Message, "镜像名称")

	// DescribeCommunityImages should NOT be called
	assert.Empty(t, executor.calls)
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

	// Confirm must show resolved spec so user sees what will be created
	assert.Equal(t, float64(1), capturedArgs["Gpu"])
	assert.Equal(t, float64(16), capturedArgs["CPU"])
	assert.Equal(t, float64(65536), capturedArgs["Memory"]) // 64GB in MB

	// price should be the full price step result
	priceResult, ok := capturedArgs["price"].(map[string]any)
	assert.True(t, ok)
	assert.NotNil(t, priceResult)
}

// --- Platform image selection tests ---

func TestCreateInstance_PlatformImage_DefaultQueryDoesNotForceSystem(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})
	assert.NoError(t, err)

	// Find the DescribeCompShareImages call
	var imageArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "DescribeCompShareImages" {
			imageArgs = call.args
			break
		}
	}
	assert.NotNil(t, imageArgs, "should call DescribeCompShareImages")
	_, hasImageType := imageArgs["ImageType"]
	assert.False(t, hasImageType, "should NOT force ImageType=System; must return all platform images")
}

func TestCreateInstance_PlatformImage_WithImageName_UsesNameFilter(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType":   "4090",
		"ImageName": "PyTorch",
	})
	assert.NoError(t, err)

	var imageArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "DescribeCompShareImages" {
			imageArgs = call.args
			break
		}
	}
	assert.NotNil(t, imageArgs)
	assert.Equal(t, "PyTorch", imageArgs["Name"], "should pass ImageName as Name filter")
}

func TestPickPlatformImageId_PrefersNameMatch(t *testing.T) {
	result := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04"},
			map[string]any{"CompShareImageId": "img-pytorch", "Name": "PyTorch 2.1 CUDA 12"},
		},
	}

	// With ImageName → should prefer PyTorch over Ubuntu
	id := pickPlatformImageId(map[string]any{"ImageName": "PyTorch"}, result)
	assert.Equal(t, "img-pytorch", id)

	// Without ImageName → falls back to first entry
	id = pickPlatformImageId(map[string]any{}, result)
	assert.Equal(t, "img-ubuntu", id)
}

func TestPickPlatformImage_ExactMatchWins(t *testing.T) {
	result := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch 2.1 CUDA 12"},
			map[string]any{"CompShareImageId": "img-2", "Name": "CUDA"},
		},
	}
	// Exact match (case-insensitive) takes priority over contains
	id := pickPlatformImageId(map[string]any{"ImageName": "CUDA"}, result)
	assert.Equal(t, "img-2", id)
}

func TestPickPlatformImage_ContainsMatch(t *testing.T) {
	result := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04"},
			map[string]any{"CompShareImageId": "img-comfy", "Name": "ComfyUI v1.3 CUDA 12"},
		},
	}
	id := pickPlatformImageId(map[string]any{"ImageName": "comfyui"}, result)
	assert.Equal(t, "img-comfy", id, "case-insensitive contains should match")
}

func TestPickPlatformImage_NoMatchFallsBackToFirst(t *testing.T) {
	result := map[string]any{
		"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-ubuntu", "Name": "Ubuntu 22.04"},
		},
	}
	id := pickPlatformImageId(map[string]any{"ImageName": "NonExistent"}, result)
	assert.Equal(t, "img-ubuntu", id, "should fall back to first when no match")
}

func TestPickPlatformImage_EmptyResult(t *testing.T) {
	assert.Equal(t, "", pickPlatformImageId(map[string]any{}, nil))
	assert.Equal(t, "", pickPlatformImageId(map[string]any{}, map[string]any{}))
	assert.Equal(t, "", pickPlatformImageId(map[string]any{}, map[string]any{"ImageSet": []any{}}))
	assert.Equal(t, "未知", pickPlatformImageName(map[string]any{}, nil))
}

// --- Capacity check exact-match tests (bug regression) ---

func TestCreateInstance_CapacityCheck_WrongGpuCount_Rejected(t *testing.T) {
	// Inventory has 1-card available but user requests 2-card → should fail.
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = mockInstanceTypes("4090",
		struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
		struct{ Gpu, Cpu, MemGB float64 }{2, 32, 128},
	)
	// Only 1-card has stock; 2-card is sold out.
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		map[string]any{"Gpu": float64(2), "Cpu": float64(32), "Mem": float64(128), "ResourceEnough": false},
	}}
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
		"Gpu":     float64(2),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success, "should reject when target spec is sold out")
	assert.Equal(t, "检查库存", result.StoppedAt)
	assert.Contains(t, result.Message, "库存不足")
}

func TestCreateInstance_CapacityCheck_SpecNotInList_Rejected(t *testing.T) {
	// Inventory returns specs that don't match the target at all.
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = mockInstanceTypes("4090",
		struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
	)
	// Specs only contain a different combo — target 1/16/64GB won't match 1/8/32GB.
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(8), "Mem": float64(32), "ResourceEnough": true},
	}}
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success, "should reject when target spec not found in inventory list")
	assert.Equal(t, "检查库存", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
}

// --- Spec candidate / ambiguity tests ---

// multiMemoryInstanceTypes returns an API result with Memory: [64, 94] for 4090 × 1.
func multiMemoryInstanceTypes() map[string]any {
	return map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "4090",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64), float64(94)}},
						},
					},
				},
			},
		},
	}
}

func TestListSpecCandidates_ExpandsAllCombinations(t *testing.T) {
	result := multiMemoryInstanceTypes()
	candidates := listSpecCandidates(result, "4090", 1)
	assert.Len(t, candidates, 2)
	assert.Equal(t, specCandidate{CPU: 16, MemoryMB: 64 * 1024}, candidates[0])
	assert.Equal(t, specCandidate{CPU: 16, MemoryMB: 94 * 1024}, candidates[1])
}

func TestListSpecCandidates_MultipleCollections(t *testing.T) {
	result := map[string]any{
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "A800",
				"MachineSizes": []any{
					map[string]any{
						"Gpu": float64(1),
						"Collection": []any{
							map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
							map[string]any{"Cpu": float64(32), "Memory": []any{float64(128)}},
						},
					},
				},
			},
		},
	}
	candidates := listSpecCandidates(result, "A800", 1)
	assert.Len(t, candidates, 2)
	assert.Equal(t, specCandidate{CPU: 16, MemoryMB: 64 * 1024}, candidates[0])
	assert.Equal(t, specCandidate{CPU: 32, MemoryMB: 128 * 1024}, candidates[1])
}

func TestResolveTargetSpec_SingleCandidate_AutoSelect(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询可用配比"] = mockInstanceTypes("4090",
		struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
	)
	gpu, cpu, mem, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(64*1024), mem)
}

func TestResolveTargetSpec_MultiCandidate_NoCpuMem_DefaultsToFirst(t *testing.T) {
	// Multiple candidates, user gave neither Cpu nor Memory → default to first.
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	gpu, cpu, mem, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(64*1024), mem, "should default to first candidate (64GB)")
}

func TestResolveTargetSpec_MultiCandidate_OnlyCpu_StillAmbiguous(t *testing.T) {
	// Both candidates have Cpu=16 → filtering by Cpu alone doesn't resolve.
	wfCtx := NewContext(map[string]any{"GpuType": "4090", "Cpu": float64(16)})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	_, _, _, err := resolveTargetSpec(wfCtx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "多种合法配比")
}

func TestResolveTargetSpec_MultiCandidate_CpuMemExactMatch(t *testing.T) {
	wfCtx := NewContext(map[string]any{
		"GpuType": "4090",
		"Cpu":     float64(16),
		"Memory":  float64(94 * 1024), // 94GB in MB
	})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	gpu, cpu, mem, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(94*1024), mem)
}

func TestResolveTargetSpec_MultiCandidate_IllegalCombo(t *testing.T) {
	wfCtx := NewContext(map[string]any{
		"GpuType": "4090",
		"Cpu":     float64(32),
		"Memory":  float64(64 * 1024),
	})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	_, _, _, err := resolveTargetSpec(wfCtx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "不支持")
	assert.Contains(t, err.Error(), "合法选项")
}

func TestResolveTargetSpec_MultiCandidate_OnlyMemory_Resolves(t *testing.T) {
	// Two candidates: 16C/64GB, 16C/94GB. User gives Memory=94GB → unique match.
	wfCtx := NewContext(map[string]any{
		"GpuType": "4090",
		"Memory":  float64(94 * 1024),
	})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	gpu, cpu, mem, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(94*1024), mem)
}

func TestCreateInstance_MultiCandidate_DefaultsToFirst_WorkflowProceeds(t *testing.T) {
	// API returns Memory: [64, 94]. User gives no Cpu/Memory → defaults to first (16C/64GB).
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = multiMemoryInstanceTypes()
	// Capacity specs must include the default combo for the check to pass.
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success, "multi-candidate with no Cpu/Memory should default to first and succeed")

	// Verify the create call used the default spec (16C / 64GB=65536MB)
	var createArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "CreateCompShareInstance" {
			createArgs = call.args
			break
		}
	}
	assert.NotNil(t, createArgs)
	assert.Equal(t, float64(16), createArgs["CPU"])
	assert.Equal(t, float64(64*1024), createArgs["Memory"])
}

func TestCreateInstance_ExplicitCpuMemory_OverridesDefault(t *testing.T) {
	// API returns Memory: [64, 94]. User explicitly requests 94GB → uses that, not the default.
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = multiMemoryInstanceTypes()
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(94), "ResourceEnough": true},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
		"Cpu":     float64(16),
		"Memory":  float64(94 * 1024),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)

	var createArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "CreateCompShareInstance" {
			createArgs = call.args
			break
		}
	}
	assert.NotNil(t, createArgs)
	assert.Equal(t, float64(16), createArgs["CPU"])
	assert.Equal(t, float64(94*1024), createArgs["Memory"])
}

func TestCreateInstance_SingleCandidate_ConfirmShowsSpec(t *testing.T) {
	// Single candidate — auto-selected, confirm card must show it.
	executor := createMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, float64(1), capturedArgs["Gpu"])
	assert.Equal(t, float64(16), capturedArgs["CPU"])
	assert.Equal(t, float64(65536), capturedArgs["Memory"])
}
