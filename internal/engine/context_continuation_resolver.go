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

type ContextContinuationResolver interface {
	ResolveContextContinuation(context.Context, ContextContinuationInput) (*ContextContinuationDecision, error)
}

type ContextContinuationInput struct {
	UserText        string
	RouterIntent    intent.Intent
	ContextFrame    ContextFrame
	InstanceContext string
}

type ContextContinuationDecision struct {
	Decision     string `json:"decision"`
	GPUPref      string `json:"gpu_pref"`
	ZonePref     string `json:"zone_pref"`
	ImagePref    string `json:"image_pref"`
	ImageSource  string `json:"image_source"`
	WorkloadPref string `json:"workload_pref"`
	InstanceRef  string `json:"instance_ref"`
	Clarify      string `json:"clarify"`
	Reason       string `json:"reason"`
}

const (
	ContextContinuationContinue = "continue_task"
	ContextContinuationNew      = "new_question"
	ContextContinuationClear    = "clear_context"
	ContextContinuationClarify  = "clarify"
)

type llmContextContinuationResolver struct {
	client LLMClient
}

func (r *llmContextContinuationResolver) ResolveContextContinuation(ctx context.Context, in ContextContinuationInput) (*ContextContinuationDecision, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("context continuation resolver: no LLM client")
	}
	resp, err := r.client.Chat(ctx, llm.ChatRequest{Messages: buildContextContinuationPrompt(in)})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("context continuation resolver: empty LLM response")
	}
	return parseContextContinuationDecision(resp.Content)
}

func (e *Engine) SetContextContinuationResolver(resolver ContextContinuationResolver) {
	e.contextContinuationResolver = resolver
}

func (e *Engine) resolveContextContinuation(ctx context.Context, userMsg string, route intent.Intent, frame ContextFrame) (*ContextContinuationDecision, error) {
	decision, err := e.resolveContextDecision(ctx, userMsg, route, frame)
	if err != nil || decision == nil {
		return nil, err
	}
	return contextDecisionToContinuation(*decision), nil
}

func buildContextContinuationPrompt(in ContextContinuationInput) []openai.ChatCompletionMessage {
	var sys strings.Builder
	sys.WriteString("你是优云智算上下文续接解析器。判断当前用户话语是否在续接上一轮保存的任务。\n")
	sys.WriteString("只输出 JSON 对象，不要任何额外文字：\n")
	sys.WriteString(`{"decision":"new_question","gpu_pref":"","zone_pref":"","image_pref":"","image_source":"","workload_pref":"","instance_ref":"","clarify":"","reason":""}` + "\n")
	sys.WriteString("decision 只能是 continue_task、new_question、clear_context、clarify。\n")
	sys.WriteString("规则：\n")
	sys.WriteString("- 用户明确沿用或短句替换上一轮创建/部署任务的 GPU、可用区、镜像、工作负载时，输出 continue_task，并只填写用户本轮明确修改的字段。\n")
	sys.WriteString("- 用户询问价格、建议、概念、教程、库存解释、无关知识时，输出 new_question，不要续接创建/部署。\n")
	sys.WriteString("- 用户说算了、不用了、取消、换个话题时，输出 clear_context。\n")
	sys.WriteString("- 用户话语确实像续接但缺少必要信息时，输出 clarify，并给出一句简短追问。\n")
	sys.WriteString("- 不要猜默认 GPU、默认可用区、默认镜像来源；没有明确说就留空。\n")
	sys.WriteString("- image_source 只能是 platform、community、custom、shared，用户没有明确说来源时留空。\n")

	var usr strings.Builder
	usr.WriteString("router_intent: " + string(in.RouterIntent) + "\n")
	usr.WriteString("context_frame:\n" + summarizeContextFrame(in.ContextFrame) + "\n")
	if strings.TrimSpace(in.InstanceContext) != "" {
		usr.WriteString("instance_context:\n" + strings.TrimSpace(in.InstanceContext) + "\n")
	}
	usr.WriteString("user_text: " + strings.TrimSpace(in.UserText))
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sys.String()},
		{Role: openai.ChatMessageRoleUser, Content: usr.String()},
	}
}

func parseContextContinuationDecision(raw string) (*ContextContinuationDecision, error) {
	var out ContextContinuationDecision
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &out); err != nil {
		return nil, err
	}
	return sanitizeContextContinuationDecision(out), nil
}

func sanitizeContextContinuationDecision(in ContextContinuationDecision) *ContextContinuationDecision {
	out := ContextContinuationDecision{
		Decision:     normalizeContextContinuationDecision(in.Decision),
		GPUPref:      strings.TrimSpace(in.GPUPref),
		ZonePref:     strings.TrimSpace(in.ZonePref),
		ImagePref:    strings.TrimSpace(in.ImagePref),
		ImageSource:  normalizeCreatePreferenceImageSource(in.ImageSource),
		WorkloadPref: strings.TrimSpace(in.WorkloadPref),
		InstanceRef:  strings.TrimSpace(in.InstanceRef),
		Clarify:      strings.TrimSpace(in.Clarify),
		Reason:       strings.TrimSpace(in.Reason),
	}
	if out.Decision == "" {
		out.Decision = ContextContinuationNew
	}
	if out.Decision != ContextContinuationContinue {
		out.GPUPref = ""
		out.ZonePref = ""
		out.ImagePref = ""
		out.ImageSource = ""
		out.WorkloadPref = ""
		out.InstanceRef = ""
	}
	if out.Decision != ContextContinuationClarify {
		out.Clarify = ""
	}
	return &out
}

func normalizeContextContinuationDecision(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ContextContinuationContinue, "continue", "resume", "update":
		return ContextContinuationContinue
	case ContextContinuationClear, "clear", "cancel", "stop":
		return ContextContinuationClear
	case ContextContinuationClarify, "ask":
		return ContextContinuationClarify
	case ContextContinuationNew, "", "new", "new_task", "new_query":
		return ContextContinuationNew
	default:
		return ContextContinuationNew
	}
}

func summarizeContextFrame(frame ContextFrame) string {
	var lines []string
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			lines = append(lines, k+": "+strings.TrimSpace(v))
		}
	}
	add("kind", frame.Kind)
	add("status", frame.Status)
	add("original_user_msg", frame.OriginalUserMsg)
	add("gpu", frame.GPU)
	add("image_pref", frame.ImagePref)
	add("image_source", frame.ImageSource)
	add("workload", frame.Workload)
	add("zone", frame.Zone)
	add("zone_label", frame.ZoneLabel)
	add("stage", frame.Stage)
	add("failure_reason", frame.FailureReason)
	if len(frame.AlternativeZones) > 0 {
		var zs []string
		for _, z := range frame.AlternativeZones {
			label := strings.TrimSpace(z.Label)
			if label == "" {
				label = strings.TrimSpace(z.Zone)
			}
			if label != "" {
				zs = append(zs, label)
			}
		}
		if len(zs) > 0 {
			add("alternative_zones", strings.Join(zs, "、"))
		}
	}
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "\n")
}

func summarizeInstanceContext(state SessionState) string {
	var lines []string
	if state.SelectedInstanceID != "" {
		if state.SelectedInstanceName != "" {
			lines = append(lines, "selected_instance: "+state.SelectedInstanceName+" ("+state.SelectedInstanceID+")")
		} else {
			lines = append(lines, "selected_instance: "+state.SelectedInstanceID)
		}
	}
	if len(state.PendingSelectionItems) > 0 {
		var items []string
		for _, item := range state.PendingSelectionItems {
			if item.Index <= 0 || item.ID == "" {
				continue
			}
			label := item.Name
			if label == "" {
				label = item.ID
			}
			items = append(items, fmt.Sprintf("%d:%s(%s)", item.Index, label, item.ID))
			if len(items) >= 5 {
				break
			}
		}
		if len(items) > 0 {
			lines = append(lines, "recent_instance_list: "+strings.Join(items, ", "))
		}
	}
	return strings.Join(lines, "\n")
}
