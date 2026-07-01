package workflow

import "errors"

const createDiskMissingSizeMessage = "创建数据盘需要指定磁盘大小（GB）。请告诉我要加多大的数据盘，例如 30GB。"

func CreateDiskDef() *Definition {
	return &Definition{
		Name:        "CreateDiskWorkflow",
		Description: "查询实例 → 查询数据盘价格 → 确认创建数据盘 → 创建并挂载",
		Steps: []Step{
			stepQueryForDisk(),
			stepQueryCreateDiskPrice(),
			stepConfirmCreateDisk(),
			stepCreateAndAttachDisk(),
		},
	}
}

func stepQueryForDisk() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			size := paramNum(wfCtx.Params, "Size", 0)
			if size <= 0 {
				return nil, NewMissingSlotError(createDiskMissingSizeMessage, "size_gb")
			}
			wfCtx.Params["Size"] = size
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(wfCtx *Context, result map[string]any) (bool, string) {
			uhostID, _ := wfCtx.Params["UHostId"].(string)
			if uhostID != "" && !narrowInstanceResultToUHostID(result, uhostID) {
				return false, "未找到该实例。"
			}
			state := extractInstanceState(result)
			if state == "" {
				return false, "未找到该实例。"
			}
			if isPodInstanceResult(result) || isContainerInstanceResult(result) {
				return false, "Pod/容器 Pod 实例不支持普通新建数据盘。可改用系统盘扩容，或使用平台支持的共享存储能力。"
			}
			return true, ""
		},
	}
}

func stepQueryCreateDiskPrice() Step {
	return Step{
		Name: "查询数据盘价格",
		Type: StepToolCall,
		Tool: "GetCompShareInstancePrice",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			args := map[string]any{
				"ChargeType": "Postpay",
				"Disks": []any{map[string]any{
					"IsBoot": false,
					"Type":   "CLOUD_SSD",
					"Size":   wfCtx.Params["Size"],
				}},
			}
			if _, err := addRequiredInstanceLocationArgs(args, queried); err != nil {
				return nil, err
			}
			if err := addSourceInstanceSpecForDiskPrice(args, queried); err != nil {
				return nil, err
			}
			return args, nil
		},
	}
}

func addSourceInstanceSpecForDiskPrice(args map[string]any, result map[string]any) error {
	host, ok := firstInstance(result)
	if !ok {
		return errors.New("未获取到源实例规格，无法安全查询数据盘价格。")
	}
	gpuType := stringFieldAny(host["GpuType"])
	if gpuType == "" {
		gpuType = stringFieldAny(host["GPUType"])
	}
	gpu := firstNumberField(host, "GPU", "Gpu")
	cpu := firstNumberField(host, "CPU", "Cpu")
	memory := firstNumberField(host, "Memory", "Mem")
	if gpuType == "" || gpu <= 0 || cpu <= 0 || memory <= 0 {
		return errors.New("未获取到源实例完整规格，无法安全查询数据盘价格。")
	}
	args["GpuType"] = gpuType
	args["Gpu"] = gpu
	args["Cpu"] = cpu
	args["Memory"] = memory
	return nil
}

func firstNumberField(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if n, ok := priceNumber(m[key]); ok {
			return n
		}
	}
	return 0
}

func stepConfirmCreateDisk() Step {
	return Step{
		Name: "确认创建数据盘",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			price, err := requiredPriceField(wfCtx.Result("查询数据盘价格"), "Disks")
			if err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["disk_size_gb"] = wfCtx.Params["Size"]
			summary["disk_type"] = "SSDDataDisk"
			summary["charge_type"] = "Postpay"
			summary["price"] = price
			summary["warning"] = "将创建一块 SSD 云数据盘并挂载到该实例，按量计费。"
			return summary, nil
		},
	}
}

func stepCreateAndAttachDisk() Step {
	return Step{
		Name: "创建并挂载数据盘",
		Type: StepToolCall,
		Tool: "CreateAndAttachCompshareDisk",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			queried := wfCtx.Result("查询实例")
			name := extractInstanceName(queried)
			if name == "" {
				name = "data-disk"
			}
			args := map[string]any{
				"UHostId":    wfCtx.Params["UHostId"],
				"Size":       wfCtx.Params["Size"],
				"Name":       name + "-data",
				"DiskType":   "SSDDataDisk",
				"ChargeType": "Postpay",
			}
			return addRequiredInstanceLocationArgs(args, queried)
		},
	}
}
