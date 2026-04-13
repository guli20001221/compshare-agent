package tools

import openai "github.com/sashabaranov/go-openai"

// Registry holds all registered tools for function calling.
var Registry = []openai.Tool{
	// --- Knowledge Tools (local, no API call) ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetGPUSpecs",
			Description: "查询 GPU 型号的详细规格参数（显存、算力、最大卡数、适用场景等）。不传 GpuType 则返回所有 GPU 规格概览。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型，如 4090 / A100 / H20 / 3090 等。不传则返回全部 GPU 规格。",
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
			Description: "查询用户的算力共享实例列表及详情，包括实例状态、GPU 类型、IP 地址、计费方式等。不传 UHostIds 则查询全部实例。",
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
			Name:        "GetCompShareInstancePrice",
			Description: "查询创建算力共享实例的价格，返回按量/包日/包月/抢占式等多种计费方式的分项价格（实例、磁盘、镜像）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，如 cn-wlcb-a",
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
			Description: "检查指定 GPU 类型和配置的库存是否充足，返回各规格组合的可用性。建议在创建实例前调用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区",
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
			Description: "查询可用的算力共享镜像列表，包括官方镜像、自定义镜像等。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ImageType": map[string]any{
						"type":        "string",
						"description": "镜像类型：System(平台公共镜像) / Custom(自定义镜像) / App(应用镜像)，不传则返回全部",
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
	// --- Workflow Meta-Tools ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CreateInstanceWorkflow",
			Description: "创建实例的完整工作流。自动执行：查询镜像→检查库存→查询价格→用户确认→创建实例→查看状态。用户要求创建实例时必须使用此工具，不要直接调用 CreateCompShareInstance。",
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
						"description": "可用区，默认 cn-wlcb-a",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Dynamic(按量) / Month(包月) / Day(包日) / Spot(抢占式)，默认 Dynamic",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "实例名称（可选）",
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
			Description: "开机工作流。用户要求开机时使用此工具。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要开机的实例 ID",
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
}
