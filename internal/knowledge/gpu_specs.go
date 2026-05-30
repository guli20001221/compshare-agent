package knowledge

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
)

// GPUSpec holds physical specifications for a GPU model.
// MaxCPU/MaxMemory/MaxGPU are synced from uhost-compshare-api/libs/gpu.go.
// VRAM and FP16 are public specifications from NVIDIA.
// Prices are NOT included — must be queried via GetCompShareInstancePrice API.
type GPUSpec struct {
	Name        string   `json:"name"`
	VRAM        int      `json:"vram_gb"`        // 显存 GB
	FP16        float64  `json:"fp16_tflops"`    // FP16 算力 TFLOPS (Tensor Core dense, 不含 sparsity)
	MaxGPU      int      `json:"max_gpu"`        // 最大 GPU 卡数
	MaxCPU      int      `json:"max_cpu"`        // 最大 CPU 核数
	MaxMemoryGB int      `json:"max_memory_gb"`  // 最大内存 GB
	BestFor     []string `json:"best_for"`       // 推荐使用场景
	SpotSupport bool     `json:"spot_support"`   // 是否支持抢占式
	Generation  string   `json:"generation"`     // 架构代次
}

// gpuSpecs is the canonical GPU specification table.
// Sync MaxCPU/MaxMemory/MaxGPU from uhost-compshare-api/internal/api/compshare/libs/gpu.go.
// VRAM/FP16 from NVIDIA official specs.
//
// FP16 口径统一为 Tensor Core dense peak（不含 sparsity 2:4 加速）。
// 消费级 GPU（无 Tensor Core 或仅 INT8 Tensor）使用 shader FP16 peak。
// 标注 [估算] 的数值为基于架构推算，非 NVIDIA 官方确认。
var gpuSpecs = map[string]GPUSpec{
	"2080": {
		Name: "RTX 2080", VRAM: 8, FP16: 20.3, // Tensor Core FP16 peak
		MaxGPU: 8, MaxCPU: 92, MaxMemoryGB: 334,
		BestFor: []string{"轻量推理", "学习实验"}, SpotSupport: false,
		Generation: "Turing",
	},
	"2080Ti": {
		Name: "RTX 2080 Ti", VRAM: 11, FP16: 26.9, // Tensor Core FP16 peak
		MaxGPU: 8, MaxCPU: 48, MaxMemoryGB: 192,
		BestFor: []string{"推理", "轻量微调", "学习实验"}, SpotSupport: true,
		Generation: "Turing",
	},
	"3080Ti": {
		Name: "RTX 3080 Ti", VRAM: 12, FP16: 34.1, // Tensor Core FP16 peak
		MaxGPU: 8, MaxCPU: 12, MaxMemoryGB: 125,
		BestFor: []string{"推理", "SD绘图", "轻量微调"}, SpotSupport: true,
		Generation: "Ampere",
	},
	"3090": {
		Name: "RTX 3090", VRAM: 24, FP16: 35.6, // Tensor Core FP16 peak
		MaxGPU: 8, MaxCPU: 124, MaxMemoryGB: 450,
		BestFor: []string{"推理", "SD/ComfyUI", "LoRA微调"}, SpotSupport: true,
		Generation: "Ampere",
	},
	"4090": {
		Name: "RTX 4090", VRAM: 24, FP16: 82.6, // Tensor Core FP16 peak
		// MaxCPU uses the conservative lower bound: upstream defines
		// G_AMD_4090 (MaxCPU=128) and G_INTEL_4090 (MaxCPU=140). We report
		// the AMD value so users don't plan past AMD-platform limits.
		MaxGPU: 10, MaxCPU: 128, MaxMemoryGB: 680,
		BestFor: []string{"推理", "LoRA微调", "SD/ComfyUI", "vLLM部署"}, SpotSupport: true,
		Generation: "Ada Lovelace",
	},
	"4090Pro": {
		Name: "RTX 4090 Pro", VRAM: 24, FP16: 82.6,
		MaxGPU: 10, MaxCPU: 96, MaxMemoryGB: 950,
		BestFor: []string{"推理", "LoRA微调", "大内存训练"}, SpotSupport: true,
		Generation: "Ada Lovelace",
	},
	"4090_48G": {
		Name: "RTX 4090 48G", VRAM: 48, FP16: 82.6,
		MaxGPU: 8, MaxCPU: 124, MaxMemoryGB: 940,
		BestFor: []string{"大模型推理", "全量微调中小模型"}, SpotSupport: false,
		Generation: "Ada Lovelace",
	},
	"5090": {
		Name: "RTX 5090", VRAM: 32, FP16: 105.0, // [估算] 基于 Blackwell 架构公开信息推算
		MaxGPU: 8, MaxCPU: 124, MaxMemoryGB: 940,
		BestFor: []string{"推理", "LoRA微调", "最新架构"}, SpotSupport: true,
		Generation: "Blackwell",
	},
	"5090D": {
		Name: "RTX 5090D", VRAM: 32, FP16: 105.0, // [估算] 同 5090
		MaxGPU: 8, MaxCPU: 124, MaxMemoryGB: 940,
		BestFor: []string{"推理", "LoRA微调"}, SpotSupport: true,
		Generation: "Blackwell",
	},
	"P40": {
		Name: "Tesla P40", VRAM: 24, FP16: 12.0, // shader FP16 peak (无 FP16 Tensor Core)
		MaxGPU: 8, MaxCPU: 48, MaxMemoryGB: 502,
		BestFor: []string{"预算推理", "学习入门"}, SpotSupport: false,
		Generation: "Pascal",
	},
	"V100S": {
		Name: "Tesla V100S", VRAM: 32, FP16: 32.8, // Tensor Core FP16 dense (非 sparsity)
		MaxGPU: 8, MaxCPU: 92, MaxMemoryGB: 576,
		BestFor: []string{"训练", "推理", "科学计算"}, SpotSupport: true,
		Generation: "Volta",
	},
	"A100": {
		Name: "A100 80GB", VRAM: 80, FP16: 312.0, // Tensor Core FP16 dense
		MaxGPU: 8, MaxCPU: 124, MaxMemoryGB: 1024,
		BestFor: []string{"全量训练", "大模型微调", "大模型推理"}, SpotSupport: true,
		Generation: "Ampere",
	},
	"A800": {
		Name: "A800 80GB", VRAM: 80, FP16: 312.0, // Tensor Core FP16 dense (同 A100 算力)
		MaxGPU: 8, MaxCPU: 124, MaxMemoryGB: 1800,
		BestFor: []string{"全量训练", "大模型微调", "大模型推理"}, SpotSupport: false,
		Generation: "Ampere",
	},
	"H20": {
		Name: "H20 96GB", VRAM: 96, FP16: 148.0, // Tensor Core FP16 dense
		MaxGPU: 8, MaxCPU: 188, MaxMemoryGB: 1800,
		BestFor: []string{"大模型推理", "长序列处理", "大规模训练"}, SpotSupport: false,
		Generation: "Hopper",
	},
}

// GetGPUSpecs returns specifications for the given GPU type.
// If gpuType is empty, returns all GPU specs.
func GetGPUSpecs(gpuType string) (map[string]any, error) {
	if gpuType != "" {
		spec, ok := gpuSpecs[gpuType]
		if !ok {
			return nil, fmt.Errorf("未知的 GPU 类型: %s，支持的类型: %s", gpuType, allGPUTypes())
		}
		return map[string]any{"gpu_type": gpuType, "spec": spec}, nil
	}

	// Return summary of all GPUs
	type summary struct {
		Name   string  `json:"name"`
		VRAM   int     `json:"vram_gb"`
		FP16   float64 `json:"fp16_tflops"`
		MaxGPU int     `json:"max_gpu"`
	}
	all := make(map[string]summary, len(gpuSpecs))
	for k, v := range gpuSpecs {
		all[k] = summary{Name: v.Name, VRAM: v.VRAM, FP16: v.FP16, MaxGPU: v.MaxGPU}
	}
	return map[string]any{"gpu_specs": all}, nil
}

// GPURec is a GPU recommendation entry returned by GetGPURecommendation.
type GPURec struct {
	GPUType string  `json:"gpu_type"`
	Name    string  `json:"name"`
	VRAM    int     `json:"vram_gb"`
	FP16    float64 `json:"fp16_tflops"`
	Reason  string  `json:"reason"`
}

// GetGPURecommendation recommends GPU models based on the usage scene.
func GetGPURecommendation(scene string, budgetSensitive bool) map[string]any {
	scene = strings.ToLower(scene)

	var recs []GPURec

	switch {
	case containsAny(scene, "推理", "inference", "部署", "deploy", "vllm", "ollama"):
		recs = buildRecs("推理/部署", budgetSensitive,
			recEntry{"4090", "性价比最优，24GB显存适合中小模型推理和vLLM部署"},
			recEntry{"H20", "96GB大显存，适合大模型推理和长序列"},
			recEntry{"A100", "80GB显存+312TFLOPS，适合大模型高吞吐推理"},
			recEntry{"3090", "预算友好，24GB显存可跑7B-13B模型推理"},
		)

	case containsAny(scene, "lora", "微调", "finetune", "fine-tune", "fine_tune"):
		recs = buildRecs("LoRA微调", budgetSensitive,
			recEntry{"4090", "24GB显存+82.6TFLOPS，LoRA微调7B-13B模型最优选"},
			recEntry{"A100", "80GB显存适合LoRA微调30B+大模型"},
			recEntry{"3090", "预算友好，24GB显存可LoRA微调7B模型"},
			recEntry{"4090_48G", "48GB显存可LoRA微调更大模型"},
		)

	case containsAny(scene, "全量训练", "全量微调", "full", "pretrain", "train"):
		recs = buildRecs("全量训练", budgetSensitive,
			recEntry{"A100", "80GB显存+312TFLOPS，全量训练标准选择"},
			recEntry{"A800", "同A100算力，1800GB大内存"},
			recEntry{"H20", "96GB显存，长序列训练优势"},
		)

	case containsAny(scene, "sd", "stable diffusion", "comfyui", "绘图", "画图", "图片生成"):
		recs = buildRecs("SD/ComfyUI绘图", budgetSensitive,
			recEntry{"4090", "Ada架构+24GB显存，出图速度快"},
			recEntry{"3090", "24GB显存够用，性价比高"},
			recEntry{"3080Ti", "12GB显存可跑SD，预算最低"},
		)

	case containsAny(scene, "学习", "入门", "实验", "学生", "beginner", "learn"):
		recs = buildRecs("学习入门", budgetSensitive,
			recEntry{"3080Ti", "入门级性价比最高"},
			recEntry{"P40", "24GB显存，预算最友好"},
			recEntry{"3090", "24GB显存，性能更好"},
		)

	default:
		// General recommendation
		recs = buildRecs("通用场景", budgetSensitive,
			recEntry{"4090", "综合性价比最优，适合大多数场景"},
			recEntry{"A100", "专业训练和大模型首选"},
			recEntry{"3090", "预算友好的全能选手"},
			recEntry{"H20", "大显存+高性能，适合大模型"},
		)
	}

	result := map[string]any{
		"scene":            scene,
		"budget_sensitive": budgetSensitive,
		"recommendations":  recs,
		"note":             "价格请通过 GetCompShareInstancePrice 查询实时价格。库存请通过 CheckCompShareResourceCapacity 确认。",
	}
	return result
}

type recEntry struct {
	gpuType string
	reason  string
}

func buildRecs(scene string, budgetSensitive bool, entries ...recEntry) []GPURec {
	var recs []GPURec
	for _, e := range entries {
		spec, ok := gpuSpecs[e.gpuType]
		if !ok {
			// Development-time drift guard: a recommendation entry references
			// a GPU type not defined in gpuSpecs. This should only happen when
			// a new entry is added without the corresponding spec. In
			// production it's zero-frequency; we log rather than panic to
			// avoid taking down the agent for a knowledge-layer inconsistency.
			log.Printf("[knowledge] buildRecs(%q): recommendation entry %q missing from gpuSpecs; skipped", scene, e.gpuType)
			continue
		}
		recs = append(recs, GPURec{
			GPUType: e.gpuType,
			Name:    spec.Name,
			VRAM:    spec.VRAM,
			FP16:    spec.FP16,
			Reason:  e.reason,
		})
	}
	// If budget sensitive, reverse to put cheaper options first
	if budgetSensitive && len(recs) > 1 {
		for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
			recs[i], recs[j] = recs[j], recs[i]
		}
	}
	return recs
}

// ExecuteTool handles knowledge tool calls from the LLM.
// Returns the result as a JSON-serializable map.
func ExecuteTool(name string, args map[string]any) (map[string]any, error) {
	switch name {
	case "GetGPUSpecs":
		gpuType, _ := args["GpuType"].(string)
		return GetGPUSpecs(gpuType)

	case "GetGPURecommendation":
		scene, _ := args["scene"].(string)
		budget, _ := args["budget_sensitive"].(bool)
		return GetGPURecommendation(scene, budget), nil

	case "GetModelVRAMRequirement":
		modelName, _ := args["model_name"].(string)
		quant, _ := args["quantization"].(string)
		return GetModelVRAMRequirement(modelName, quant), nil

	default:
		return nil, fmt.Errorf("unknown knowledge tool: %s", name)
	}
}

// IsKnowledgeTool returns true if the action is a local knowledge tool
// (not an external API call).
func IsKnowledgeTool(action string) bool {
	switch action {
	case "GetGPUSpecs", "GetGPURecommendation", "GetModelVRAMRequirement":
		return true
	default:
		return false
	}
}

// ResultToJSON converts a result map to a JSON string for LLM consumption.
func ResultToJSON(result map[string]any) string {
	b, err := json.Marshal(result)
	if err != nil {
		errMap := map[string]string{"error": err.Error()}
		b2, _ := json.Marshal(errMap)
		return string(b2)
	}
	return string(b)
}

// containsAny checks if s contains any of the given substrings (case-insensitive).
// Caller is expected to pass a lowercased s.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func allGPUTypes() string {
	types := make([]string, 0, len(gpuSpecs))
	for k := range gpuSpecs {
		types = append(types, k)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}
