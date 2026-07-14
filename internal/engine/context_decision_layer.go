package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
)

type ContextDecisionLayer interface {
	ResolveContextDecision(context.Context, ContextDecisionInput) (*ContextDecision, error)
}

type ContextDecisionInput struct {
	UserText            string
	RouterIntent        intent.Intent
	ActiveTask          ContextFrame
	TaskSnapshot        string
	SelectedEntity      string
	PendingChoices      string
	RecentFacts         string
	LastAssistantPrompt string
}

type ContextDecision struct {
	Decision     string            `json:"decision"`
	Target       string            `json:"target"`
	SlotUpdates  map[string]string `json:"slot_updates,omitempty"`
	GPUPref      string            `json:"gpu_pref"`
	ZonePref     string            `json:"zone_pref"`
	ImagePref    string            `json:"image_pref"`
	ImageSource  string            `json:"image_source"`
	WorkloadPref string            `json:"workload_pref"`
	InstanceRef  string            `json:"instance_ref"`
	Metric       string            `json:"metric"`
	BillingTopic string            `json:"billing_topic"`
	Clarify      string            `json:"clarify"`
	Reason       string            `json:"reason"`
}

const (
	ContextDecisionContinueTask   = "continue_task"
	ContextDecisionNewTask        = "new_task"
	ContextDecisionSelectEntity   = "select_entity"
	ContextDecisionAnswerFollowup = "answer_followup"
	ContextDecisionClearContext   = "clear_context"
	ContextDecisionClarify        = "clarify"

	ContextDecisionTargetCreate    = "create"
	ContextDecisionTargetDeploy    = "deploy"
	ContextDecisionTargetInstance  = "instance"
	ContextDecisionTargetStock     = "stock"
	ContextDecisionTargetPricing   = "pricing"
	ContextDecisionTargetBilling   = "billing"
	ContextDecisionTargetKnowledge = "knowledge"
)

type llmContextDecisionLayer struct {
	client LLMClient
}

func (r *llmContextDecisionLayer) ResolveContextDecision(ctx context.Context, in ContextDecisionInput) (*ContextDecision, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("context decision layer: no LLM client")
	}
	resp, err := r.client.Chat(ctx, llm.ChatRequest{Messages: buildContextDecisionPrompt(in)})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("context decision layer: empty LLM response")
	}
	return parseContextDecision(resp.Content)
}

type legacyContextContinuationLayer struct {
	resolver ContextContinuationResolver
}

func (r legacyContextContinuationLayer) ResolveContextDecision(ctx context.Context, in ContextDecisionInput) (*ContextDecision, error) {
	if r.resolver == nil {
		return nil, fmt.Errorf("context decision layer: no legacy resolver")
	}
	semanticTask := ""
	if strings.TrimSpace(in.TaskSnapshot) != "" {
		semanticTask = "task_snapshot (understanding_only):\n" + strings.TrimSpace(in.TaskSnapshot)
	}
	decision, err := r.resolver.ResolveContextContinuation(ctx, ContextContinuationInput{
		UserText:        in.UserText,
		RouterIntent:    in.RouterIntent,
		ContextFrame:    in.ActiveTask,
		InstanceContext: strings.Join(nonEmptyStrings(in.SelectedEntity, in.PendingChoices, semanticTask), "\n"),
	})
	if err != nil || decision == nil {
		return nil, err
	}
	return contextContinuationToDecision(*decision), nil
}

func (e *Engine) SetContextDecisionLayer(layer ContextDecisionLayer) {
	e.contextDecisionLayer = layer
}

func (e *Engine) SetContextDecisionObserver(observer func(ContextDecisionTrace)) {
	e.contextDecisionObserver = observer
}

type ContextDecisionTrace struct {
	UserText       string
	RouterIntent   string
	Decision       string
	Target         string
	Reason         string
	ActiveTaskKind string
	HasSelection   bool
	HasChoices     bool
	HasFacts       bool
	HasTaskMemory  bool
	ReadSet        []string
	StateDelta     []string
	ToolScope      string
	ToolNames      []string
	Error          string
}

func (e *Engine) resolveContextDecision(ctx context.Context, userMsg string, route intent.Intent, frame ContextFrame) (*ContextDecision, error) {
	if e == nil {
		return nil, fmt.Errorf("context decision layer: nil engine")
	}
	userKey := strings.TrimSpace(userMsg)
	if e.contextDecisionAttemptedThisTurn && e.contextDecisionUserTextThisTurn == userKey {
		return cloneContextDecision(e.contextDecisionThisTurn), e.contextDecisionErrThisTurn
	}
	// Direct unit callers can invoke the seam without ChatWithOptions (and thus
	// without its per-turn reset). A different user message is a different
	// synthetic turn; do not leak a cached judgment across it.
	if e.contextDecisionAttemptedThisTurn && e.contextDecisionUserTextThisTurn != userKey {
		e.resetContextDecisionTurn()
	}
	e.contextDecisionAttemptedThisTurn = true
	e.contextDecisionUserTextThisTurn = userKey

	layer := e.contextDecisionLayer
	if layer == nil && e.contextContinuationResolver != nil {
		layer = legacyContextContinuationLayer{resolver: e.contextContinuationResolver}
	}
	if layer == nil {
		client := e.agentLLMClient
		if client == nil {
			client = e.llmClient
		}
		layer = &llmContextDecisionLayer{client: client}
	}
	in := e.buildContextDecisionInput(userMsg, route, frame, time.Now())
	decision, err := layer.ResolveContextDecision(ctx, in)
	if err != nil {
		e.contextDecisionErrThisTurn = err
		e.emitContextDecisionTrace(in, nil, err)
		return nil, err
	}
	if decision == nil {
		err = fmt.Errorf("context decision layer: empty decision")
		e.contextDecisionErrThisTurn = err
		e.emitContextDecisionTrace(in, nil, err)
		return nil, err
	}
	decision = sanitizeContextDecision(*decision)
	e.contextDecisionThisTurn = cloneContextDecision(decision)
	e.emitContextDecisionTrace(in, decision, nil)
	return cloneContextDecision(decision), nil
}

func (e *Engine) resetContextDecisionTurn() {
	if e == nil {
		return
	}
	e.contextDecisionAttemptedThisTurn = false
	e.contextDecisionUserTextThisTurn = ""
	e.contextDecisionThisTurn = nil
	e.contextDecisionErrThisTurn = nil
	e.contextDecisionTraceThisTurn = ContextDecisionTrace{}
	e.contextDecisionTraceSeenThisTurn = false
}

func cloneContextDecision(in *ContextDecision) *ContextDecision {
	if in == nil {
		return nil
	}
	out := *in
	if in.SlotUpdates != nil {
		out.SlotUpdates = make(map[string]string, len(in.SlotUpdates))
		for key, value := range in.SlotUpdates {
			out.SlotUpdates[key] = value
		}
	}
	return &out
}

func (e *Engine) buildContextDecisionInput(userMsg string, route intent.Intent, frame ContextFrame, now time.Time) ContextDecisionInput {
	state := e.sessionState
	state.ContextFrame = frame
	input := buildContextDecisionInput(state, userMsg, route, now)
	input.LastAssistantPrompt = compactContextDecisionAssistantPrompt(e.lastAssistantContent())
	return input
}

func compactContextDecisionAssistantPrompt(s string) string {
	return truncateRunes(strings.TrimSpace(s), 600)
}

func buildContextDecisionInput(state SessionState, userMsg string, route intent.Intent, now time.Time) ContextDecisionInput {
	return ContextDecisionInput{
		UserText:       strings.TrimSpace(userMsg),
		RouterIntent:   route,
		ActiveTask:     state.ContextFrame,
		TaskSnapshot:   summarizeTaskSnapshotForDecision(state.TaskSnapshot),
		SelectedEntity: summarizeSelectedEntityContext(state),
		PendingChoices: summarizePendingChoicesContext(state),
		RecentFacts:    assembleFactContext(state.RecentFacts, now),
	}
}

func buildContextDecisionPrompt(in ContextDecisionInput) []openai.ChatCompletionMessage {
	var sys strings.Builder
	sys.WriteString("你是优云智算上下文决策层。只判断是否沿用上下文，不回答用户问题。\n")
	sys.WriteString("只输出 JSON 对象，不要任何额外文字：\n")
	sys.WriteString(`{"decision":"new_task","target":"","slot_updates":{},"gpu_pref":"","zone_pref":"","image_pref":"","image_source":"","workload_pref":"","instance_ref":"","metric":"","billing_topic":"","clarify":"","reason":""}` + "\n")
	sys.WriteString("decision 只能是 continue_task、new_task、select_entity、answer_followup、clear_context、clarify。\n")
	sys.WriteString("规则：\n")
	sys.WriteString("- 用户短句沿用当前待办任务并补充或修改参数时，输出 continue_task。\n")
	sys.WriteString("- 把补充或修改的参数写入 slot_updates，例如 size_gb、target_size_gb、cpu、memory_gb、gpu_count、gpu_type、zone、image_pref、image_id、image_source、workload、stop_time、name、cfs_id。\n")
	sys.WriteString("- 用户引用最近实例列表、已选实例或“它/这台/第 N 台”时，输出 select_entity，并填写 instance_ref。\n")
	sys.WriteString("- 用户追问最近库存、价格、监控、费用等只读结果时，输出 answer_followup，并填写 target；普通费用续问 target=billing，billing_topic=cost；明确询问退费/退款/退订估算时才填写 billing_topic=refund。\n")
	sys.WriteString("- 用户提出新的价格、建议、概念、教程、知识问题时，输出 new_task。\n")
	sys.WriteString("- 用户说算了、不用了、取消、换个话题时，输出 clear_context。\n")
	sys.WriteString("- 不确定时输出 clarify，并给一句简短追问。\n")
	sys.WriteString("- router_intent 只是可能出错的参考信号，不能单独决定清除或继续任务。\n")
	sys.WriteString("- task_snapshot 和 freshness=expired 的信息只能帮助理解，不得授权执行；需要执行时必须依赖仍有效的 active_task、用户本轮明确输入和后端确认。\n")
	sys.WriteString("- 不要生成最终 API 参数；GPU、可用区、镜像、实例 ID 都只表达用户意图，后端会校验。\n")
	sys.WriteString("- 写操作必须交给后端确认卡；不要自行判断已经确认。\n")
	sys.WriteString("- 不要猜默认 GPU、默认可用区、默认镜像来源；没有明确说就留空。\n")
	sys.WriteString("- image_source 只能是 platform、community、custom、shared，用户没有明确说来源时留空。\n")
	sys.WriteString("- image_id、cfs_id 只能在用户明确给出 ID 或上下文已有 ID 时填写；只说镜像名称时填 image_pref，不要猜 ID。\n")

	var usr strings.Builder
	usr.WriteString("router_intent: " + string(in.RouterIntent) + "\n")
	usr.WriteString("active_task:\n" + summarizeContextFrame(in.ActiveTask) + "\n")
	if strings.TrimSpace(in.TaskSnapshot) != "" {
		usr.WriteString("task_snapshot (understanding_only):\n" + strings.TrimSpace(in.TaskSnapshot) + "\n")
	}
	if strings.TrimSpace(in.SelectedEntity) != "" {
		usr.WriteString("selected_entity:\n" + strings.TrimSpace(in.SelectedEntity) + "\n")
	}
	if strings.TrimSpace(in.PendingChoices) != "" {
		usr.WriteString("pending_choices:\n" + strings.TrimSpace(in.PendingChoices) + "\n")
	}
	if strings.TrimSpace(in.RecentFacts) != "" {
		usr.WriteString("recent_facts:\n" + strings.TrimSpace(in.RecentFacts) + "\n")
	}
	if strings.TrimSpace(in.LastAssistantPrompt) != "" {
		usr.WriteString("last_assistant_prompt:\n" + strings.TrimSpace(in.LastAssistantPrompt) + "\n")
	}
	usr.WriteString("user_text: " + strings.TrimSpace(in.UserText))

	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sys.String()},
		{Role: openai.ChatMessageRoleUser, Content: usr.String()},
	}
}

func summarizeTaskSnapshotForDecision(task TaskSnapshot) string {
	if taskSnapshotEmpty(task) || task.Status == TaskSnapshotStatusResolved {
		return ""
	}
	var lines []string
	add := func(key, value string) {
		if value = strings.TrimSpace(value); value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	add("goal", task.Goal)
	add("intent", task.Intent)
	add("workflow", task.Workflow)
	add("stage", task.Stage)
	add("status", task.Status)
	add("freshness", task.Freshness)
	if len(task.MissingSlots) > 0 {
		add("missing_slots", strings.Join(task.MissingSlots, ","))
	}
	if len(task.Constraints) > 0 {
		add("constraints", strings.Join(task.Constraints, ","))
	}
	if len(task.Decisions) > 0 {
		add("decisions", strings.Join(task.Decisions, ","))
	}
	add("end_reason", task.EndReason)
	return strings.Join(lines, "\n")
}

func parseContextDecision(raw string) (*ContextDecision, error) {
	var in struct {
		Decision     string         `json:"decision"`
		Target       string         `json:"target"`
		SlotUpdates  map[string]any `json:"slot_updates,omitempty"`
		GPUPref      string         `json:"gpu_pref"`
		ZonePref     string         `json:"zone_pref"`
		ImagePref    string         `json:"image_pref"`
		ImageSource  string         `json:"image_source"`
		WorkloadPref string         `json:"workload_pref"`
		InstanceRef  string         `json:"instance_ref"`
		Metric       string         `json:"metric"`
		BillingTopic string         `json:"billing_topic"`
		Clarify      string         `json:"clarify"`
		Reason       string         `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &in); err != nil {
		return nil, err
	}
	out := ContextDecision{
		Decision:     in.Decision,
		Target:       in.Target,
		SlotUpdates:  normalizeContextSlotUpdatesAny(in.SlotUpdates),
		GPUPref:      in.GPUPref,
		ZonePref:     in.ZonePref,
		ImagePref:    in.ImagePref,
		ImageSource:  in.ImageSource,
		WorkloadPref: in.WorkloadPref,
		InstanceRef:  in.InstanceRef,
		Metric:       in.Metric,
		BillingTopic: in.BillingTopic,
		Clarify:      in.Clarify,
		Reason:       in.Reason,
	}
	return sanitizeContextDecision(out), nil
}

func sanitizeContextDecision(in ContextDecision) *ContextDecision {
	out := ContextDecision{
		Decision:     normalizeContextDecision(in.Decision),
		Target:       normalizeContextDecisionTarget(in.Target),
		SlotUpdates:  normalizeContextSlotUpdates(in.SlotUpdates),
		GPUPref:      strings.TrimSpace(in.GPUPref),
		ZonePref:     strings.TrimSpace(in.ZonePref),
		ImagePref:    strings.TrimSpace(in.ImagePref),
		ImageSource:  normalizeCreatePreferenceImageSource(in.ImageSource),
		WorkloadPref: strings.TrimSpace(in.WorkloadPref),
		InstanceRef:  strings.TrimSpace(in.InstanceRef),
		Metric:       strings.TrimSpace(in.Metric),
		BillingTopic: strings.TrimSpace(in.BillingTopic),
		Clarify:      strings.TrimSpace(in.Clarify),
		Reason:       strings.TrimSpace(in.Reason),
	}
	mirrorLegacyContextDecisionSlots(&out)
	switch out.Decision {
	case ContextDecisionContinueTask:
		out.InstanceRef = ""
	case ContextDecisionSelectEntity:
		out.SlotUpdates = nil
		out.GPUPref = ""
		out.ZonePref = ""
		out.ImagePref = ""
		out.ImageSource = ""
		out.WorkloadPref = ""
		out.Metric = ""
		out.BillingTopic = ""
	case ContextDecisionAnswerFollowup:
		out.GPUPref = ""
		out.ZonePref = ""
		out.ImagePref = ""
		out.ImageSource = ""
		out.WorkloadPref = ""
		out.InstanceRef = ""
	case ContextDecisionClarify:
		out.SlotUpdates = nil
		out.GPUPref = ""
		out.ZonePref = ""
		out.ImagePref = ""
		out.ImageSource = ""
		out.WorkloadPref = ""
		out.InstanceRef = ""
	default:
		out.Target = ""
		out.SlotUpdates = nil
		out.GPUPref = ""
		out.ZonePref = ""
		out.ImagePref = ""
		out.ImageSource = ""
		out.WorkloadPref = ""
		out.InstanceRef = ""
		out.Metric = ""
		out.BillingTopic = ""
	}
	if out.Decision != ContextDecisionClarify {
		out.Clarify = ""
	}
	return &out
}

func normalizeContextSlotUpdates(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		k := normalizeContextSlotKey(key)
		v := strings.TrimSpace(value)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeContextSlotUpdatesAny(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		k := normalizeContextSlotKey(key)
		v := contextSlotValueString(value)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func contextSlotValueString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strings.TrimSpace(fmt.Sprint(v))
	case bool:
		return fmt.Sprint(v)
	case map[string]any, []any:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func normalizeContextSlotKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "instance_id", "uhost_id", "uhostid":
		return "instance_id"
	case "disk_id", "udisk_id", "udiskid":
		return "disk_id"
	case "size", "size_gb", "disk_size", "disk_size_gb":
		return "size_gb"
	case "target_size", "target_size_gb", "disk_space", "diskspace":
		return "target_size_gb"
	case "cpu", "cpu_count":
		return "cpu"
	case "memory", "memory_gb", "mem":
		return "memory_gb"
	case "gpu_count", "gpu_num":
		return "gpu_count"
	case "gpu", "gpu_type", "gpu_model", "gpu_pref":
		return "gpu_type"
	case "zone", "zone_pref":
		return "zone"
	case "image", "image_pref", "image_name":
		return "image_pref"
	case "image_id", "comp_share_image_id", "compshareimageid", "comp_share_imageid":
		return "image_id"
	case "image_source":
		return "image_source"
	case "workload", "workload_pref", "model":
		return "workload"
	case "stop_time", "time", "schedule_time":
		return "stop_time"
	case "name", "instance_name":
		return "name"
	case "description", "desc":
		return "description"
	case "cfs_id", "cfsid":
		return "cfs_id"
	default:
		return ""
	}
}

func mirrorLegacyContextDecisionSlots(out *ContextDecision) {
	if out == nil {
		return
	}
	if out.SlotUpdates == nil {
		out.SlotUpdates = map[string]string{}
	}
	setIf := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		key = normalizeContextSlotKey(key)
		if key == "" {
			return
		}
		if _, exists := out.SlotUpdates[key]; !exists {
			out.SlotUpdates[key] = strings.TrimSpace(value)
		}
	}
	setIf("gpu_type", out.GPUPref)
	setIf("zone", out.ZonePref)
	setIf("image_pref", out.ImagePref)
	setIf("image_source", out.ImageSource)
	setIf("workload", out.WorkloadPref)
	if v := out.SlotUpdates["gpu_type"]; out.GPUPref == "" {
		out.GPUPref = v
	}
	if v := out.SlotUpdates["zone"]; out.ZonePref == "" {
		out.ZonePref = v
	}
	if v := out.SlotUpdates["image_pref"]; out.ImagePref == "" {
		out.ImagePref = v
	}
	if v := normalizeCreatePreferenceImageSource(out.SlotUpdates["image_source"]); out.ImageSource == "" {
		out.ImageSource = v
		if v != "" {
			out.SlotUpdates["image_source"] = v
		}
	}
	if v := out.SlotUpdates["workload"]; out.WorkloadPref == "" {
		out.WorkloadPref = v
	}
	if len(out.SlotUpdates) == 0 {
		out.SlotUpdates = nil
	}
}

func normalizeContextDecision(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ContextDecisionContinueTask, "continue", "resume", "update":
		return ContextDecisionContinueTask
	case ContextDecisionSelectEntity, "select", "entity", "resolve_entity":
		return ContextDecisionSelectEntity
	case ContextDecisionAnswerFollowup, "followup", "answer":
		return ContextDecisionAnswerFollowup
	case ContextDecisionClearContext, "clear", "cancel", "stop":
		return ContextDecisionClearContext
	case ContextDecisionClarify, "ask":
		return ContextDecisionClarify
	case ContextDecisionNewTask, "new", "new_question", "new_query":
		return ContextDecisionNewTask
	default:
		// Empty or unknown model output is uncertainty, not evidence that the user
		// started a new task. Treat it as clarification so the caller preserves the
		// active frame and can continue through the context-aware read-only path.
		return ContextDecisionClarify
	}
}

func normalizeContextDecisionTarget(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ContextDecisionTargetCreate:
		return ContextDecisionTargetCreate
	case ContextDecisionTargetDeploy:
		return ContextDecisionTargetDeploy
	case ContextDecisionTargetInstance:
		return ContextDecisionTargetInstance
	case ContextDecisionTargetStock:
		return ContextDecisionTargetStock
	case ContextDecisionTargetPricing:
		return ContextDecisionTargetPricing
	case ContextDecisionTargetBilling:
		return ContextDecisionTargetBilling
	case ContextDecisionTargetKnowledge:
		return ContextDecisionTargetKnowledge
	default:
		return ""
	}
}

func contextDecisionToContinuation(decision ContextDecision) *ContextContinuationDecision {
	switch decision.Decision {
	case ContextDecisionContinueTask:
		return &ContextContinuationDecision{
			Decision:     ContextContinuationContinue,
			GPUPref:      decision.GPUPref,
			ZonePref:     decision.ZonePref,
			ImagePref:    decision.ImagePref,
			ImageSource:  decision.ImageSource,
			WorkloadPref: decision.WorkloadPref,
			Clarify:      decision.Clarify,
			Reason:       decision.Reason,
		}
	case ContextDecisionNewTask:
		return &ContextContinuationDecision{Decision: ContextContinuationNew, Reason: decision.Reason}
	case ContextDecisionClearContext:
		return &ContextContinuationDecision{Decision: ContextContinuationClear, Reason: decision.Reason}
	case ContextDecisionClarify:
		return &ContextContinuationDecision{Decision: ContextContinuationClarify, Clarify: decision.Clarify, Reason: decision.Reason}
	default:
		return nil
	}
}

func contextContinuationToDecision(decision ContextContinuationDecision) *ContextDecision {
	switch decision.Decision {
	case ContextContinuationContinue:
		return sanitizeContextDecision(ContextDecision{
			Decision:     ContextDecisionContinueTask,
			Target:       ContextDecisionTargetCreate,
			GPUPref:      decision.GPUPref,
			ZonePref:     decision.ZonePref,
			ImagePref:    decision.ImagePref,
			ImageSource:  decision.ImageSource,
			WorkloadPref: decision.WorkloadPref,
			Clarify:      decision.Clarify,
			Reason:       decision.Reason,
		})
	case ContextContinuationClear:
		return sanitizeContextDecision(ContextDecision{Decision: ContextDecisionClearContext, Reason: decision.Reason})
	case ContextContinuationClarify:
		return sanitizeContextDecision(ContextDecision{Decision: ContextDecisionClarify, Clarify: decision.Clarify, Reason: decision.Reason})
	default:
		return sanitizeContextDecision(ContextDecision{Decision: ContextDecisionNewTask, Reason: decision.Reason})
	}
}

func summarizeSelectedEntityContext(state SessionState) string {
	if state.SelectedInstanceID == "" {
		return ""
	}
	if state.SelectedInstanceName != "" {
		return "selected_instance: " + state.SelectedInstanceName + " (" + state.SelectedInstanceID + ")"
	}
	return "selected_instance: " + state.SelectedInstanceID
}

func summarizePendingChoicesContext(state SessionState) string {
	if state.PendingSelectionKind == "" || len(state.PendingSelectionItems) == 0 {
		return ""
	}
	var items []string
	for _, item := range state.PendingSelectionItems {
		if item.Index <= 0 || item.ID == "" {
			continue
		}
		label := item.Name
		if label == "" {
			label = item.ID
		}
		detail := fmt.Sprintf("%d:%s(%s)", item.Index, label, item.ID)
		var attrs []string
		if item.State != "" {
			attrs = append(attrs, "state="+item.State)
		}
		if item.GpuType != "" || item.GPU > 0 {
			attrs = append(attrs, fmt.Sprintf("gpu=%s x%d", item.GpuType, item.GPU))
		}
		if len(attrs) > 0 {
			detail += "[" + strings.Join(attrs, ",") + "]"
		}
		items = append(items, detail)
		if len(items) >= 10 {
			break
		}
	}
	if len(items) == 0 {
		return ""
	}
	return "kind: " + state.PendingSelectionKind + "\n" + strings.Join(items, "\n")
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

// hasContextDecisionContext is deliberately narrower than "the session has
// ever mentioned an instance". It becomes true only for state that has an
// outstanding continuity question this turn: an active/expired task, an
// unresolved semantic task snapshot, or fresh fact breadcrumbs. Pending
// selection has its own pre-planner seam. That avoids adding a resolver LLM
// call to every ordinary turn
// merely because the user selected a machine days ago.
func (e *Engine) hasContextDecisionContext() bool {
	if e == nil || !ContextContinuationEnabled() || !e.sessionStateHydrated {
		return false
	}
	state := e.sessionState
	if !contextFrameEmpty(state.ContextFrame) {
		return true
	}
	if !taskSnapshotEmpty(state.TaskSnapshot) && state.TaskSnapshot.Status != TaskSnapshotStatusResolved {
		return true
	}
	if e.sessionFactContextEnabled && len(state.RecentFacts) > 0 {
		return true
	}
	// Pending resource selection owns its own pre-planner decision seam while
	// the in-memory prompt is active. Persisted selection items alone are not a
	// reason to run a second decision after an exact choice has already consumed
	// and cleared an unrelated workflow frame.
	return false
}

// resolveTurnContextDecision is the single per-turn lifecycle gate. The
// underlying resolve call is cached, so downstream consumers (workflow,
// create, fact follow-up, resource selection) all observe the same answer.
// Only explicit new/clear decisions mutate task lifetime here; a route label,
// handler success or resolver failure never does.
func (e *Engine) resolveTurnContextDecision(ctx context.Context, userMsg string, route intent.Intent) (*ContextDecision, bool, error) {
	if !e.hasContextDecisionContext() {
		return nil, false, nil
	}
	decision, err := e.resolveContextDecision(ctx, userMsg, route, e.sessionState.ContextFrame)
	if err != nil || decision == nil {
		return nil, true, err
	}
	if decision.Decision == ContextDecisionNewTask || decision.Decision == ContextDecisionClearContext {
		e.clearTaskCarryFromContextDecision(decision.Decision)
	}
	return decision, true, nil
}

func (e *Engine) clearTaskCarryFromContextDecision(decision string) {
	if e == nil || !e.sessionStateHydrated {
		return
	}
	if !contextFrameEmpty(e.sessionState.ContextFrame) {
		e.clearContextFrame()
	} else if !taskSnapshotEmpty(e.sessionState.TaskSnapshot) && e.sessionState.TaskSnapshot.Status != TaskSnapshotStatusResolved {
		task := e.sessionState.TaskSnapshot
		task.Status = TaskSnapshotStatusResolved
		task.Freshness = ContinuityFreshnessStale
		if decision == ContextDecisionClearContext {
			task.EndReason = "用户明确取消了任务"
		} else {
			task.EndReason = "用户开始了新任务"
		}
		task.UpdatedAtUnix = time.Now().Unix()
		e.sessionState.TaskSnapshot = task
	}
	e.sessionState.PendingDeployModel = ""
	e.sessionState.LastDeployWorkload = ""
	e.sessionState.LastDeployZone = ""
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func contextDecisionReadSet(in ContextDecisionInput) []string {
	readSet := []string{"user_text", "router_intent"}
	if !contextFrameEmpty(in.ActiveTask) {
		readSet = append(readSet, "active_task")
	}
	if strings.TrimSpace(in.TaskSnapshot) != "" {
		readSet = append(readSet, "task_snapshot")
	}
	if strings.TrimSpace(in.SelectedEntity) != "" {
		readSet = append(readSet, "selected_entity")
	}
	if strings.TrimSpace(in.PendingChoices) != "" {
		readSet = append(readSet, "pending_choices")
	}
	if strings.TrimSpace(in.RecentFacts) != "" {
		readSet = append(readSet, "recent_facts")
	}
	if strings.TrimSpace(in.LastAssistantPrompt) != "" {
		readSet = append(readSet, "last_assistant_prompt")
	}
	return readSet
}

func contextDecisionStateDelta(decision *ContextDecision) []string {
	if decision == nil {
		return []string{"task:preserve"}
	}
	switch decision.Decision {
	case ContextDecisionNewTask, ContextDecisionClearContext:
		return []string{"task:clear"}
	case ContextDecisionContinueTask:
		if len(decision.SlotUpdates) > 0 {
			return []string{"task:continue", "slots:merge_user_validated"}
		}
		return []string{"task:continue"}
	case ContextDecisionClarify:
		return []string{"task:preserve", "reply:clarify"}
	case ContextDecisionAnswerFollowup:
		return []string{"task:preserve", "facts:read_only"}
	case ContextDecisionSelectEntity:
		return []string{"task:preserve", "selection:resolve"}
	default:
		return []string{"task:preserve"}
	}
}

func (e *Engine) emitContextDecisionTrace(in ContextDecisionInput, decision *ContextDecision, err error) {
	if e == nil {
		return
	}
	scope := toolScopeForIntent(in.RouterIntent)
	trace := ContextDecisionTrace{
		UserText:       in.UserText,
		RouterIntent:   string(in.RouterIntent),
		ActiveTaskKind: in.ActiveTask.Kind,
		HasSelection:   strings.TrimSpace(in.SelectedEntity) != "",
		HasChoices:     strings.TrimSpace(in.PendingChoices) != "",
		HasFacts:       strings.TrimSpace(in.RecentFacts) != "",
		HasTaskMemory:  strings.TrimSpace(in.TaskSnapshot) != "",
		ReadSet:        contextDecisionReadSet(in),
		StateDelta:     contextDecisionStateDelta(decision),
		ToolScope:      string(scope.Mode),
		ToolNames:      append([]string(nil), scope.Names...),
	}
	if decision != nil {
		trace.Decision = decision.Decision
		trace.Target = decision.Target
		trace.Reason = decision.Reason
	}
	if err != nil {
		trace.Error = err.Error()
	}
	e.contextDecisionTraceThisTurn = trace
	e.contextDecisionTraceSeenThisTurn = true
	if e.contextDecisionObserver != nil {
		e.contextDecisionObserver(trace)
	}
}
