package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
)

type CreatePreferenceExtractor interface {
	ExtractCreatePreferences(context.Context, CreatePreferenceExtractionInput) (*CreatePreferenceExtractionResult, error)
}

type CreatePreferenceExtractionInput struct {
	UserText string
	Intent   intent.Intent
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

func (x *llmCreatePreferenceExtractor) ExtractCreatePreferences(ctx context.Context, in CreatePreferenceExtractionInput) (*CreatePreferenceExtractionResult, error) {
	if x == nil || x.client == nil {
		return nil, fmt.Errorf("create preference extractor: no LLM client")
	}
	resp, err := x.client.Chat(ctx, llm.ChatRequest{Messages: buildCreatePreferencePrompt(in)})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("create preference extractor: empty LLM response")
	}
	pref, err := parseCreatePreferenceExtraction(resp.Content)
	if err != nil {
		return nil, err
	}
	return sanitizeCreatePreferenceForInput(*pref, in), nil
}

func (e *Engine) extractCreatePreference(ctx context.Context, userMsg string, route intent.Intent) (*CreatePreferenceExtractionResult, error) {
	extractor := e.createPreferenceExtractor
	if extractor == nil {
		client := e.agentLLMClient
		if client == nil {
			client = e.llmClient
		}
		extractor = &llmCreatePreferenceExtractor{client: client}
	}
	return extractor.ExtractCreatePreferences(ctx, CreatePreferenceExtractionInput{
		UserText: userMsg,
		Intent:   route,
	})
}

func buildCreatePreferencePrompt(in CreatePreferenceExtractionInput) []openai.ChatCompletionMessage {
	var sys strings.Builder
	sys.WriteString("你是优云智算创建/部署偏好抽取器。只抽取用户明确表达的偏好，不要补全、猜测或规划。\n")
	sys.WriteString("只输出一个 JSON 对象，不要任何额外文字：\n")
	sys.WriteString(`{"workload_pref":"","image_pref":"","image_source":"","gpu_pref":"","zone_pref":"","purpose":""}` + "\n")
	sys.WriteString("字段说明：\n")
	sys.WriteString("- workload_pref：用户要运行/部署的模型、应用或任务，例如 DeepSeek R1 32B、数字人、ComfyUI。\n")
	sys.WriteString("- image_pref：用户明确点名的镜像或环境，例如 PyTorch、CUDA、vLLM、Ollama、Ubuntu、Windows。\n")
	sys.WriteString("- image_source：仅当用户明确说平台/社区/自制/共享镜像时填 platform/community/custom/shared，否则留空。\n")
	sys.WriteString("- gpu_pref：用户明确点名的 GPU。\n")
	sys.WriteString("- zone_pref：用户明确点名的地域/可用区中文名或区 ID。\n")
	sys.WriteString("- purpose：仅当用户明确说训练/推理/图像视频/系统/应用镜像/社区镜像用途时填写 training/inference/image_video/system/platform_app/community，否则留空。\n")
	sys.WriteString("约束：不得因为用户要部署某个模型就推断 purpose 或 image_source；例如“部署 DeepSeek R1 32B”只能抽 workload_pref，不得自动填写 purpose 或 image_source。\n")

	var usr strings.Builder
	usr.WriteString("intent: " + string(in.Intent) + "\n")
	usr.WriteString("用户原话：" + strings.TrimSpace(in.UserText))
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sys.String()},
		{Role: openai.ChatMessageRoleUser, Content: usr.String()},
	}
}

func parseCreatePreferenceExtraction(raw string) (*CreatePreferenceExtractionResult, error) {
	var out CreatePreferenceExtractionResult
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &out); err != nil {
		return nil, err
	}
	return sanitizeCreatePreference(out), nil
}

func sanitizeCreatePreference(in CreatePreferenceExtractionResult) *CreatePreferenceExtractionResult {
	out := CreatePreferenceExtractionResult{
		WorkloadPref: strings.TrimSpace(in.WorkloadPref),
		ImagePref:    strings.TrimSpace(in.ImagePref),
		ImageSource:  normalizeCreatePreferenceImageSource(in.ImageSource),
		GPUPref:      strings.TrimSpace(in.GPUPref),
		ZonePref:     strings.TrimSpace(in.ZonePref),
		Purpose:      normalizeCreatePreferencePurpose(in.Purpose),
	}
	return &out
}

func sanitizeCreatePreferenceForInput(pref CreatePreferenceExtractionResult, in CreatePreferenceExtractionInput) *CreatePreferenceExtractionResult {
	out := sanitizeCreatePreference(pref)
	userText := strings.ToLower(strings.TrimSpace(in.UserText))
	if !userExplicitlyMentionedImageSource(userText, out.ImageSource) {
		out.ImageSource = ""
	}
	if !userExplicitlyMentionedPurpose(userText, out.Purpose) {
		out.Purpose = ""
	}
	return out
}

func userExplicitlyMentionedImageSource(userText, imageSource string) bool {
	switch imageSource {
	case "":
		return true
	case "platform":
		return strings.Contains(userText, "平台镜像") || strings.Contains(userText, "官方镜像") || strings.Contains(userText, "platform")
	case "community":
		return strings.Contains(userText, "社区镜像") || strings.Contains(userText, "community")
	case "custom":
		return strings.Contains(userText, "自制镜像") || strings.Contains(userText, "私有镜像") || strings.Contains(userText, "custom")
	case "shared":
		return strings.Contains(userText, "共享镜像") || strings.Contains(userText, "shared")
	default:
		return false
	}
}

func userExplicitlyMentionedPurpose(userText, purpose string) bool {
	switch purpose {
	case "":
		return true
	case "training":
		return strings.Contains(userText, "训练") || strings.Contains(userText, "train")
	case "inference":
		return strings.Contains(userText, "推理") || strings.Contains(userText, "inference") || strings.Contains(userText, "infer")
	case "image_video":
		return strings.Contains(userText, "图像") || strings.Contains(userText, "视频") || strings.Contains(userText, "image") || strings.Contains(userText, "video")
	case "system":
		return strings.Contains(userText, "系统") || strings.Contains(userText, "system")
	case "platform_app":
		return strings.Contains(userText, "应用镜像") || strings.Contains(userText, "应用环境") || strings.Contains(userText, "app")
	case "community":
		return strings.Contains(userText, "社区镜像") || strings.Contains(userText, "community")
	default:
		return false
	}
}

func normalizeCreatePreferenceImageSource(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return ""
	case "platform", "平台", "平台镜像":
		return "platform"
	case "community", "社区", "社区镜像":
		return "community"
	case "custom", "自制", "自制镜像":
		return "custom"
	case "shared", "共享", "共享镜像":
		return "shared"
	default:
		return ""
	}
}

func normalizeCreatePreferencePurpose(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return ""
	case "training", "train", "deep_learning", "深度学习", "训练":
		return "training"
	case "inference", "infer", "llm_inference", "推理":
		return "inference"
	case "image_video", "image_gen", "video", "图像", "视频", "图像视频":
		return "image_video"
	case "system", "os", "系统":
		return "system"
	case "platform_app", "app", "应用", "应用镜像":
		return "platform_app"
	case "community", "社区", "社区镜像":
		return "community"
	default:
		return ""
	}
}

func deployMessageWithCreatePreference(userMsg string, pref CreatePreferenceExtractionResult) string {
	var b strings.Builder
	b.WriteString("用户原话：" + strings.TrimSpace(userMsg))
	lines := []struct {
		key string
		val string
	}{
		{"workload_pref", pref.WorkloadPref},
		{"image_pref", pref.ImagePref},
		{"image_source", pref.ImageSource},
		{"gpu_pref", pref.GPUPref},
		{"zone_pref", pref.ZonePref},
		{"purpose", pref.Purpose},
	}
	wroteHeader := false
	for _, line := range lines {
		v := strings.TrimSpace(line.val)
		if v == "" {
			continue
		}
		if !wroteHeader {
			b.WriteString("\n\n已抽取的偏好：")
			wroteHeader = true
		}
		b.WriteString("\n- " + line.key + ": " + v)
	}
	return b.String()
}
