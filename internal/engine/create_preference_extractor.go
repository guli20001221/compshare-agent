package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// CreatePreferenceExtractor runs only after the router has already classified
// the turn as a create/deploy command. It extracts preferences; real GPU, zone,
// image and inventory validation remains in workflow/deploy planning.
type CreatePreferenceExtractor interface {
	ExtractCreatePreferences(ctx context.Context, input CreatePreferenceExtractionInput) (CreatePreferenceExtractionResult, error)
}

type CreatePreferenceExtractionInput struct {
	UserText  string
	Intent    intent.Intent
	SpeechAct intent.SpeechAct
}

type CreatePreferenceExtractionResult struct {
	WorkloadPref string `json:"workload_pref"`
	ImagePref    string `json:"image_pref"`
	ImageSource  string `json:"image_source"`
	GPUPref      string `json:"gpu_pref"`
	ZonePref     string `json:"zone_pref"`
	Purpose      string `json:"purpose"`
}

type llmCreatePreferenceExtractor struct {
	client LLMClient
}

func createPreferenceExtractorEnabledFromOS() bool {
	v := strings.TrimSpace(os.Getenv(createPreferenceExtractorEnv))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on")
}

func (e *Engine) extractCreatePreference(ctx context.Context, userMsg string, route intent.Intent, act intent.SpeechAct) (*CreatePreferenceExtractionResult, error) {
	extractor := e.createPreferenceExtractor
	if extractor == nil {
		client := e.agentLLMClient
		if client == nil {
			client = e.llmClient
		}
		if client == nil {
			return nil, fmt.Errorf("create preference extractor: no LLM client")
		}
		extractor = llmCreatePreferenceExtractor{client: client}
	}
	result, err := extractor.ExtractCreatePreferences(ctx, CreatePreferenceExtractionInput{
		UserText:  userMsg,
		Intent:    route,
		SpeechAct: act,
	})
	if err != nil {
		return nil, err
	}
	result = sanitizeCreatePreference(result)
	return &result, nil
}

func (x llmCreatePreferenceExtractor) ExtractCreatePreferences(ctx context.Context, input CreatePreferenceExtractionInput) (CreatePreferenceExtractionResult, error) {
	if x.client == nil {
		return CreatePreferenceExtractionResult{}, fmt.Errorf("create preference extractor: no LLM client")
	}
	resp, err := x.client.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: strings.Join([]string{
					"你只抽取用户在创建或部署 GPU 实例命令中明确表达的偏好。",
					"没有明确表达的字段必须返回空字符串，不要猜默认值。",
					"workload_pref 是要运行的模型、应用或任务，例如 DeepSeek R1 32B、数字人、ComfyUI。",
					"image_pref 是用户明确指定的镜像或环境，例如 PyTorch、vLLM、Ubuntu。",
					"image_source 只能是 platform、community、custom、shared 或空字符串。",
					"gpu_pref 和 zone_pref 保留用户原文中的 GPU 和可用区偏好。",
					"purpose 只能是 deep_learning、llm_inference、image_video、system、platform_app、community 或空字符串。",
					"只返回 JSON 对象，字段为 workload_pref,image_pref,image_source,gpu_pref,zone_pref,purpose。",
				}, "\n"),
			},
			{
				Role: openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("intent=%s\nspeech_act=%s\n用户原话：%s",
					input.Intent, input.SpeechAct, input.UserText),
			},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{Type: openai.ChatCompletionResponseFormatTypeJSONObject},
	})
	if err != nil {
		return CreatePreferenceExtractionResult{}, err
	}
	return parseCreatePreferenceExtraction(resp.Content)
}

func parseCreatePreferenceExtraction(content string) (CreatePreferenceExtractionResult, error) {
	var result CreatePreferenceExtractionResult
	if err := json.Unmarshal([]byte(extractJSONObject(content)), &result); err != nil {
		return CreatePreferenceExtractionResult{}, err
	}
	return sanitizeCreatePreference(result), nil
}

func sanitizeCreatePreference(in CreatePreferenceExtractionResult) CreatePreferenceExtractionResult {
	return CreatePreferenceExtractionResult{
		WorkloadPref: strings.TrimSpace(in.WorkloadPref),
		ImagePref:    strings.TrimSpace(in.ImagePref),
		ImageSource:  normalizePreferenceImageSource(in.ImageSource),
		GPUPref:      strings.TrimSpace(in.GPUPref),
		ZonePref:     strings.TrimSpace(in.ZonePref),
		Purpose:      normalizePreferencePurpose(in.Purpose),
	}
}

func normalizePreferenceImageSource(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	switch {
	case s == "platform" || s == "app" || strings.Contains(s, "平台"):
		return "platform"
	case s == "community" || strings.Contains(s, "社区"):
		return "community"
	case s == "custom" || strings.Contains(s, "自制") || strings.Contains(s, "私有"):
		return "custom"
	case s == "shared" || strings.Contains(s, "共享"):
		return "shared"
	default:
		return ""
	}
}

func normalizePreferencePurpose(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	switch {
	case s == "deep_learning" || strings.Contains(s, "训练") || strings.Contains(s, "deep"):
		return "deep_learning"
	case s == "llm_inference" || strings.Contains(s, "推理") || strings.Contains(s, "llm"):
		return "llm_inference"
	case s == "image_video" || strings.Contains(s, "图像") || strings.Contains(s, "视频"):
		return "image_video"
	case s == "system" || strings.Contains(s, "系统"):
		return "system"
	case s == "platform_app" || strings.Contains(s, "平台应用"):
		return "platform_app"
	case s == "community" || strings.Contains(s, "社区"):
		return "community"
	default:
		return ""
	}
}

func (e *Engine) createInstanceWorkflowArgsFromPreference(ctx context.Context, pref CreatePreferenceExtractionResult) map[string]any {
	args := map[string]any{}
	if pref.GPUPref != "" {
		args["GpuType"] = pref.GPUPref
	}
	if pref.ZonePref != "" {
		if zone, clarify := e.resolveRequestedZone(ctx, pref.ZonePref); clarify == "" && zone != "" {
			args["Zone"] = zone
		} else {
			args["Zone"] = pref.ZonePref
		}
	}
	if imageQuery := createImageQueryFromPreference(pref.ImagePref); imageQuery != "" {
		args["ImageName"] = imageQuery
	}
	if pref.ImageSource != "" {
		args["ImageSource"] = pref.ImageSource
	}
	if pref.Purpose != "" {
		args["ImagePurpose"] = pref.Purpose
	} else if pref.ImageSource == "community" {
		args["ImagePurpose"] = "community"
	}
	return args
}

func (e *Engine) mergeCreatePreferenceArgs(ctx context.Context, base map[string]any, pref CreatePreferenceExtractionResult) map[string]any {
	merged := map[string]any{}
	for k, v := range base {
		merged[k] = v
	}
	prefArgs := e.createInstanceWorkflowArgsFromPreference(ctx, pref)
	for k, v := range prefArgs {
		if _, exists := merged[k]; exists && !createPreferenceCanOverrideBase(k) {
			continue
		}
		merged[k] = v
	}
	return merged
}

func createPreferenceCanOverrideBase(key string) bool {
	switch key {
	case "ImageName", "ImageSource", "ImagePurpose":
		return true
	default:
		return false
	}
}

func createImageQueryFromPreference(pref string) string {
	s := strings.TrimSpace(pref)
	if s == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"最新", "",
		"镜像", "",
		"基础", "",
		"环境", "",
	)
	normalized := strings.TrimSpace(replacer.Replace(s))
	lower := strings.ToLower(normalized)
	switch {
	case strings.Contains(lower, "pytorch") || strings.Contains(lower, "torch"):
		return "torch"
	case strings.Contains(lower, "cuda"):
		return "cuda"
	case strings.Contains(lower, "vllm"):
		return "vLLM"
	case strings.Contains(lower, "ollama"):
		return "Ollama"
	case strings.Contains(lower, "comfy"):
		return "ComfyUI"
	case strings.Contains(lower, "ubuntu"):
		return "Ubuntu"
	case strings.Contains(lower, "windows"):
		return "Windows"
	default:
		return normalized
	}
}

func deployMessageWithCreatePreference(userMsg string, pref CreatePreferenceExtractionResult) string {
	pref = sanitizeCreatePreference(pref)
	var lines []string
	if pref.WorkloadPref != "" {
		lines = append(lines, "部署目标："+pref.WorkloadPref)
	}
	if pref.ImageSource != "" {
		lines = append(lines, "镜像来源："+pref.ImageSource)
	}
	if pref.ImagePref != "" {
		lines = append(lines, "镜像偏好："+pref.ImagePref)
	}
	if pref.GPUPref != "" {
		lines = append(lines, "GPU偏好："+pref.GPUPref)
	}
	if pref.ZonePref != "" {
		lines = append(lines, "可用区偏好："+pref.ZonePref)
	}
	if pref.Purpose != "" {
		lines = append(lines, "用途偏好："+pref.Purpose)
	}
	if len(lines) == 0 {
		return userMsg
	}
	return strings.TrimSpace(userMsg) + "\n\n[已抽取的创建/部署偏好]\n" + strings.Join(lines, "\n")
}
