package workflow

import "fmt"

// CreateInstanceDef returns the 6-step workflow definition for creating a
// CompShare GPU instance.
func CreateInstanceDef() *Definition {
	return &Definition{
		Name:        "创建算力实例",
		Description: "查询镜像 → 检查库存 → 查询价格 → 确认 → 创建实例 → 查看状态",
		Steps: []Step{
			stepQueryImages(),
			stepCheckCapacity(),
			stepGetPrice(),
			stepConfirmCreate(),
			stepCreateInstance(),
			stepDescribeInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

func stepQueryImages() Step {
	return Step{
		Name: "查询镜像",
		Type: StepToolCall,
		Tool: "DescribeCompShareImages",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"GpuType": wfCtx.Params["GpuType"],
				"Zone":    paramStr(wfCtx.Params, "Zone", "cn-wlcb-a"),
			}, nil
		},
	}
}

func stepCheckCapacity() Step {
	return Step{
		Name: "检查库存",
		Type: StepToolCall,
		Tool: "CheckCompShareResourceCapacity",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			imageId := pickFirstImageId(wfCtx.Result("查询镜像"))
			return map[string]any{
				"GpuType": wfCtx.Params["GpuType"],
				"Zone":    paramStr(wfCtx.Params, "Zone", "cn-wlcb-a"),
				"Gpu":     paramNum(wfCtx.Params, "Gpu", 1),
				"ImageId": imageId,
			}, nil
		},
	}
}

func stepGetPrice() Step {
	return Step{
		Name: "查询价格",
		Type: StepToolCall,
		Tool: "GetCompShareInstancePrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"GpuType":    wfCtx.Params["GpuType"],
				"Zone":       paramStr(wfCtx.Params, "Zone", "cn-wlcb-a"),
				"Gpu":        paramNum(wfCtx.Params, "Gpu", 1),
				"Cpu":        paramNum(wfCtx.Params, "Cpu", 16),
				"Memory":     paramNum(wfCtx.Params, "Memory", 65536),
				"ChargeType": paramStr(wfCtx.Params, "ChargeType", "Dynamic"),
			}, nil
		},
	}
}

func stepConfirmCreate() Step {
	return Step{
		Name: "确认创建",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			imageName := pickFirstImageName(wfCtx.Result("查询镜像"))
			return map[string]any{
				"Workflow":   "创建算力实例",
				"GpuType":    wfCtx.Params["GpuType"],
				"Gpu":        paramNum(wfCtx.Params, "Gpu", 1),
				"Zone":       paramStr(wfCtx.Params, "Zone", "cn-wlcb-a"),
				"ChargeType": paramStr(wfCtx.Params, "ChargeType", "Dynamic"),
				"ImageName":  imageName,
				"PriceResult": wfCtx.Result("查询价格"),
			}, nil
		},
	}
}

func stepCreateInstance() Step {
	return Step{
		Name: "创建实例",
		Type: StepToolCall,
		Tool: "CreateCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			imageId := pickFirstImageId(wfCtx.Result("查询镜像"))
			args := map[string]any{
				"GpuType":    wfCtx.Params["GpuType"],
				"Zone":       paramStr(wfCtx.Params, "Zone", "cn-wlcb-a"),
				"Gpu":        paramNum(wfCtx.Params, "Gpu", 1),
				"Cpu":        paramNum(wfCtx.Params, "Cpu", 16),
				"Memory":     paramNum(wfCtx.Params, "Memory", 65536),
				"ChargeType": paramStr(wfCtx.Params, "ChargeType", "Dynamic"),
				"ImageId":    imageId,
			}
			if name, ok := wfCtx.Params["Name"]; ok {
				args["Name"] = name
			}
			return args, nil
		},
	}
}

func stepDescribeInstance() Step {
	return Step{
		Name: "查看状态",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			createResult := wfCtx.Result("创建实例")
			ids, ok := createResult["UHostIds"].([]any)
			if !ok || len(ids) == 0 {
				return nil, fmt.Errorf("创建实例未返回 UHostIds")
			}
			return map[string]any{
				"UHostIds": ids,
				"Zone":     paramStr(wfCtx.Params, "Zone", "cn-wlcb-a"),
			}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// paramStr returns params[key] as a string, or defaultVal if missing/wrong type.
func paramStr(params map[string]any, key, defaultVal string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultVal
}

// paramNum returns params[key] as a float64, or defaultVal if missing/wrong type.
func paramNum(params map[string]any, key string, defaultVal float64) float64 {
	if v, ok := params[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return defaultVal
}

// pickFirstImageId extracts the first ImageId from a DescribeCompShareImages result.
func pickFirstImageId(result map[string]any) string {
	if result == nil {
		return ""
	}
	imageSet, ok := result["ImageSet"].([]any)
	if !ok || len(imageSet) == 0 {
		return ""
	}
	first, ok := imageSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := first["ImageId"].(string); ok {
		return id
	}
	return ""
}

// pickFirstImageName extracts the first ImageName, returning "未知" if missing.
func pickFirstImageName(result map[string]any) string {
	if result == nil {
		return "未知"
	}
	imageSet, ok := result["ImageSet"].([]any)
	if !ok || len(imageSet) == 0 {
		return "未知"
	}
	first, ok := imageSet[0].(map[string]any)
	if !ok {
		return "未知"
	}
	if name, ok := first["ImageName"].(string); ok {
		return name
	}
	return "未知"
}
