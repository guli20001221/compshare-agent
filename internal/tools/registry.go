package tools

import openai "github.com/sashabaranov/go-openai"

// Registry holds all registered tools for function calling.
var Registry = []openai.Tool{
	// --- Knowledge Tools (local, no API call) ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetGPUSpecs",
			Description: "查询 GPU 型号的概览规格参数（显存、算力、最大卡数、适用场景等），不展开控制台全部 CPU/内存/GPU 合法组合。用户明确要求所有/完整规格或某型号所有配置时，应使用 DescribeAvailableCompShareInstanceTypes。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型，如 4090 / A100 / H20 / 3090 等。不传则返回全部 GPU 概览规格。",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetGPURecommendation",
			Description: "根据使用场景推荐最合适的 GPU 配置。支持的场景包括：推理/部署、LoRA微调、全量训练、SD/ComfyUI绘图、学习入门等。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scene": map[string]any{
						"type":        "string",
						"description": "使用场景描述，如 '训练7B模型'、'部署vLLM'、'跑SD绘图'、'学习入门' 等",
					},
					"budget_sensitive": map[string]any{
						"type":        "boolean",
						"description": "是否对价格敏感，为 true 时优先推荐性价比高的选项",
					},
				},
				"required": []string{"scene"},
			},
		},
	},
	// --- External API Tools ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareInstance",
			Description: "查询用户自己账号下的算力共享实例列表及详情，不用于查询机房库存或平台是否还有 GPU 可售。返回实例状态（Running/Stopped/Install/Install Fail/Starting/Stopping/Rebooting）、GPU 类型、IP、计费等。不传 UHostIds 查全部。Limit 最大 100。State 含义：Install=初始化中, Install Fail=初始化失败。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "实例 ID 列表，不传则查询全部",
					},
					"Limit": map[string]any{
						"type":        "integer",
						"description": "分页大小，默认 20，最大 100",
					},
					"Offset": map[string]any{
						"type":        "integer",
						"description": "分页偏移，默认 0",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeAvailableCompShareInstanceTypes",
			Description: "获取可用 GPU 机型列表及每种机型的合法 CPU/内存/GPU 组合。用于回答所有/完整 GPU 规格、某型号所有规格、CPU/内存组合、可选配置，也可用于回答 GPU 机型是否可售/是否售罄；返回 Status（Normal/SoldOut），不返回精确剩余数量。注意：返回的 Memory 单位为 GB，创建实例时需转换为 MB。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，如 cn-wlcb-01",
					},
					"MachineTypes": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "按完整机型名称精确筛选，如 [\"H20\"]。用户问 4090 的所有规格、RTX40 系列、某型号家族/变体时不要传此参数，应先查全量再保留 4090、4090_48G 等相关变体，避免漏规格。",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareInstancePrice",
			Description: "查询创建实例的价格。返回按量/包日/包月/抢占式等分项价格（实例、磁盘、镜像）。Zone 格式为 cn-wlcb-01。Memory 单位为 MB（如 65536 = 64GB）。不传 ChargeType 则返回所有计费方式的价格。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，如 cn-wlcb-01",
					},
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型：4090 / 5090 / A100 / A800 / H20 / 3080Ti / 3090 / P40 / 2080Ti / 2080 / V100S 等",
					},
					"Gpu": map[string]any{
						"type":        "integer",
						"description": "GPU 数量",
					},
					"Cpu": map[string]any{
						"type":        "integer",
						"description": "CPU 核数",
					},
					"Memory": map[string]any{
						"type":        "integer",
						"description": "内存大小，单位 MB",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Month / Day / Dynamic / Postpay / Spot，不传则返回所有方式",
						"enum":        []string{"Month", "Day", "Dynamic", "Postpay", "Spot"},
					},
				},
				"required": []string{"Zone", "GpuType", "Gpu", "Cpu", "Memory"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CheckCompShareResourceCapacity",
			Description: "预检某个具体创建实例配置是否有足够资源，适合在用户已给出 GPU/CPU/内存/镜像/计费方式等创建参数时使用；也可在库存问题已识别 GPU 型号并拿到可用区后，确认该机型当前是否真实可创建。Zone 必须为 cn-wlcb-01 格式。MachineType 固定传 G。MinimalCpuPlatform 传 Auto（或 Intel/Auto、Amd/Auto）。CompShareImageId 和 ChargeType 必填。Disks 至少包含一个系统盘，如 [{IsBoot:true, Type:CLOUD_SSD, Size:60}]。返回各 GPU/CPU/Memory 组合的可用性。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，如 cn-wlcb-01",
					},
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型",
					},
					"MachineType": map[string]any{
						"type":        "string",
						"description": "固定为 G",
						"default":     "G",
					},
					"MinimalCpuPlatform": map[string]any{
						"type":        "string",
						"description": "CPU 平台：Intel/Auto, Amd/Auto, Auto",
						"default":     "Auto",
					},
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "镜像 ID",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式",
					},
					"Disks": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"IsBoot": map[string]any{"type": "boolean"},
								"Type":   map[string]any{"type": "string"},
								"Size":   map[string]any{"type": "integer"},
							},
						},
						"description": "磁盘配置",
					},
				},
				"required": []string{"Zone", "GpuType", "CompShareImageId", "ChargeType"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareImages",
			Description: "查询平台镜像列表。ImageType 枚举：System（系统镜像，裸 Ubuntu/Windows）、App（应用基础镜像，如 PyTorch/CUDA/ComfyUI/Ollama），不传返回全部。查自制镜像请用 DescribeCompShareCustomImages，查社区镜像请用 DescribeCommunityImages。不用于查库存。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "按镜像 ID 精确查询",
					},
					"ImageType": map[string]any{
						"type":        "string",
						"description": "镜像类型：System(系统镜像) / App(应用基础镜像)，不传则返回全部",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "按镜像名称筛选，如 PyTorch / Ubuntu / CUDA",
					},
					"Limit": map[string]any{
						"type":        "integer",
						"description": "返回数据长度，默认 20",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareSoftwarePort",
			Description: "查询平台支持的软件及其端口映射列表（SSH、JupyterLab、FileBrowser 等）。用于诊断端口连通性问题。仅需 Region 参数（自动填充）。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareInstanceMonitor",
			Description: "Get current instance monitor data such as CPU, memory, GPU, and VRAM utilization. Pass UHostIds only. Do not pass historical time-window fields; historical monitor windows are not enabled in this stage.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "实例 ID 列表（必填）",
					},
				},
				"required": []string{"UHostIds"},
			},
		},
	},
	// --- Additional API Tools (Phase 2) ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareCustomImages",
			Description: "查询用户自制镜像列表（仅查询，不进入创建主链路）。返回用户自己制作的镜像，包含 CompShareImageId、Name、Status 等字段。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "镜像 ID（可选，传则查特定镜像）",
					},
					"Offset": map[string]any{
						"type":        "integer",
						"description": "分页偏移，默认 0",
					},
					"Limit": map[string]any{
						"type":        "integer",
						"description": "返回数据长度，默认 20",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCommunityImages",
			Description: "查询社区镜像列表。支持按名称/作者/标签/模糊搜索筛选。返回 CompshareImageGroup 分组结构，每组含 Data 版本数组。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Name": map[string]any{
						"type":        "string",
						"description": "镜像名称筛选",
					},
					"Author": map[string]any{
						"type":        "string",
						"description": "作者昵称，精确搜索",
					},
					"FuzzySearch": map[string]any{
						"type":        "string",
						"description": "模糊搜索关键词（支持镜像名和作者昵称）",
					},
					"Tag": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "标签筛选",
					},
					"Offset": map[string]any{
						"type":        "integer",
						"description": "分页偏移，默认 0",
					},
					"Limit": map[string]any{
						"type":        "integer",
						"description": "返回数据长度，默认 20",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareJupyterToken",
			Description: "获取实例 Jupyter 访问 Token。传 UHostIds 数组但仅使用第一个元素。返回的 JupyterToken 是敏感数据，必须脱敏。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "实例 ID 列表（仅用首元素）",
					},
				},
				"required": []string{"UHostIds"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareInstanceUserPrice",
			Description: "查用户折后价/实际价格。返回 PriceDetails（折后）、OriginalPriceDetails（原价）、ListPriceDetails（目录价）三组明细。计费方式用 Postpay（等同于按量 Dynamic）。参数 GPU/CPU 大写。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，如 cn-wlcb-01",
					},
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型：4090 / 5090 / A100 / A800 / H20 / 3090 等",
					},
					"GPU": map[string]any{
						"type":        "integer",
						"description": "GPU 数量（注意大写）",
					},
					"CPU": map[string]any{
						"type":        "integer",
						"description": "CPU 核数（注意大写）",
					},
					"Memory": map[string]any{
						"type":        "integer",
						"description": "内存大小，单位 MB",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Month / Day / Postpay / Spot，按量用 Postpay（不是 Dynamic）",
						"enum":        []string{"Month", "Day", "Postpay", "Spot"},
					},
				},
				"required": []string{"Zone", "GpuType", "GPU", "CPU", "Memory"},
			},
		},
	},
	// --- Workflow Meta-Tools ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CreateInstanceWorkflow",
			Description: "创建实例的完整工作流。自动执行：查询镜像→检查库存→查询价格→用户确认→创建实例→查看状态。支持平台镜像和社区镜像。平台镜像默认查询公共镜像（含系统镜像和应用基础镜像如 PyTorch/CUDA 等）。传 ImageName 可按名称缩小镜像范围（平台和社区均可用）。传 ImageSource='community' 使用社区镜像创建。不支持自制/私有镜像。用户要求创建实例时必须使用此工具，不要直接调用 CreateCompShareInstance。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型：4090 / A100 / H20 / 3090 等",
					},
					"Gpu": map[string]any{
						"type":        "number",
						"description": "GPU 数量，默认 1",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，默认 cn-wlcb-01",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Dynamic(按量) / Month(包月) / Day(包日) / Spot(抢占式)，默认 Dynamic",
					},
					"Cpu": map[string]any{
						"type":        "number",
						"description": "CPU 核数（可选）。不指定时使用平台默认值。需与 Memory 一起构成合法配比，可通过 DescribeAvailableCompShareInstanceTypes 查询。",
					},
					"Memory": map[string]any{
						"type":        "number",
						"description": "内存大小，单位 MB（可选）。不指定时使用平台默认值。如 64GB = 65536。需与 Cpu 一起构成合法配比。",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "实例名称（可选）",
					},
					"ImageSource": map[string]any{
						"type":        "string",
						"description": "镜像来源：platform（平台镜像，默认）/ community（社区镜像）",
						"enum":        []string{"platform", "community"},
					},
					"ImageName": map[string]any{
						"type":        "string",
						"description": "镜像名称关键词。平台镜像按 Name 精确/模糊匹配；社区镜像用于 FuzzySearch。如 PyTorch / Ubuntu / ComfyUI。",
					},
				},
				"required": []string{"GpuType"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "StopInstanceWorkflow",
			Description: "关机工作流。会提醒用户关机后磁盘仍然收费。用户要求关机时使用此工具。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要关机的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "StartInstanceWorkflow",
			Description: "开机工作流。用户要求开机时使用此工具。支持无卡模式（WithoutGpu=true）：不分配 GPU，仅用于数据拷贝或维护，费用更低。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要开机的实例 ID",
					},
					"WithoutGpu": map[string]any{
						"type":        "boolean",
						"description": "无卡模式开机，不分配 GPU，仅用于数据访问/维护，费用更低。默认 false（正常带卡开机）。",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "RebootInstanceWorkflow",
			Description: "重启实例工作流。检查状态→确认→重启。仅 Running 状态可重启。会中断当前运行的任务。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要重启的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "RenameInstanceWorkflow",
			Description: "重命名实例工作流。确认→修改名称。名称最长63字符，支持中英文、数字、下划线等。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要改名的实例 ID",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "新的实例名称",
					},
				},
				"required": []string{"UHostId", "Name"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "ResetPasswordWorkflow",
			Description: "重置实例密码工作流。普通主机需先关机，容器实例支持在线重置。密码要求8-32字符，至少2种字符类型（大小写字母/数字/特殊字符）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要重置密码的实例 ID",
					},
					"Password": map[string]any{
						"type":        "string",
						"description": "新密码（明文，系统会自动 base64 编码）",
					},
				},
				"required": []string{"UHostId", "Password"},
			},
		},
	},
	// --- Scheduled Shutdown Workflows ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "SetStopSchedulerWorkflow",
			Description: "设置定时关机工作流。为运行中的实例设置自动关机时间。支持相对时间（如30分钟后）或绝对时间。抢占式实例不支持。用户要求定时关机、自动关机、延时关机时使用此工具。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要设置定时关机的实例 ID",
					},
					"AfterMinutes": map[string]any{
						"type":        "number",
						"description": "几分钟后关机（正整数，最小 5）。与 ShutdownAt 二选一。如：60 表示 1 小时后关机。",
					},
					"ShutdownAt": map[string]any{
						"type":        "string",
						"description": "指定关机时间。支持格式：2026-04-16 23:00（按北京时间解析）或 RFC3339。与 AfterMinutes 二选一。",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CancelStopSchedulerWorkflow",
			Description: "取消定时关机工作流。取消实例已设置的定时关机任务。用户要求取消定时关机、取消自动关机时使用此工具。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要取消定时关机的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	// --- Additional Read-Only Tools ---
	//
	// DescribeCompShareSupportZone and CheckCompShareNetOptimizer are registered
	// in security/levels.go (L0) but NOT exposed to the LLM. They are internal
	// APIs called by handlers/diagnosis chains, not user-facing tools.
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareInstanceUpgradePrice",
			Description: "查询实例变配（升降级 CPU/GPU/内存）的价格差额。用于变配前展示费用变化。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要变配的实例 ID",
					},
					"CPU": map[string]any{
						"type":        "number",
						"description": "目标 CPU 核数",
					},
					"GPU": map[string]any{
						"type":        "number",
						"description": "目标 GPU 数量",
					},
					"Memory": map[string]any{
						"type":        "number",
						"description": "目标内存大小（MB）",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	// --- Additional Workflow Tools ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "ResizeInstanceWorkflow",
			Description: "实例变配工作流。修改实例的 CPU/GPU/内存配置。实例必须处于关机状态。用户要求'加卡'、'升级配置'、'加内存'时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要变配的实例 ID",
					},
					"Cpu": map[string]any{
						"type":        "number",
						"description": "目标 CPU 核数",
					},
					"Gpu": map[string]any{
						"type":        "number",
						"description": "目标 GPU 数量",
					},
					"Memory": map[string]any{
						"type":        "number",
						"description": "目标内存大小（MB）",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "ReinstallInstanceWorkflow",
			Description: "重装系统工作流。将实例重装为指定镜像，系统盘数据会被清除。用户要求'换镜像'、'重装系统'、'换成 Ubuntu'时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要重装的实例 ID",
					},
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "目标镜像 ID",
					},
					"Password": map[string]any{
						"type":        "string",
						"description": "新的登录密码（可选，不传则保留原密码）",
					},
				},
				"required": []string{"UHostId", "CompShareImageId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CreateDiskWorkflow",
			Description: "创建并挂载数据盘工作流。为实例创建一块新的云数据盘并自动挂载。用户要求'加数据盘'、'加磁盘'、'磁盘不够'时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要挂载数据盘的实例 ID",
					},
					"Size": map[string]any{
						"type":        "number",
						"description": "磁盘大小（GB），如 100",
					},
				},
				"required": []string{"UHostId", "Size"},
			},
		},
	},
	// --- Diagnosis Meta-Tools ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DiagnoseSSH",
			Description: "诊断 SSH 连接失败。自动执行：检查实例状态与 DescribeCompShareInstance 返回的 SshLoginCommand → 检查资源使用 → 给出结论、只读自查命令和建议。用户反馈 SSH 连不上、连接超时、连接被拒时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要诊断的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DiagnoseInitFailure",
			Description: "诊断实例初始化失败。检查实例当前状态并给出修复建议。用户反馈创建失败、初始化失败、实例异常时使用。可传 UHostId 查特定实例，不传则扫描所有实例找出初始化失败的。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要诊断的实例 ID（可选，不传则扫描所有实例）",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DiagnoseGPU",
			Description: "诊断 GPU 检测不到问题（nvidia-smi 报错）。自动执行：检查实例状态与 GPU 配置 → 检查 GPU 监控数据 → 给出结论和建议。用户反馈 nvidia-smi 报错、GPU 找不到、显卡无法识别时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要诊断的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DiagnoseBilling",
			Description: "诊断费用异常。查询实例列表并分析各项费用明细，解释扣费原因。用户反馈为什么扣这么多钱、费用不对、扣费异常时使用。可传 UHostId 查特定实例，不传则分析所有实例。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要诊断的实例 ID（可选，不传则分析所有实例）",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DiagnosePortOrFirewall",
			Description: "诊断端口/服务可达性问题。先查实例应用入口，再查询平台已知应用端口映射，给出排查线索；SSH 以实例 SshLoginCommand 为准，不以平台应用端口目录为准。用户报告服务无法访问、端口不通、JupyterLab/SSH/FileBrowser 打不开时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要诊断的实例 ID",
					},
					"Service": map[string]any{
						"type":        "string",
						"description": "目标服务名（可选，如 JupyterLab、SSH、FileBrowser，支持别名和大小写不敏感）",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DiagnoseImageIssue",
			Description: "诊断镜像问题。镜像无法使用、启动异常、环境不符、初始化失败疑似镜像原因时使用。自动检查实例状态和镜像类型，区分社区镜像与官方镜像给出建议。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要诊断的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
}

// VisibleRegistryForSubset returns a filtered tool list scoped to the given
// tool names. If subset is nil or empty, falls back to VisibleRegistry (full
// read-only or mutating set). Used by the ReAct loop when the planner
// classified an intent that has a defined tool subset (e.g. diagnosis).
func VisibleRegistryForSubset(subset []string, mutatingEnabled bool) []openai.Tool {
	if len(subset) == 0 {
		return VisibleRegistry(mutatingEnabled)
	}
	allowed := make(map[string]struct{}, len(subset))
	for _, name := range subset {
		allowed[name] = struct{}{}
	}
	base := VisibleRegistry(mutatingEnabled)
	visible := make([]openai.Tool, 0, len(subset))
	for _, tool := range base {
		if tool.Function == nil {
			continue
		}
		if _, ok := allowed[tool.Function.Name]; ok {
			visible = append(visible, tool)
		}
	}
	return visible
}

// VisibleRegistry returns the tool list exposed to the LLM for the current
// runtime mode. Read-only mode hides mutating workflow tools while keeping
// query, knowledge, and cloud-side diagnosis tools available.
func VisibleRegistry(mutatingEnabled bool) []openai.Tool {
	if mutatingEnabled {
		return Registry
	}
	policies := DefaultToolExecutionPolicies()
	visible := make([]openai.Tool, 0, len(Registry))
	for _, tool := range Registry {
		if tool.Function == nil {
			continue
		}
		policy, ok := policies[tool.Function.Name]
		if ok && (policy.Route == ActionRouteWorkflow || policy.Class == ActionClassMutating) {
			continue
		}
		visible = append(visible, tool)
	}
	return visible
}
