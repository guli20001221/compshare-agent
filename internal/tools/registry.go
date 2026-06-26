package tools

import openai "github.com/sashabaranov/go-openai"

// agenticSearchKnowledgeOn gates the agentic-RAG SearchKnowledge tool (P3) in
// VisibleRegistry. Default false => byte-identical to before the tool existed,
// for EVERY intent. Set once at boot from COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE
// (cmd/trace.go). The flag is read at the single tool-visibility choke point
// (VisibleRegistry/VisibleRegistryForSubset), so that filter IS the gate
// (unified plan P3, gating design 2 — full byte-identity when off).
var agenticSearchKnowledgeOn bool

// SetAgenticSearchKnowledgeEnabled toggles SearchKnowledge visibility. Boot-only
// (reversible by restart), mirroring the USE_SKILL_REGISTRY precedent (#114).
func SetAgenticSearchKnowledgeEnabled(v bool) { agenticSearchKnowledgeOn = v }

// AgenticSearchKnowledgeEnabled reports whether the agentic SearchKnowledge gate
// is on. Used by the engine (P4a) to decide whether to relax the diagnosis
// instance-demand dead-end so the agent can retrieve evidence first.
func AgenticSearchKnowledgeEnabled() bool { return agenticSearchKnowledgeOn }

// Registry holds all registered tools for function calling.
var Registry = []openai.Tool{
	// --- Knowledge Tools (local, no API call) ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "SearchKnowledge",
			Description: "检索平台与第三方工具（vLLM/SGLang/Ollama/ComfyUI 等）运维知识库，返回带证据片段的条目（chunk_id/标题/摘要/片段）。排查报错、定位原因或回答“怎么做/为什么”类工具问题时，先用它取证再作答；引用返回的 chunk 内容，不要凭空编造命令或参数。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "要检索的症状、报错或问题（简短自然语言）。",
					},
					"context_hint": map[string]any{
						"type":        "string",
						"description": "可选，缩小检索范围的产品/工具领域提示，如 vllm / sglang / gpu。",
					},
				},
				"required": []string{"query"},
			},
		},
	},
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
						"description": "GPU 类型，可填上游返回的机型名称，例如 4090 或 A100。不传则返回本地 GPU 概览；要确认当前平台完整可选机型和配比，请用 DescribeAvailableCompShareInstanceTypes。",
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
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetModelVRAMRequirement",
			Description: "根据大模型名称(或参数量)估算推理所需显存,并给出可单卡承载的 GPU 选项;无单卡可承载时给出多卡方案。用于部署/选型,如 'Qwen32B'、'Llama3-70B'、'deepseek-67b'。仅做显存与可承载性计算;算力/场景优选请叠加 GetGPURecommendation,价格用 GetCompShareInstancePrice。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"model_name": map[string]any{
						"type":        "string",
						"description": "模型名称或参数量,如 'Qwen32B'、'Qwen2.5-32B-Instruct'、'Llama3-70B'、'7B'。",
					},
					"quantization": map[string]any{
						"type":        "string",
						"description": "量化精度,默认 fp16。可选 fp16 / bf16 / fp8 / int8 / int4(越低显存越省)。",
					},
				},
				"required": []string{"model_name"},
			},
		},
	},
	// --- External API Tools ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareInstance",
			Description: "查询用户自己账号下的算力共享实例列表及详情，不用于查询机房库存或平台是否还有 GPU 可售。返回实例状态（Running/Stopped/Install/Install Fail/Starting/Stopping/Rebooting）、GPU 类型、IP、计费、以及每台实例挂载的磁盘 DiskSet[]（含 DiskId/DiskShortId/Name/DiskType/Type=Boot|Data|Udisk，TotalDiskSpace 为数据盘总容量 GB）。用户问\"我有哪些数据盘 / 实例 X 挂了哪些磁盘 / 我的磁盘列表\"时也用此 tool，平台无独立 disk 查询接口。不传 UHostIds 查全部。Limit 最大 100。State 含义：Install=初始化中, Install Fail=初始化失败。",
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
			Description: "获取可用 GPU 机型列表及每种机型的合法 CPU/内存/GPU 组合。用于回答所有/完整 GPU 规格、某型号所有规格、CPU/内存组合、可选配置，也可用于回答 GPU 机型是否可售/是否售罄；返回 Status（Normal/SoldOut），只表示是否售卖，不代表实时可创建库存，也不返回精确剩余数量。注意：返回的 Memory 单位为 GB，创建实例时需转换为 MB。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，格式示例 cn-wlcb-01；真实可用区以支持区/机型接口返回为准。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "可用区所属地域，如 cn-bj2（cn-bj2-03 所在地域）。指定非默认可用区时需一并提供，否则上游报 Zone not available。",
					},
					"MachineTypes": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "按完整机型名称精确筛选，例如 [\"H20\"]。不确定完整名称、询问某型号家族/变体或当前支持哪些卡型时不要传此参数，应先查全量再按返回的 Name 过滤。",
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
			Description: "查询创建实例的目录价/标准价。返回按量/包日/包月/抢占式等分项价格（实例、磁盘、镜像）。Zone 格式为 cn-wlcb-01。Memory 单位为 MB（如 65536 = 64GB）。不传 ChargeType 则返回所有计费方式的价格。注意：本接口参数 Gpu/Cpu 小写；按量/按小时用 Postpay。用户实际折后价请用 GetCompShareInstanceUserPrice（其参数 GPU/CPU 大写）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，格式示例 cn-wlcb-01；真实可用区以支持区/机型接口返回为准。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "可用区所属地域，如 cn-bj2（cn-bj2-03 所在地域）。指定非默认可用区时需一并提供，否则上游报 Zone not available。",
					},
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型，必须来自 DescribeAvailableCompShareInstanceTypes 返回的 Name；例如 4090 或 A100，不要自行编造。",
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
						"description": "计费方式：Month / Day / Postpay / Spot，不传则返回所有方式。按量/按小时用 Postpay。",
						"enum":        []string{"Month", "Day", "Postpay", "Spot"},
					},
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "镜像 ID。创建/询价流程已选定镜像时应传入，用于计算镜像价格并匹配上游请求结构。",
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
						"description": "磁盘配置，创建价格应带系统盘，例如 [{IsBoot:true, Type:CLOUD_SSD, Size:60}]。",
					},
				},
				"required": []string{"Zone", "GpuType", "Gpu", "Cpu", "Memory"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareGpuInventory",
			Description: "查询 GPU 原始库存快照。返回 GpuInventory.Exclusive/Spot，按 zone_id -> GPU 型号 -> 剩余张数组织；该数量只表示原始 GPU 张数，不保证任意 CPU/内存/镜像组合都能创建。要确认某个具体配置是否可创建，还需要 CheckCompShareResourceCapacity。",
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
			Name:        "CheckCompShareResourceCapacity",
			Description: "预检某个具体创建实例配置是否有足够资源，适合在用户已给出 GPU/CPU/内存/镜像/计费方式等创建参数时使用；也可在库存问题已识别 GPU 型号并拿到可用区后，确认该机型当前是否真实可创建。只传 Zone/Region 字符串，内部会处理上游所需字段，不要手填 zone_id/az_group。MachineType 固定传 G。MinimalCpuPlatform 传 Auto（或 Intel/Auto、Amd/Auto）。CompShareImageId 和 ChargeType 必填。Disks 至少包含一个系统盘，如 [{IsBoot:true, Type:CLOUD_SSD, Size:60}]。返回各 GPU/CPU/Memory 组合的可用性。注意：本接口会校验镜像存在/状态，并在 ucloud 路径触发底层镜像适配解析；但它仍不能保证最终创建一定成功，镜像的 SupportedGpuTypes 也只能作为候选排序和风险提示。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，格式示例 cn-wlcb-01；真实可用区以支持区/机型接口返回为准。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "可用区所属地域，如 cn-bj2（cn-bj2-03 所在地域）。指定非默认可用区时需一并提供，否则上游报 Zone not available。",
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
					"Offset": map[string]any{
						"type":        "integer",
						"description": "分页偏移量，配合 Limit 翻页",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareImageTags",
			Description: "查询平台镜像标签分类目录。用于回答镜像有哪些标签、可按哪些分类筛选镜像；不返回具体镜像列表，不用于解释镜像概念或教程。",
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
			Name:        "DescribeModelRepositoryModels",
			Description: "查询公共模型仓库中的模型列表，可按模型名称或标签筛选。用于回答模型仓库里有哪些模型、某个模型是否存在、某类标签下有哪些模型；不用于创建实例或部署模型。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "按模型名称模糊搜索，如 qwen / llama / deepseek。",
					},
					"tags": map[string]any{
						"type":        "string",
						"description": "按模型仓库标签筛选，多个标签用逗号分隔，如 LLM。",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeModelRepositoryTags",
			Description: "查询公共模型仓库可用标签列表。用于回答模型仓库有哪些标签、可以按哪些模型标签筛选；不返回镜像标签。",
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
			Name:        "DescribeCompShareSoftwarePort",
			Description: "查询平台镜像的应用端口映射目录（JupyterLab、FileBrowser 等）。用于诊断应用端口连通性问题。注意：本接口返回的是镜像应用端口，SSH 登录信息以 DescribeCompShareInstance.SshLoginCommand 为准，不以本接口为准。仅需 Region 参数（自动填充）。",
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
			Description: "查询实例监控数据，如 CPU、内存、GPU、显存使用率。实时监控只传 UHostIds；历史监控必须传单个实例且同时传 StartTime/EndTime，时间窗最多 24 小时。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "实例 ID 列表（必填）",
					},
					"StartTime": map[string]any{
						"type":        "integer",
						"description": "历史监控开始时间，Unix 秒级时间戳；实时监控不要传。",
					},
					"EndTime": map[string]any{
						"type":        "integer",
						"description": "历史监控结束时间，Unix 秒级时间戳；必须晚于 StartTime。",
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
			Name:        "DescribeCompShareSharingImages",
			Description: "查询其他账号共享给当前账号的镜像列表。用于回答“共享给我的镜像”“别人共享给我的镜像在哪看”；不用于社区公开镜像列表，也不用于把自己的镜像共享给别人。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "按共享镜像 ID 精确查询。",
					},
					"Limit": map[string]any{
						"type":        "integer",
						"description": "分页大小，默认 20。",
					},
					"Offset": map[string]any{
						"type":        "integer",
						"description": "分页偏移，默认 0。",
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
					"SortCondition": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"Field": map[string]any{
								"type":        "string",
								"enum":        []string{"PubTime", "CreatedCount", "Favor", "ImageUseTime", "FavoritesCount"},
								"description": "排序字段。CreatedCount 表示按被用于创建实例的次数排序；PubTime 表示发布时间。",
							},
							"ASC": map[string]any{
								"type":        "boolean",
								"description": "是否升序；取热门或最新时通常为 false。",
							},
						},
					},
					"ExcludeReadme": map[string]any{
						"type":        "boolean",
						"description": "为 true 时响应不含 Readme 富文本，仅做列表/筛选时可省流量",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareInstanceUserPrice",
			Description: "查用户折后价/实际价格。返回 PriceDetails（折后）、OriginalPriceDetails（原价）、ListPriceDetails（目录价）三组明细。按量/按小时计费方式用 Postpay。参数 GPU/CPU 大写。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，格式示例 cn-wlcb-01；真实可用区以支持区/机型接口返回为准。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "可用区所属地域，如 cn-bj2（cn-bj2-03 所在地域）。指定非默认可用区时需一并提供，否则上游报 Zone not available。",
					},
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型，必须来自 DescribeAvailableCompShareInstanceTypes 返回的 Name；例如 4090 或 A100，不要自行编造。",
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
						"description": "计费方式：Month / Day / Postpay / Spot，按量/按小时用 Postpay。",
						"enum":        []string{"Month", "Day", "Postpay", "Spot"},
					},
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "镜像 ID。创建/询价流程已选定镜像时应传入，用于计算镜像价格并匹配上游请求结构。",
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
						"description": "磁盘配置，创建价格应带系统盘，例如 [{IsBoot:true, Type:CLOUD_SSD, Size:60}]。",
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
			Description: "创建实例的完整工作流。自动执行：查询镜像→检查库存→查询价格→用户确认→创建实例→查看状态。支持平台镜像和社区镜像。平台镜像默认查询公共镜像（含系统镜像和应用基础镜像如 PyTorch/CUDA 等）。传 ImageName 可按名称缩小镜像范围（平台和社区均可用）。传 ImageSource='community' 使用社区镜像创建。Pod 区必须使用容器镜像，普通区可使用系统镜像或应用镜像。不支持自制/私有镜像。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"GpuType": map[string]any{
						"type":        "string",
						"description": "GPU 类型，优先使用上游机型接口返回的 Name；例如 4090 或 A100。",
					},
					"Gpu": map[string]any{
						"type":        "number",
						"description": "GPU 数量，默认 1",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区，格式示例 cn-wlcb-01；真实候选来自上游支持区和机型接口。",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Postpay(按量/按小时后付费) / Month(包月) / Day(包日) / Spot(抢占式)，默认 Postpay。",
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
	// DescribeCompShareSupportZone is registered in security/levels.go (L0) but
	// not exposed to the LLM. It is an internal API called by create/deploy
	// planning. Read-only status/cost/entry/CFS tools below are user-facing.
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareRefundPrice",
			Description: "估算实例现在释放/退订可退金额，只做只读估算，不执行释放。必须传用户明确指定的实例 ID 列表；如果用户没说明是哪台实例，应先追问或列出候选。返回 RefundPriceSet，Code 非 0 时按 Message 说明原因。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "要估算退费的实例 ID 列表。",
					},
				},
				"required": []string{"UHostIds"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "DescribeCompShareJupyterToken",
			Description: "查询实例 Jupyter 入口所需 token 的只读接口。token 属于敏感凭据，回答时不要明文展示完整 token；只用于判断 Jupyter 是否需要 token、入口是否可用，必要时提示用户到控制台安全查看。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostIds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "实例 ID 列表，通常只传一台实例。",
					},
				},
				"required": []string{"UHostIds"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CheckCompShareNetOptimizer",
			Description: "查询当前账号/地域的网络加速状态，只读。用于回答网络加速是否已开通、哪些地域已加速；不会修改网络配置。用户要求开启加速时应使用 EnableNetOptimizerWorkflow。",
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
			Name:        "DescribeCFS",
			Description: "查询 CFS 共享文件存储列表或单个 CFS。CFS 是共享文件存储，可挂载到算力实例用于共享数据集/模型文件。可传 CfsId 精确查询；可传 Zone/Region 字符串筛选，内部会处理上游字段，不要手填 zone_id/az_group。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CfsId": map[string]any{
						"type":        "string",
						"description": "CFS ID，可选。",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区字符串，可选。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "地域字符串，可选。",
					},
				},
				"required": []string{},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareCFSPrice",
			Description: "查询创建 CFS 共享文件存储的价格。Size 单位 GB，上游支持 50 到 2048；必须指定可用区，且 CFS 当前只支持 Pod/容器可用区，不支持普通 UCloud 区；CFS 不支持按量付费，ChargeType 使用 Month/Year/Day/Dynamic。只传 Zone/Region 字符串，内部会处理上游字段。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Size": map[string]any{
						"type":        "integer",
						"description": "CFS 容量，单位 GB，范围 50 到 2048。",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Month / Year / Day / Dynamic。CFS 不支持 Postpay。",
						"enum":        []string{"Month", "Year", "Day", "Dynamic"},
					},
					"Quantity": map[string]any{
						"type":        "integer",
						"description": "购买时长，默认 1。",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区字符串。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "地域字符串。",
					},
				},
				"required": []string{"Size", "Zone"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareCFSUpgradePrice",
			Description: "查询 CFS 扩容到目标容量的价格差额。Size 是目标容量 GB，必须大于当前容量；工作流会从 DescribeCFS 结果带入内部可用区编号，模型不要手填 zone_id/az_group。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CfsId": map[string]any{
						"type":        "string",
						"description": "CFS ID。",
					},
					"Size": map[string]any{
						"type":        "integer",
						"description": "目标容量，单位 GB。",
					},
				},
				"required": []string{"CfsId", "Size"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareCFSRefundPrice",
			Description: "估算 CFS 共享文件存储现在退订可退金额，只做只读估算，不执行删除或释放。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CFSId": map[string]any{
						"type":        "string",
						"description": "CFS ID。",
					},
				},
				"required": []string{"CFSId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "GetCompShareInstanceUpgradePrice",
			Description: "查询实例变配（升降级 CPU/GPU/内存）的价格差额。用于变配前展示费用变化。必须指定 CPU/GPU/Memory 中至少一项目标值，仅传 UHostId 无差额可算。",
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
					"Region": map[string]any{
						"type":        "string",
						"description": "源实例所在地域。工作流会从实例查询结果填入；模型只传字符串，不需要填写 zone_id/az_group。",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "源实例所在可用区。工作流会从实例查询结果填入；模型只传可用区字符串。",
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
			Description: "新建数据盘并挂载到实例的工作流。只创建一块指定 Size 的新云数据盘并自动挂载，不支持挂载已有盘，也不是扩已有盘。用户要求'加数据盘'、'新建数据盘'、'加磁盘'、'磁盘不够'时使用；如果没给 Size，先追问容量。",
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
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "ResizeDiskWorkflow",
			Description: "扩已有磁盘工作流。用于把实例上已经挂载的系统盘或数据盘扩到指定目标容量。不会新建磁盘，也不支持挂载已有盘。Size 是目标容量 GB，不是新增容量；例如从 60GB 扩到 120GB 时 Size=120。扩系统盘传 DiskType=Boot；扩数据盘优先传 DiskId，实例有多块数据盘时必须传 DiskId。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要扩盘的实例 ID",
					},
					"DiskId": map[string]any{
						"type":        "string",
						"description": "要扩容的已有磁盘 ID。扩多块数据盘中的某一块时必须指定",
					},
					"DiskType": map[string]any{
						"type":        "string",
						"description": "要扩的盘类型。扩系统盘传 Boot；扩唯一数据盘可传 Data",
						"enum":        []string{"Boot", "Data"},
					},
					"Size": map[string]any{
						"type":        "number",
						"description": "目标容量（GB），必须大于当前容量；不是新增容量",
					},
				},
				"required": []string{"UHostId", "Size"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CreateCustomImageWorkflow",
			Description: "从已有实例创建自制镜像的确认式工作流。自动执行：DescribeCompShareInstance -> 用户确认 -> CreateCompShareCustomImage -> GetCompShareImageCreateProgress。用于用户要保存当前环境、把实例做成自定义镜像、下次复用环境。需要 UHostId 和镜像 Name；Description 可选。不用于发布社区镜像，不要直接调用原始 CreateCompShareCustomImage。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "源实例 ID",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "要创建的自制镜像名称。用户没给名称时，应先追问名称，不要编造。",
					},
					"Description": map[string]any{
						"type":        "string",
						"description": "镜像描述，可选",
					},
				},
				"required": []string{"UHostId", "Name"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "EnableNetOptimizerWorkflow",
			Description: "开启/同步指定可用区网络加速的确认式工作流。需要 Zone；先查询该区域网络加速状态，未开通时必须用户确认后才调用上游开启/同步接口；本轮 agent 暂不暴露关闭能力，因此不用于关闭网络加速。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Zone": map[string]any{
						"type":        "string",
						"description": "要开启网络加速的可用区字符串；真实可用区以支持区接口返回为准，不要填写 az_group。",
					},
					"Region": map[string]any{
						"type":        "string",
						"description": "地域字符串，可选；通常可由 Zone 推导。",
					},
				},
				"required": []string{"Zone"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CreateCFSWorkflow",
			Description: "创建 CFS 共享文件存储的确认式工作流。CFS 用于共享数据集/模型文件，当前只支持 Pod/容器可用区；创建前会查询价格并要求确认。需要 Name、Size、Zone；Size 单位 GB，范围 50 到 2048。CFS 不支持按量付费，不能用于删除 CFS。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Name": map[string]any{
						"type":        "string",
						"description": "CFS 名称。",
					},
					"Size": map[string]any{
						"type":        "number",
						"description": "CFS 容量，单位 GB，范围 50 到 2048。",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "可用区字符串，例如 cn-pod-01。真实可用区以支持区接口返回为准；不要填写 zone_id/az_group。",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Month / Year / Day / Dynamic。默认 Month，CFS 不支持 Postpay。",
						"enum":        []string{"Month", "Year", "Day", "Dynamic"},
					},
					"Quantity": map[string]any{
						"type":        "number",
						"description": "购买时长，默认 1。",
					},
				},
				"required": []string{"Name", "Size", "Zone"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "ResizeCFSWorkflow",
			Description: "扩容 CFS 共享文件存储的确认式工作流。先查询 CFS 当前容量和价格差额，再确认扩容。Size 是目标容量 GB，必须大于当前容量；不支持缩容或删除。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CfsId": map[string]any{
						"type":        "string",
						"description": "要扩容的 CFS ID。",
					},
					"Size": map[string]any{
						"type":        "number",
						"description": "目标容量，单位 GB，必须大于当前容量。",
					},
				},
				"required": []string{"CfsId", "Size"},
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
	agenticSearch := AgenticSearchKnowledgeEnabled()
	policies := DefaultToolExecutionPolicies()
	visible := make([]openai.Tool, 0, len(Registry))
	for _, tool := range Registry {
		if tool.Function == nil {
			continue
		}
		// Agentic-RAG SearchKnowledge (P3) is gated behind
		// COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE. When off it is invisible for EVERY
		// intent (full-registry AND subset), so the flag-off tool surface is
		// byte-identical to before this tool existed. When on it survives the
		// read-only filter below (Route=knowledge, read_cheap) and is scoped by
		// the subset filter (P4a adds it only to the diagnosis subset).
		if tool.Function.Name == "SearchKnowledge" && !agenticSearch {
			continue
		}
		if !mutatingEnabled {
			policy, ok := policies[tool.Function.Name]
			if ok && (policy.Route == ActionRouteWorkflow || policy.Class == ActionClassMutating) {
				continue
			}
		}
		visible = append(visible, tool)
	}
	return visible
}
