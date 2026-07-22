package workflow

import (
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceTypes builds a DescribeAvailableCompShareInstanceTypes result
// for the given GPU type with one size entry per (gpuCount, cpu, memGB) tuple.
// livePriceDetails is the PriceDetails array a real GetCompShareInstanceUserPrice
// reply carries, copied from the capture at eval/real_cli_golden_doubao_lite.md:74-79.
//
// Two things about it are load-bearing and neither was true of the fixture it
// replaces. The amount arrives under "Instance"; "Price" appears in no live
// capture, so a fixture using it exercised only priceAmountFor's fallback — a
// branch production never reaches. And ONE call quotes EVERY charge type at once,
// which is what makes the create's exact-charge-type lookup work for 包日/包月/抢占式
// rather than only for the one that was asked about. A fixture quoting Postpay
// alone made a Month create look priceless, which is a property of the fixture and
// not of the platform.
func livePriceDetails() []any {
	return []any{
		map[string]any{"ChargeType": "Postpay", "Instance": 1.58},
		map[string]any{"ChargeType": "Dynamic", "Instance": 1.58},
		map[string]any{"ChargeType": "Day", "Instance": 34.9},
		map[string]any{"ChargeType": "Month", "Instance": 951.85},
		map[string]any{"ChargeType": "Spot", "Instance": 1.1},
	}
}

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
			map[string]any{
				"Name":         gpuType,
				"MachineSizes": machineSizes,
				"CpuPlatforms": map[string]any{"Amd": map[string]any{}},
				"Disks":        []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
			},
		},
	}
}

// createMockExecutor returns a mock with successful results for all API calls
// in the CreateInstance workflow. Default spec: 4090 × 1 / 16C / 64GB.
func createMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12", "Size": float64(102400)},
		}},
		"DescribeAvailableCompShareInstanceTypes": mockInstanceTypes("4090",
			struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
		),
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
		}},
		"GetCompShareInstanceUserPrice": {"PriceDetails": livePriceDetails()},
		"CreateCompShareInstance":       {"UHostIds": []any{"uhost-new001"}},
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
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "工作流执行完成", result.Message)
	assert.Equal(t, []any{"uhost-new001"}, result.Data["UHostIds"])

	// All 12 steps completed. The three inventory steps are here so the plain
	// flow's purchase-mode gate and its confirm card read the same live fact the
	// guided flow does; without them createInventoryPoolSupport had nothing to
	// read on this path and every charge type looked supported.
	assert.Len(t, result.Steps, 12)
	expectedNames := []string{"查询镜像", "查询可用配比", "查询官方GPU库存", "查询Pod GPU库存", "查询GPU库存", "形成执行草稿", "检查库存", "查询价格", "形成确认快照", "确认创建", "创建实例", "查看状态"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	// 8 API calls in order. Two of them are the split GPU inventory read (the
	// request's zone_id selects the backend rather than filtering the result, so
	// the official and Pod pools need one call each). Still neither the confirm
	// gate NOR any resolve step calls the executor, which is the point.
	assert.Len(t, executor.calls, 8)
	expectedActions := []string{
		"DescribeCompShareImages",
		"DescribeAvailableCompShareInstanceTypes",
		"DescribeCompShareGpuInventory",
		"DescribeCompShareGpuInventory",
		"CheckCompShareResourceCapacity",
		"GetCompShareInstanceUserPrice",
		"CreateCompShareInstance",
		"DescribeCompShareInstance",
	}
	for i, action := range expectedActions {
		assert.Equal(t, action, executor.calls[i].action)
	}
}

func TestCreateInstance_DescribeFailureDoesNotHideCreatedInstance(t *testing.T) {
	executor := createMockExecutor()
	executor.failOn = "DescribeCompShareInstance"
	confirmFn := func(action string, args map[string]any) bool { return true }

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, nil)
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success, "CreateCompShareInstance returned UHostIds, so a not-ready describe must not flip create success")
	assert.Empty(t, result.StoppedAt)
	assert.Contains(t, executor.calls, executorCall{action: "CreateCompShareInstance", args: executor.calls[6].args})
	assert.Equal(t, "查看状态", result.Steps[len(result.Steps)-1].Name)
	assert.Equal(t, "failed", result.Steps[len(result.Steps)-1].Status)
}

func TestCreateInstance_ConfirmDenied(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认创建", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)

	// Only 6 API calls before the confirm step (2 images/catalog + 2 inventory
	// backends + capacity + price); CreateCompShareInstance never called
	assert.Len(t, executor.calls, 6)
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
	result, err := eng.runCreateTest(def, map[string]any{
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
	assert.Equal(t, "Postpay", createArgs["ChargeType"])
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
	assert.Equal(t, "Postpay", priceArgs["ChargeType"], "default hourly billing should use Postpay for UserPrice API")
	assert.Equal(t, "img-001", priceArgs["CompShareImageId"], "price query must use the same resolved image as create")
	assert.Equal(t, []any{map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": uint32(100)}}, priceArgs["Disks"], "price query must include system disk config from image/catalog")

	assert.Equal(t, "G", createArgs["MachineType"])
	assert.Equal(t, "Amd/Auto", createArgs["MinimalCpuPlatform"])
	assert.Equal(t, "Password", createArgs["LoginMode"])
	assert.Equal(t, []any{map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": uint32(100)}}, createArgs["Disks"])
}

func TestCreateInstance_PlatformImageSkipsOfflineCandidate(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId": "img-pytorch-offline",
			"Name":             "PyTorch:24.04-py3",
			"Status":           "Offline",
		},
		map[string]any{
			"CompShareImageId": "img-pytorch-available",
			"Name":             "cuda130_torch291_py312",
			"Status":           "Available",
		},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, nil)
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType":   "4090",
		"ImageName": "PyTorch",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)

	for _, action := range []string{"CheckCompShareResourceCapacity", "GetCompShareInstanceUserPrice", "CreateCompShareInstance"} {
		var args map[string]any
		for _, call := range executor.calls {
			if call.action == action {
				args = call.args
				break
			}
		}
		assert.NotNil(t, args, "expected %s call", action)
		assert.Equal(t, "img-pytorch-available", args["CompShareImageId"], "expected %s to use the available PyTorch-compatible image", action)
	}
}

func TestCreateInstance_PlatformImageAllOfflineBlockedBeforeCapacity(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId": "img-pytorch-offline",
			"Name":             "PyTorch:24.04-py3",
			"Status":           "Offline",
		},
	}}
	confirmFn := func(action string, args map[string]any) bool {
		t.Fatalf("confirm should not be reached when all requested images are unavailable")
		return false
	}

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, nil)
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType":   "4090",
		"ImageName": "PyTorch",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	// Stops while forming the draft: an image that cannot be selected is not a
	// question to ask capacity about. This used to surface one step later, from
	// 检查库存's own image lookup.
	assert.Equal(t, "形成执行草稿", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到可用的 PyTorch 镜像")
	for _, call := range executor.calls {
		assert.NotEqual(t, "CheckCompShareResourceCapacity", call.action)
		assert.NotEqual(t, "GetCompShareInstanceUserPrice", call.action)
		assert.NotEqual(t, "CreateCompShareInstance", call.action)
	}
}

func TestCreateInstance_DynamicInputNormalizesToPostpay(t *testing.T) {
	executor := createMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return true
	}
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType":    "4090",
		"ChargeType": "Dynamic",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, "Postpay", capturedArgs["ChargeType"])

	for _, call := range executor.calls {
		switch call.action {
		case "CheckCompShareResourceCapacity", "GetCompShareInstanceUserPrice", "CreateCompShareInstance":
			assert.Equal(t, "Postpay", call.args["ChargeType"], "%s should not receive deprecated Dynamic", call.action)
		}
	}
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
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType":    "A100",
		"Zone":       "cn-bj2-04",
		"Gpu":        float64(2),
		"ChargeType": "Month",
		"Name":       "my-gpu-server",
	}, withNormalZone("cn-bj2-04", "cn-bj2", 6004))

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
	result, err := eng.runCreateTest(def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "检查库存", result.StoppedAt)
	assert.Contains(t, result.Message, "检查库存")

	// 5 API calls: images + catalog + the two inventory backends + the failed
	// CheckCompShareResourceCapacity. Nothing runs after capacity fails.
	assert.Len(t, executor.calls, 5)
	assert.Equal(t, "DescribeCompShareImages", executor.calls[0].action)
	assert.Equal(t, "DescribeAvailableCompShareInstanceTypes", executor.calls[1].action)
	assert.Equal(t, "DescribeCompShareGpuInventory", executor.calls[2].action)
	assert.Equal(t, "DescribeCompShareGpuInventory", executor.calls[3].action)
	assert.Equal(t, "CheckCompShareResourceCapacity", executor.calls[4].action)
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
		"GetCompShareInstanceUserPrice": {"PriceDetails": livePriceDetails()},
		"CreateCompShareInstance":       {"UHostIds": []any{"uhost-new002"}},
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
	result, err := eng.runCreateTest(def, map[string]any{
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
	_, err := eng.runCreateTest(def, map[string]any{
		"GpuType":     "4090",
		"ImageSource": "community",
		"ImageName":   "stable diffusion",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, "Stable Diffusion WebUI", capturedArgs["image"])
}

func TestCreateInstance_CommunityImage_NoName_Rejected(t *testing.T) {
	executor := communityMockExecutor()
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.runCreateTest(def, map[string]any{
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
	_, err := eng.runCreateTest(def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	// Verify summary fields are present
	assert.Equal(t, "4090", capturedArgs["GpuType"])
	assert.Equal(t, "Ubuntu 22.04 CUDA 12", capturedArgs["image"])
	assert.Equal(t, "CreateInstanceWorkflow", capturedArgs["workflow"])
	assert.Equal(t, "cn-wlcb-01", capturedArgs["Zone"])
	assert.Equal(t, "Postpay", capturedArgs["ChargeType"])

	// Confirm must show resolved spec so user sees what will be created
	assert.Equal(t, float64(1), capturedArgs["Gpu"])
	assert.Equal(t, float64(16), capturedArgs["CPU"])
	assert.Equal(t, float64(65536), capturedArgs["Memory"]) // 64GB in MB

	// price is rendered as a readable display string for the confirm card, NOT
	// the raw GetCompShareInstanceUserPrice object (which the frontend stringified
	// as "[object Object]"). Default ChargeType is Postpay; the mock returns
	// PriceDetails [{ChargeType: Postpay, Price: 1.58}] with no list price.
	//
	// The （预估） is in the VALUE, not only in PriceNote, because upstream returns
	// no quote id and no validity — it cannot hold this number — and the HTTP
	// frontend that renders these args is not this repo's to relabel. A bare
	// number there would read as a commitment the platform never made.
	assert.Equal(t, "¥1.58/小时（预估）", capturedArgs["price"])
	assert.Equal(t, "最终费用以实际创建和结算结果为准", capturedArgs["PriceNote"])
}

// TestEstimatedPriceDisplayText covers the price rendering, retargeted from
// confirmPriceText onto extractEstimatedPrice, which absorbed it — there is now
// one producer of the create's price string, so the card and the sealed record
// cannot disagree about it.
//
// The inputs are unchanged. `want` gained （预估）, which is the point of the
// change: upstream returns no quote id and no validity, so every one of these
// numbers is an estimate. An empty `want` means "no snapshot at all" rather than
// an empty string — an absent price must stay absent, because a 0 renders as free.
func TestEstimatedPriceDisplayText(t *testing.T) {
	cases := []struct {
		name       string
		price      any
		chargeType string
		want       string
	}{
		{
			name:       "postpay payable only",
			price:      map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.58}}},
			chargeType: "Postpay",
			want:       "¥1.58/小时",
		},
		{
			name: "discount surfaces list price as 原价",
			price: map[string]any{
				"PriceDetails":     []any{map[string]any{"ChargeType": "Postpay", "Price": 1.58}},
				"ListPriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.98}},
			},
			chargeType: "Postpay",
			want:       "¥1.58/小时（原价 ¥1.98）",
		},
		{
			name: "list equal to payable -> no 原价",
			price: map[string]any{
				"PriceDetails":     []any{map[string]any{"ChargeType": "Postpay", "Price": 1.58}},
				"ListPriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.58}},
			},
			chargeType: "Postpay",
			want:       "¥1.58/小时",
		},
		{
			// The LIVE shape. Every captured GetCompShareInstancePriceResponse
			// quotes under "Instance"; the "Price" key the other cases use appears
			// in none of them. This case was labelled "fallback (catalog-shape
			// robustness)" — exactly backwards from what upstream actually sends.
			name:       "Instance field (the shape upstream actually returns)",
			price:      map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Instance": 2.0}}},
			chargeType: "Postpay",
			want:       "¥2.00/小时",
		},
		{
			name:       "Month period unit",
			price:      map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Price": 1000.0}}},
			chargeType: "Month",
			want:       "¥1000.00/月",
		},
		{
			name:       "resolved charge type absent -> empty",
			price:      map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Price": 1.0}}},
			chargeType: "Postpay",
			want:       "",
		},
		{
			name:       "unexpected shape -> empty (never a raw object)",
			price:      map[string]any{"unexpected": true},
			chargeType: "Postpay",
			want:       "",
		},
		{
			name:       "nil result -> empty",
			price:      nil,
			chargeType: "Postpay",
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractEstimatedPrice(tc.price, tc.chargeType)
			if tc.want == "" {
				assert.Nil(t, got, "nothing usable was quoted — the card must show no price, not a zero")
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want+estimatedPriceSuffix, got.DisplayText)
		})
	}
}

// --- Platform image selection tests ---

func TestCreateInstance_PlatformImage_DefaultQueryDoesNotForceSystem(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.runCreateTest(def, map[string]any{
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
	_, err := eng.runCreateTest(def, map[string]any{
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

func TestCreateInstance_PlatformImage_WithImageIDUsesExactFilter(t *testing.T) {
	executor := createMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.runCreateTest(def, map[string]any{
		"GpuType":          "4090",
		"ImageName":        "PyTorch",
		"CompShareImageId": "img-exact",
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
	assert.Equal(t, "img-exact", imageArgs["CompShareImageId"], "image id must win over fuzzy name filters")
	assert.NotContains(t, imageArgs, "Name")
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
	result, err := eng.runCreateTest(def, map[string]any{
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
	result, err := eng.runCreateTest(def, map[string]any{
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
	candidates := listSpecCandidates(result, "4090", 1, "")
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
	candidates := listSpecCandidates(result, "A800", 1, "")
	assert.Len(t, candidates, 2)
	assert.Equal(t, specCandidate{CPU: 16, MemoryMB: 64 * 1024}, candidates[0])
	assert.Equal(t, specCandidate{CPU: 32, MemoryMB: 128 * 1024}, candidates[1])
}

func TestResolveTargetSpec_SingleCandidate_AutoSelect(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询可用配比"] = mockInstanceTypes("4090",
		struct{ Gpu, Cpu, MemGB float64 }{1, 16, 64},
	)
	gpu, cpu, mem, _, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(64*1024), mem)
}

// zoneTaggedTypes builds a catalog where each entry carries a Zone + Status, as the
// real (un-MachineTypes-filtered) DescribeAvailableCompShareInstanceTypes returns.
// zoneTaggedTypes is the zone-aware catalog fixture. It carries a BootDisk for the
// same reason mockInstanceTypes does: without one, catalogBootDiskType returns ""
// and ResolveBootDisk returns nil, so every draft built on this fixture carried an
// empty disk list — and a create with no disk is not a create this platform makes.
// Every seal and aliasing test in the package is built on this, so the omission
// made them all disk-blind: the field most able to be shared between the candidate
// and the seal was the one field they never populated.
func zoneTaggedTypes(entries ...struct {
	Name, Zone, Status string
}) map[string]any {
	types := make([]any, 0, len(entries))
	for _, e := range entries {
		types = append(types, map[string]any{
			"Name": e.Name, "Zone": e.Zone, "Status": e.Status,
			"MachineSizes": []any{map[string]any{
				"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				},
			}},
			"Disks": []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
		})
	}
	return map[string]any{"AvailableInstanceTypes": types}
}

func TestResolveTargetSpec_ResolvesNonDefaultZone(t *testing.T) {
	// 2080Ti lives only in cn-sh2-02 (the real cause of the reported bug class):
	// resolveTargetSpec must pick that zone, not fail with the hardcoded default.
	wfCtx := NewContext(map[string]any{"GpuType": "2080Ti"})
	wfCtx.StepResults["查询可用配比"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"4090", "cn-wlcb-01", "Normal"},
		struct{ Name, Zone, Status string }{"2080Ti", "cn-sh2-02", "Normal"},
	)
	gpu, cpu, mem, zone, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(64*1024), mem)
	assert.Equal(t, "cn-sh2-02", zone, "must resolve the zone the GPU actually lives in")
}

func TestResolveTargetSpec_MultiZone_PrefersDefaultZone_NoDuplicates(t *testing.T) {
	// A GPU present in several zones must resolve to the default zone (preference)
	// and yield a single candidate — not one duplicate per zone.
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询可用配比"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"4090", "cn-sh2-02", "Normal"},
		struct{ Name, Zone, Status string }{"4090", "cn-wlcb-01", "Normal"},
	)
	_, _, _, zone, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, "cn-wlcb-01", zone, "must prefer the platform default zone when the GPU is in several")

	// And only one candidate (no cross-zone duplicates) → auto-selects cleanly.
	candidates := listSpecCandidates(wfCtx.StepResults["查询可用配比"], "4090", 1, zone)
	assert.Len(t, candidates, 1, "zone filter must drop the other-zone duplicate")
}

func TestResolveTargetSpec_NoCandidate_ListsRealAvailableTypes(t *testing.T) {
	// A type not in the catalog must fail with a GROUNDED message that lists the
	// real available types — this is what replaces the LLM's fabricated GPU list.
	wfCtx := NewContext(map[string]any{"GpuType": "MI300X"})
	wfCtx.StepResults["查询可用配比"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"4090", "cn-wlcb-01", "Normal"},
		struct{ Name, Zone, Status string }{"V100S", "cn-wlcb-01", "Normal"},
	)
	_, _, _, _, err := resolveTargetSpec(wfCtx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "未找到 MI300X")
	assert.Contains(t, err.Error(), "4090")
	assert.Contains(t, err.Error(), "V100S")
}

func TestCreateInstance_NonDefaultZone_ThreadsZoneToCreate(t *testing.T) {
	// End-to-end: a GPU that exists only in cn-sh2-02 must be created there, with
	// the resolved zone threaded through capacity / price / create.
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = zoneTaggedTypes(
		struct{ Name, Zone, Status string }{"2080Ti", "cn-sh2-02", "Normal"},
	)
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.runCreateTest(def, map[string]any{"GpuType": "2080Ti"})
	assert.NoError(t, err)
	assert.True(t, result.Success, "2080Ti in a non-default zone must create successfully")

	for _, call := range executor.calls {
		if call.action == "CreateCompShareInstance" {
			assert.Equal(t, "cn-sh2-02", call.args["Zone"], "create must target the GPU's real zone")
		}
		if call.action == "CheckCompShareResourceCapacity" {
			assert.Equal(t, "cn-sh2-02", call.args["Zone"], "capacity check must target the GPU's real zone")
		}
	}
}

func TestCreateInstance_PodZoneUsesDynamicZoneIDAndAzGroup(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId": "img-001", "Name": "PyTorch Container", "Size": float64(51200),
			"Status": "Available", "Container": true, "SupportedGpuTypes": []any{"4090"},
		},
	}}
	executor.results["DescribeAvailableCompShareInstanceTypes"] = map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{
			"Name":         "4090",
			"Zone":         "cn-newpod-03",
			"Status":       "Normal",
			"CpuPlatforms": map[string]any{"Amd": map[string]any{}},
			"Disks":        []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_RSSD", "MinimalSize": float64(50)}}}},
			"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}}},
		},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }

	eng := NewEngine(executor, confirmFn, nil)
	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-newpod-03",
	}, withPodZone("cn-newpod-03", "cn-newpod", 9103, 3103))

	assert.NoError(t, err)
	assert.True(t, result.Success)
	var byAction = map[string]map[string]any{}
	for _, call := range executor.calls {
		byAction[call.action] = call.args
	}
	checkArgs := byAction["CheckCompShareResourceCapacity"]
	assert.NotContains(t, checkArgs, "Zone")
	assert.NotContains(t, checkArgs, "Region")
	assert.NotContains(t, checkArgs, "az_group")
	assert.Equal(t, uint32(9103), checkArgs["zone_id"])

	for _, action := range []string{"GetCompShareInstanceUserPrice", "CreateCompShareInstance"} {
		args := byAction[action]
		assert.Equal(t, "cn-newpod-03", args["Zone"], action)
		assert.Equal(t, "cn-newpod", args["Region"], action)
		assert.Equal(t, uint32(9103), args["zone_id"], action)
		assert.Equal(t, uint32(3103), args["az_group"], action)
	}
}

func TestCreateInstance_NormalZoneCapacityKeepsZoneAndRegion(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeAvailableCompShareInstanceTypes"] = map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{
			"Name":         "4090",
			"Zone":         "cn-sh2-02",
			"Status":       "Normal",
			"CpuPlatforms": map[string]any{"Intel": map[string]any{}},
			"Disks":        []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
			"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
				map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
			}}},
		},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }

	eng := NewEngine(executor, confirmFn, nil)
	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-sh2-02",
	}, withNormalZone("cn-sh2-02", "cn-sh2", 2002))

	assert.NoError(t, err)
	assert.True(t, result.Success)
	var checkArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "CheckCompShareResourceCapacity" {
			checkArgs = call.args
		}
	}
	assert.Equal(t, "cn-sh2-02", checkArgs["Zone"])
	assert.Equal(t, "cn-sh2", checkArgs["Region"])
	assert.Equal(t, uint32(2002), checkArgs["zone_id"])
}

// The catalog query must NOT be scoped to the Spot pool. InstanceType=spot is a
// value upstream accepts and then answers empty (measured live 2026-07-22:
// rows=19 for absent/uhost/all, rows=0 for spot), and an empty catalog makes
// resolveTargetSpec fail listing no GPU types at all — it breaks every Spot
// create. ChargeType still has to reach the capacity call, which is the request
// upstream actually validates it on. A mock returns rows either way, so this
// asserts the ARGUMENT, not the mock's answer.
func TestCreateInstance_FullySpecifiedPodSpotDoesNotScopeCatalogButScopesCapacity(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-pod", "Name": "Pod CUDA", "Container": "True", "Size": float64(102400)},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType":    "4090",
		"Zone":       "cn-newpod-03",
		"ChargeType": "Spot",
	}, withPodZone("cn-newpod-03", "cn-newpod", 9103, 3103))

	assert.NoError(t, err)
	assert.True(t, result.Success)
	var catalogArgs, capacityArgs map[string]any
	for _, call := range executor.calls {
		if call.action == "DescribeAvailableCompShareInstanceTypes" {
			catalogArgs = call.args
		}
		if call.action == "CheckCompShareResourceCapacity" {
			capacityArgs = call.args
		}
	}
	assert.NotContains(t, catalogArgs, "InstanceType",
		"scoping the catalog to the Spot pool returns an empty catalog and breaks every Spot create")
	assert.Equal(t, deployment.ChargeTypeSpot, capacityArgs["ChargeType"],
		"the capacity call is where upstream actually validates the charge type")
}

func TestCreateInstance_UnsupportedImageBlocksBeforeCapacity(t *testing.T) {
	executor := createMockExecutor()
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId":  "img-v100-only",
			"Name":              "V100 专用镜像",
			"SupportedGpuTypes": []any{"V100S"},
			"Size":              float64(102400),
		},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "形成执行草稿", result.StoppedAt)
	assert.Contains(t, result.Message, "不支持当前 GPU")
	for _, call := range executor.calls {
		assert.NotEqual(t, "CheckCompShareResourceCapacity", call.action)
		assert.NotEqual(t, "CreateCompShareInstance", call.action)
	}
}

func TestResolveTargetSpec_MultiCandidate_NoCpuMem_DefaultsToFirst(t *testing.T) {
	// Multiple candidates, user gave neither Cpu nor Memory → default to first.
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	gpu, cpu, mem, _, err := resolveTargetSpec(wfCtx)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), gpu)
	assert.Equal(t, float64(16), cpu)
	assert.Equal(t, float64(64*1024), mem, "should default to first candidate (64GB)")
}

func TestResolveTargetSpec_MultiCandidate_OnlyCpu_StillAmbiguous(t *testing.T) {
	// Both candidates have Cpu=16 → filtering by Cpu alone doesn't resolve.
	wfCtx := NewContext(map[string]any{"GpuType": "4090", "Cpu": float64(16)})
	wfCtx.StepResults["查询可用配比"] = multiMemoryInstanceTypes()

	_, _, _, _, err := resolveTargetSpec(wfCtx)
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

	gpu, cpu, mem, _, err := resolveTargetSpec(wfCtx)
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

	_, _, _, _, err := resolveTargetSpec(wfCtx)
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

	gpu, cpu, mem, _, err := resolveTargetSpec(wfCtx)
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
	result, err := eng.runCreateTest(def, map[string]any{
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
	result, err := eng.runCreateTest(def, map[string]any{
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
	_, err := eng.runCreateTest(def, map[string]any{
		"GpuType": "4090",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, float64(1), capturedArgs["Gpu"])
	assert.Equal(t, float64(16), capturedArgs["CPU"])
	assert.Equal(t, float64(65536), capturedArgs["Memory"])
}
