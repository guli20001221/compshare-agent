package workflow

import "fmt"

// defaultZone is the default availability zone per API docs (cn-wlcb-01, not cn-wlcb-a).
const defaultZone = "cn-wlcb-01"

// defaultDisk is the minimum required disk configuration for instance creation.
// The system disk has a 200GB free tier on CompShare.
var defaultDisk = []any{
	map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": 60},
}

// CreateInstanceDef returns the 6-step workflow definition for creating a
// CompShare GPU instance.
func CreateInstanceDef() *Definition {
	return &Definition{
		Name:        "CreateInstanceWorkflow",
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
// Step definitions (params aligned with docs/api/ specs)
// ---------------------------------------------------------------------------

func stepQueryImages() Step {
	return Step{
		Name: "查询镜像",
		Type: StepToolCall,
		Tool: "DescribeCompShareImages",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			// API accepts: ImageType, Name, Author, Tag, Offset, Limit (Zone optional)
			return map[string]any{
				"ImageType": "System",
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
			// All fields below are required per docs/api/spec/CheckCompShareResourceCapacity.md
			return map[string]any{
				"Zone":               paramStr(wfCtx.Params, "Zone", defaultZone),
				"GpuType":            wfCtx.Params["GpuType"],
				"MachineType":        "G",
				"MinimalCpuPlatform": "Auto",
				"CompShareImageId":   imageId,
				"ChargeType":         paramStr(wfCtx.Params, "ChargeType", "Dynamic"),
				"Disks":              defaultDisk,
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
				"Zone":    paramStr(wfCtx.Params, "Zone", defaultZone),
				"GpuType": wfCtx.Params["GpuType"],
				"Gpu":     paramNum(wfCtx.Params, "Gpu", 1),
				"Cpu":     paramNum(wfCtx.Params, "Cpu", 16),
				"Memory":  paramNum(wfCtx.Params, "Memory", 65536),
			}, nil
		},
	}
}

func stepConfirmCreate() Step {
	return Step{
		Name: "确认创建",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"workflow":    "CreateInstanceWorkflow",
				"GpuType":    wfCtx.Params["GpuType"],
				"Gpu":        paramNum(wfCtx.Params, "Gpu", 1),
				"Zone":       paramStr(wfCtx.Params, "Zone", defaultZone),
				"ChargeType": paramStr(wfCtx.Params, "ChargeType", "Dynamic"),
				"image":      pickFirstImageName(wfCtx.Result("查询镜像")),
				"price":      wfCtx.Result("查询价格"),
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
			// Required fields per docs/api/instance/CreateCompShareInstance.md
			args := map[string]any{
				"Zone":             paramStr(wfCtx.Params, "Zone", defaultZone),
				"GpuType":         wfCtx.Params["GpuType"],
				"GPU":             paramNum(wfCtx.Params, "Gpu", 1),
				"CPU":             paramNum(wfCtx.Params, "Cpu", 16),
				"Memory":          paramNum(wfCtx.Params, "Memory", 65536),
				"CompShareImageId": imageId,
				"ChargeType":      paramStr(wfCtx.Params, "ChargeType", "Dynamic"),
				"Disks":           defaultDisk,
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
			}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func paramStr(params map[string]any, key, defaultVal string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultVal
}

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
	// API doc field: CompShareImageId
	if id, ok := first["CompShareImageId"].(string); ok {
		return id
	}
	return ""
}

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
	if name, ok := first["Name"].(string); ok && name != "" {
		return name
	}
	return "未知"
}
