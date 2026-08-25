package tools

import (
	"github.com/compshare-agent/internal/cfsbilling"
	openai "github.com/sashabaranov/go-openai"
)

// CustomerSupportHandoffName is the central Agent's response-only handoff
// capability. The model decides whether to call it; the active channel owns
// the exact user-facing delivery.
const CustomerSupportHandoffName = "HandoffToCustomerSupport"

func portDeltaSchema(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items": map[string]any{
			"type":    "integer",
			"minimum": 1,
			"maximum": 65535,
		},
		"maxItems":    10,
		"uniqueItems": true,
	}
}

// Registry holds all registered tools for function calling.
var Registry = []openai.Tool{
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        CustomerSupportHandoffName,
			Description: "转接人工客服。仅当用户明确要求联系人工客服，或现有能力无法完成且确实需要平台人员核验时调用；用户只是提及、询问、引用或拒绝人工客服时不要调用。渠道适配器会生成适合当前入口的转接说明，不要自行输出联系方式。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
	},
	// --- Knowledge Tools (local, no API call) ---
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "SearchKnowledge",
			Description: "检索平台文档和技术证据。用于稳定的产品规则、操作方法、技术原理和故障知识；平台当前目录、可用性、状态、价格、库存或热度使用对应只读能力。返回本轮可引用的证据条目。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "脱离对话上文也能独立理解的检索问题。",
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
			Name:        "ReadChunk",
			Description: "按 chunk_id 读取知识条目的完整正文。SearchKnowledge 返回的 snippet 只是节选，正文更长；当节选被截断、只给出结论而没有给出具体参数/步骤/取值，或据此无法确定答案时，必须先用本工具读全文再作答，不要凭节选推测。只能读 SearchKnowledge 已返回过的 chunk_id。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"chunk_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "要读取全文的 chunk_id 列表，一次最多 3 个。",
					},
				},
				"required": []string{"chunk_ids"},
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
						"description": "磁盘配置，创建价格应带系统盘；工作流会按镜像 Size 和规格目录里的启动盘类型生成，模型不要写死大小。",
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
			Description: "查询 GPU 原始库存快照。返回 GpuInventory.Exclusive/Spot，按 zone_id -> GPU 型号 -> 剩余张数组织；该数量只表示原始 GPU 张数，不保证任意 CPU/内存/镜像组合都能创建，也不作为卡片禁用或“无库存”的最终依据。要确认某个具体配置是否可创建，还需要 CheckCompShareResourceCapacity。",
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
			Description: "预检某个具体创建实例配置是否有足够资源，适合在用户已给出 GPU/CPU/内存/镜像/计费方式等创建参数时使用；也可在库存问题已识别 GPU 型号并拿到可用区后，确认该机型当前是否真实可创建。模型只提供业务字段，workflow 会按可用区类型补齐内部位置参数：普通区用 Zone/Region，Pod 区内部用 zone_id；不要手填 zone_id/az_group。MachineType 为 G。MinimalCpuPlatform 由规格目录推导，缺失时为 Auto。CompShareImageId 和 ChargeType 必填。Disks 由镜像 Size 和规格目录生成，不要写死 60GB。返回各 GPU/CPU/Memory 组合的可用性。注意：本接口会校验镜像存在/状态，并在 ucloud 路径触发底层镜像适配解析；但它仍不能保证最终创建一定成功，镜像的 SupportedGpuTypes 也只能作为候选排序和风险提示。",
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
			Description: "查询平台镜像列表。ImageType 枚举：System（系统镜像，裸 Ubuntu/Windows）、App（应用基础镜像，如 PyTorch/CUDA/ComfyUI/Ollama）、Game（游戏镜像）、Other（其他），不传返回全部。查自制镜像请用 DescribeCompShareCustomImages，查社区镜像请用 DescribeCommunityImages。不用于查库存。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "按镜像 ID 精确查询",
					},
					"ImageType": map[string]any{
						"type":        "string",
						"description": "镜像类型：System(系统镜像) / App(应用基础镜像) / Game(游戏镜像) / Other(其他)，不传则返回全部",
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
			Description: "查询公共模型目录，可按关键词、来源、标签、分类和目录状态筛选。ZoneID 只筛选该区 Available 的模型；ReplicaStatus 筛选任一可用区存在对应副本状态，二者不是同一维度。目录记录不等于目标可用区已有健康副本；目标区判断优先使用 typed 模型仓库能力。不用于创建实例或部署模型。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Keyword": map[string]any{
						"type":        "string",
						"description": "按模型名称模糊搜索，如 qwen / llama / deepseek。",
					},
					"Source": map[string]any{
						"type":        "string",
						"enum":        []string{"HuggingFace", "ModelScope", "Internal"},
						"description": "模型来源；省略时查询全部来源。",
					},
					"Tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "标签列表，标签需与目录实际值匹配。",
					},
					"Categories": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "模型分类列表。",
					},
					"Status": map[string]any{
						"type":        "string",
						"enum":        []string{"Active", "Offline", "Draft"},
						"description": "目录状态。",
					},
					"ReplicaStatus": map[string]any{
						"type":        "string",
						"enum":        []string{"Offline", "Incomplete", "Missing", "Healthy"},
						"description": "副本状态筛选：匹配任一可用区存在该状态；不是相对于 ZoneID 的状态。",
					},
					"ZoneID": map[string]any{
						"type":        "integer",
						"description": "目标可用区内部编号；只筛选 AvailableZoneIDs 包含该区的模型。外层能力应从实时可用区目录解析，不应由模型猜测。",
					},
					"Offset": map[string]any{
						"type":        "integer",
						"description": "分页偏移量。",
					},
					"Limit": map[string]any{
						"type":        "integer",
						"description": "返回条数。",
					},
					"SortBy": map[string]any{
						"type":        "string",
						"enum":        []string{"UseCount", "SizeBytes", "CreateTime", "UpdateTime"},
						"description": "排序字段。",
					},
					"SortOrder": map[string]any{
						"type":        "string",
						"enum":        []string{"asc", "desc"},
						"description": "排序方向。",
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
			Description: "查询当前账户的自制镜像列表。返回用户自己制作的镜像，包含 CompShareImageId、Name、Status、Container、SupportedGpuTypes 等字段；创建实例时应由 CreateInstanceWorkflow 按 custom 来源实时核验并确认。",
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
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "按社区镜像 ID 精确查询",
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
								"enum":        []string{"PubTime", "CreatedCount", "Favor", "ImageUseTime", "FavoritesCount", "Price"},
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
						"description": "磁盘配置，创建价格应带系统盘；工作流会按镜像 Size 和规格目录里的启动盘类型生成，模型不要写死大小。",
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
			Description: "创建算力实例的候选请求。用于用户明确要求实际创建实例；支持平台镜像、社区镜像和当前账户的自制镜像，配置不完整时可进入引导卡继续选择。价格、库存或创建方法查询不使用。",
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
						"enum":        []string{"Postpay", "Spot", "Day", "Month"},
					},
					"Cpu": map[string]any{
						"type":        "number",
						"description": "CPU 核数（可选）。不指定时使用平台默认值。需与 Memory 一起构成合法配比，可通过 DescribeAvailableCompShareInstanceTypes 查询。",
					},
					"Memory": map[string]any{
						"type":        "number",
						"description": "内存大小，单位 MB（可选）。不指定时使用平台默认值。如 64GB = 65536。需与 Cpu 一起构成合法配比。",
					},
					"SystemDiskSize": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"description": "系统盘容量（GB）；仅在用户指定时填写，否则省略。",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "实例名称（可选，最多 63 个字符）。只在用户明确指定名称时填写并保持原文；仅允许中文、英文字母、数字以及 _ , . : -。未指定则省略，由平台生成。",
						"maxLength":   63,
						"pattern":     `^[\u4E00-\u9FA5A-Za-z0-9_,.:-]+$`,
					},
					"ImageSource": map[string]any{
						"type":        "string",
						"description": "镜像来源：platform（平台镜像，默认）/ community（社区镜像）/ custom（当前账户的自制镜像）。仅在用户明确说出来源，或 ID 来自近期已展示的镜像推荐且来源已知时填写。用户直接给出精确 CompShareImageId 但未说来源时，填写 ID 并省略本字段；服务端会通过实时目录确定其实际来源。",
						"enum":        []string{"platform", "community", "custom"},
					},
					"ImageName": map[string]any{
						"type":        "string",
						"description": "镜像名称或关键词。没有精确 ID 时用于目录搜索；用户明确改选另一镜像时原样填写。近期已提供的对话历史已有精确 CompShareImageId 且用户只是复述或简称该推荐时，以 ID 为准，不必同时填写本字段。",
					},
					// Carry an exact id when one is known because it names one
					// version without depending on fuzzy-search wording. A user
					// restating or abbreviating that recommendation does not need a
					// second name field: the exact, transcript-grounded id is the
					// stronger evidence. A genuinely different user-chosen name
					// still replaces the historical candidate.
					//
					// The value may come from a listing seen this turn OR an exact id
					// printed in a recent replayed exchange. It is only a
					// candidate: CodecImage re-verifies it against the live catalog
					// before any card or workflow can use it. An invented/stale id is
					// rejected, never replaced by a name-matched image.
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "镜像 ID，如 compshareImage-xxxx。用户本轮直接输入、本轮镜像查询或近期已提供的对话历史看到精确 ID 时，原样填写。来源未知时不要猜测 ImageSource；服务端会实时核验并确定来源。历史推荐 ID 只作待实时核验和用户确认的候选；不得填写对话中从未逐字出现的 ID 或凭空编造。用户只是复述或简称该推荐时保留该 ID；若明确改选不同镜像，则不要沿用无关历史 ID，并填写新的 ImageName。",
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
			Description: "关闭已有实例的候选请求。用于用户要求实际关机；关机方法、影响或费用咨询不使用。关机不会释放磁盘，保留资源可能继续计费。",
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
			Name: "StartInstanceWorkflow",
			// The old wording was "用于普通开机或无卡开机", which presents a spec change
			// as a second flavour of the same act. What the operation is gets said
			// here; what the no-GPU parameter does is said on the parameter.
			Description: "启动已有实例的候选请求。用于用户要求实际开机；开机方法或费用咨询不使用。若实例当前已处于无卡模式且不填写 WithoutGpuSpec，平台会先恢复其存档的原带卡规格再开机。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要开机的实例 ID",
					},
					"WithoutGpuSpec": map[string]any{
						"type": "string",
						"enum": []string{"A", "B"},
						// The old text described only the tiers, which reads as a boot
						// option. What the parameter DOES is resize the instance: the
						// GPU is given up, the original spec is archived, and getting
						// it back depends on that GPU being available again. Those are
						// facts about the operation and belong here. What must NOT
						// accumulate here is guidance about particular situations —
						// the resolver gate, not a sentence, is what stops an
						// unrequested value, and a description that argues a case goes
						// stale while the gate does not.
						"description": "可选，仅在用户明确要求无卡时填写。它不是开机选项：平台会先把实例改配为无卡规格" +
							"（A=2C/4GB，B=8C/16GB，GPU 归零，容器实例仅支持 A），再开机；原规格被存档，恢复它需要该可用区当时有对应 GPU 可用。",
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
			Description: "重启运行中实例的候选请求。仅用于用户要求实际重启；会中断当前运行任务。关机后再开机或只询问重启方法时不使用。",
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
			Description: "修改已有实例名称的候选请求。仅用于用户要求实际改名；新名称最多 63 个字符，仅允许中文、英文字母、数字以及 _ , . : -。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要改名的实例 ID",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "新的实例名称，最多 63 个字符，仅允许中文、英文字母、数字以及 _ , . : -。",
						"maxLength":   63,
						"pattern":     `^[\u4E00-\u9FA5A-Za-z0-9_,.:-]+$`,
					},
				},
				"required": []string{"UHostId", "Name"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "UpdateInstancePortsWorkflow",
			Description: "修改 Pod 平台端口配置的候选请求。用于用户明确要求添加或移除 HTTP/TCP 平台入口；工作流会读取并保留当前完整端口集合、展示精确前后差异、确认后复核并发变更，再执行一次全量替换。虚机不使用。UDP 端口不在此操作中修改，因为上游结果不提供可验证的公网 UDP 转发，不能用它承诺 WebRTC 等公网 UDP 可达。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要修改端口配置的 Pod 实例 ID。",
					},
					"AddHttpPorts":    portDeltaSchema("要新增的 HTTP 平台入口端口。"),
					"RemoveHttpPorts": portDeltaSchema("要移除的 HTTP 平台入口端口。"),
					"AddTcpPorts":     portDeltaSchema("要新增的 TCP 平台转发端口。"),
					"RemoveTcpPorts":  portDeltaSchema("要移除的 TCP 平台转发端口。"),
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "ResetPasswordWorkflow",
			Description: "重置已有实例登录密码的候选请求。Pod 必须处于 Running；UHost 容器可在 Running 或 Stopped 状态重置；普通 UHost 虚机需关机。密码规则由安全输入和服务端校验。",
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
			Description: "为运行中的非抢占式实例设置定时关机的候选请求。支持相对时间或绝对时间；取消已有定时关机应使用取消操作。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要设置定时关机的实例 ID",
					},
					"Schedule": map[string]any{
						"type":        "object",
						"description": "关机时间的结构化含义。用户说“今天/明天”时必须使用对应 mode，不要自行换算日期。",
						"properties": map[string]any{
							"mode": map[string]any{
								"type": "string", "enum": []string{"after_minutes", "today", "tomorrow", "absolute"},
							},
							"minutes": map[string]any{
								"type": "integer", "minimum": 5, "description": "仅 mode=after_minutes。",
							},
							"local_time": map[string]any{
								"type": "string", "description": "仅 mode=today/tomorrow，格式 HH:MM。",
							},
							"at": map[string]any{
								"type": "string", "description": "仅 mode=absolute，必须来自用户明确写出的完整日期时间。",
							},
							"timezone": map[string]any{
								"type": "string", "enum": []string{"Asia/Shanghai", "UTC"},
							},
						},
						"required": []string{"mode"},
					},
				},
				"required": []string{"UHostId", "Schedule"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CancelStopSchedulerWorkflow",
			Description: "取消实例已有定时关机任务的候选请求。设置或修改关机时间不使用。",
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
			Description: "查询 CFS 共享文件存储列表或单个 CFS。CFS 可挂载到算力实例用于共享数据集/模型文件。可传 CfsId 精确查询；传 Zone 时，服务端会从实时可用区目录解析内部 zone_id 并按可用区过滤。不要手填 zone_id/az_group。",
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
			Description: "查询创建 CFS 共享文件存储的价格。Size 单位 GB，上游支持 50 到 2048；必须指定可用区，且 CFS 当前只支持 Pod/容器可用区，不支持普通 UCloud 区。新购计费仅支持包月、包年和包日，不支持按量或后付费。只传 Zone/Region 字符串，内部会处理上游字段。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"Size": map[string]any{
						"type":        "integer",
						"description": "CFS 容量，单位 GB，范围 50 到 2048。",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Month（包月）/ Year（包年）/ Day（包日）。",
						"enum":        cfsbilling.NewPurchaseTypes(),
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
			Description: "修改已有实例 CPU、GPU 或内存配置的候选请求。实例必须处于关机状态；磁盘扩容不使用。",
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
			Description: "使用选定镜像重装已有实例的候选请求。重装会清除系统盘数据；仅咨询镜像或不接受清盘风险时不使用。",
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
					"ImageName": map[string]any{
						"type":        "string",
						"description": "目标镜像名称或关键词；后端会通过真实镜像接口解析为 CompShareImageId",
					},
					"ImageSource": map[string]any{
						"type":        "string",
						"description": "镜像来源：platform/community/custom/sharing（兼容 shared）；不确定时可不传，服务端会用精确镜像 ID 实时判定来源",
						"enum":        []string{"platform", "community", "custom", "sharing", "shared"},
					},
				},
				"required": []string{"UHostId"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "CreateDiskWorkflow",
			Description: "为实例新建一块云数据盘并挂载的候选请求。只创建新盘，不扩已有盘，也不挂载已有盘。",
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
			Description: "扩容实例上已有系统盘或数据盘的候选请求。Size 表示目标总容量，不是新增容量；新建并挂载数据盘不使用。",
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
			Description: "从实例发起自制镜像制作。需要源实例和用户指定的镜像名称；未知字段留空，绝不能编造名称。展示确认卡，确认后发起制作；成功仅表示已开始，初始状态为 Making，变为 Available 后才可用于创建、共享或克隆。不会关闭源实例；不用于社区发布或跨可用区克隆。普通虚机可运行或关机，容器来源必须运行。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "源实例 ID",
					},
					"Name": map[string]any{
						"type":        "string",
						"description": "要创建的自制镜像名称，最多 50 个字符，仅允许中文、英文字母、数字以及 _ , . : -。用户未提供时留空，不要编造。",
						"maxLength":   50,
						"pattern":     `^[\u4E00-\u9FA5A-Za-z0-9_,.:-]+$`,
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
			Name:        "CloneCustomImageWorkflow",
			Description: "将当前账号已有且可用的自制镜像克隆到另一个可用区的候选请求。源镜像、目标可用区或新名称未知时留空，绝不能编造；服务端会返回缺失字段或候选。只适用于自制镜像；从实例制作新镜像或复制平台、社区、共享镜像不使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"CompShareImageId": map[string]any{
						"type":        "string",
						"description": "源自制镜像 ID；如用户只给名称，应先查询自制镜像目录取得真实 ID。",
					},
					"Zone": map[string]any{
						"type":        "string",
						"description": "目标可用区字符串或展示名；不能与源镜像所在可用区相同。",
					},
					"TargetImageName": map[string]any{
						"type":        "string",
						"description": "克隆后目标镜像的名称，最多 50 个字符，仅允许中文、英文字母、数字以及 _ , . : -。用户未提供时留空，不要编造。",
						"maxLength":   50,
						"pattern":     `^[\u4E00-\u9FA5A-Za-z0-9_,.:-]+$`,
					},
					"TargetImageDescription": map[string]any{
						"type":        "string",
						"description": "克隆后目标镜像的描述，可选。",
					},
				},
				"required": []string{"CompShareImageId", "Zone", "TargetImageName"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "EnableNetOptimizerWorkflow",
			Description: "为指定可用区开启或同步网络加速的候选请求。只支持开启或同步，不支持关闭；查询当前状态使用只读能力。",
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
			Description: "创建 CFS 共享文件存储的候选请求。仅支持 Pod/容器可用区；新购计费仅支持包月、包年和包日，不支持按量或后付费；扩容已有 CFS 不使用。",
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
						"description": "可用区字符串或展示名。真实可用区以支持区接口返回为准；不要填写 zone_id/az_group。",
					},
					"ChargeType": map[string]any{
						"type":        "string",
						"description": "计费方式：Month（包月）/ Year（包年）/ Day（包日），默认包月。",
						"enum":        cfsbilling.NewPurchaseTypes(),
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
			Description: "扩容已有 CFS 的候选请求。Size 是目标总容量且必须大于当前容量；不支持缩容、删除或新建 CFS。",
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
			Name:        "DiagnoseBilling",
			Description: "核对已有实例当前配置的上游净报价及关机后仍计费的磁盘。用于查询现有实例当前费用构成；不用于账户余额、账单流水、发票或历史实际扣款。可传 UHostId 查特定实例，不传则检查全部实例。金额由服务端根据结构化上游数据生成，Agent 不应重算；用户再次询问当前费用时重新调用本工具。",
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
			Name:        "DiagnoseInstanceInternals",
			Description: InstanceOpsDescription(),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"UHostId": map[string]any{
						"type":        "string",
						"description": "要进入排查的实例 ID",
					},
					"Task": map[string]any{
						"type":        "string",
						"description": "本次要排查什么，一句话自然语言描述（由你根据用户问题自行撰写）",
					},
				},
				"required": []string{"UHostId", "Task"},
			},
		},
	},
}

// VisibleRegistry returns the tool list exposed to the LLM for the current
// runtime mode. Read-only mode hides mutating workflow tools while keeping
// query, knowledge, and cloud-side diagnosis tools available.
func VisibleRegistry(mutatingEnabled bool) []openai.Tool {
	return DefaultCapabilityRegistry().VisibleTools(mutatingEnabled)
}
