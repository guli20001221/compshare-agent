package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// deploy_model.go is the B8.3 agent-tier dispatch arm. deploy_model = "按用户需求
// 选优云已有镜像建实例并轮询到 Running" (the lead's 2026-05-31 reframe): the platform
// image already bakes in the framework/model, so this neither installs a model
// over SSH nor uses UserData — it MATCHES a need to an existing image, sizes the
// GPU, and creates the instance through the orchestrator saga.
//
// Why a dedicated arm and not a capability: capability handlers (DispatchCapability)
// reach only the ToolExecutor and cannot call e.RunAgentSaga, so routing
// deploy_model as a capability would force a raw CreateCompShareInstance that
// bypasses the saga entirely — defeating B6.2/B8.2. This arm owns the engine, so
// it drives the saga (step-trace + StepConfirm HITL + L2-refuse) and polls.
//
// The saga reuses workflow.CreateInstanceDef() verbatim (no fork) — the arm only
// produces the param dict it already consumes (GpuType / ImageSource / ImageName)
// and recovers the new UHostId via a capture CheckResult, because workflow.Result
// carries only StepSummary, not step outputs. Polling to Running is handler-side
// (a bounded loop of short reads), NOT a saga step: Step.Timeout firing STOPS the
// saga (step.go:88 → saga.go:170-182), so there is no "timed-out-but-OK" exit —
// "poll exhausted ≠ failure" must be loop logic, not saga-timeout semantics.

var (
	// deployPollMaxRounds bounds the post-create poll loop. Exhausting it is NOT
	// a failure: the instance exists and is returned with whatever state it
	// reached (the reply says "still provisioning"). Package var (not const) so
	// tests override it; mutated only from tests (no t.Parallel on those).
	deployPollMaxRounds = 30
	// deployPollInterval is the ctx-aware sleep between poll rounds. 30×20s ≈
	// 10min ceiling; a GPU instance normally reaches Running in 1-3min. Tests
	// set this to 0 to skip real waits.
	deployPollInterval = 20 * time.Second
)

// deployPlan is the resolved deploy specification the matcher produces and the
// saga consumes. ImageSource/ImageName/GpuType are CreateInstanceDef params.
type deployPlan struct {
	ImageSource string // "platform" | "community"
	ImageName   string // image Name (platform) / group ImageName (community); may be "" for platform
	GpuType     string // CreateInstance GpuType, e.g. "A100"
	ModelName   string // model the user wants to run, for the reply; may be ""
	MatchNote   string // human-readable selection rationale (GPU sizing + any fallback)
}

// tryDeployModel handles an IntentDeployModel turn end-to-end. It ALWAYS returns
// handled=true — deploy_model is a dedicated skill and never falls through to
// the generic ReAct loop; failures surface as a friendly reply, not a fallback.
func (e *Engine) tryDeployModel(ctx context.Context, dispatch plannerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	result := dispatch.result

	// (1) Mutating gate. deploy_model creates a billable instance. When writes
	// are disabled (shipped default = read-only) refuse up front, before the
	// matcher LLM call + image queries — otherwise that work is wasted only for
	// the saga's create step to be hard-refused.
	if !e.mutatingToolsEnabled {
		return e.deployReply(result, dispatch.latency,
			"实例创建属于写操作，助手当前为只读模式，暂不能为你创建实例。如需开通，请联系管理员开启写操作权限后再试。")
	}

	// (2) Match an existing image + size the GPU (TierAgent judgment + deterministic
	// VRAM arithmetic) → CreateInstanceDef params.
	plan, err := e.matchDeployImage(ctx, userMsg, onStep)
	if err != nil {
		return e.deployReply(result, dispatch.latency,
			"抱歉，我没能确定合适的镜像和配置。可以告诉我你想部署的模型（如 Qwen2.5-32B）或应用（如 ComfyUI / 数字人）吗？")
	}
	e.emitDeployStep(onStep, StepToolResult, "deploy_match",
		fmt.Sprintf("已选型：%s 镜像 %s / GPU %s。%s", sourceLabel(plan.ImageSource), plan.ImageName, plan.GpuType, plan.MatchNote))

	// (3) Drive CreateInstanceDef through the orchestrator saga. Reuse the shipped
	// definition verbatim; inject capture hooks to recover the created instance id
	// (Result carries only StepSummary, not step outputs).
	def := workflow.CreateInstanceDef()
	var createResult, describeResult map[string]any
	captureStepResult(def, "创建实例", func(r map[string]any) { createResult = r })
	captureStepResult(def, "查看状态", func(r map[string]any) { describeResult = r })

	params := map[string]any{
		"GpuType":     plan.GpuType,
		"ImageSource": plan.ImageSource,
	}
	if plan.ImageName != "" {
		params["ImageName"] = plan.ImageName
	}

	sagaResult, sagaErr := e.RunAgentSaga(ctx, def, params, "deploy_model")
	if sagaErr != nil {
		// Programming/validation error (nil def / L2 in def) — not a step failure.
		return e.deployReply(result, dispatch.latency,
			fmt.Sprintf("创建流程未能启动：%v", sagaErr))
	}
	if !sagaResult.Success {
		return e.deployReply(result, dispatch.latency, deployStopReply(sagaResult))
	}

	// (4) Recover the new instance id and poll it to Running (handler-side).
	uHostId := firstUHostID(createResult)
	if uHostId == "" {
		// Grounding guard: saga succeeded but capture didn't fire (step renamed?).
		// Fail loud rather than silently skip the poll.
		return e.deployReply(result, dispatch.latency,
			"实例已创建，但未能解析实例 ID 进行状态轮询。请用「查询我的实例」查看最新状态。")
	}
	host, state := e.pollInstanceRunning(ctx, uHostId, onStep)
	if host == nil {
		// Never observed a describe result during polling; fall back to the
		// saga's own post-create describe so the reply still carries access info.
		host = firstHost(describeResult)
		state = stringFromHost(host, "State")
	}

	return e.deployReply(result, dispatch.latency, buildDeployReply(plan, uHostId, host, state))
}

// deployReply emits the planner trace and appends the assistant message, then
// returns (reply, true). The status is always CutoverStatusDispatchedAgent: the
// agent-tier deploy arm owned the turn (TierAgent match + orchestrator saga), so
// DeriveRealizedTier labels it the agent tier — mirroring how capability dispatch
// emits "dispatched"→fast even on refusal. Centralizes the three return-side
// concerns so every exit path of tryDeployModel stays consistent.
func (e *Engine) deployReply(result intent.PlannerResult, latency time.Duration, reply string) (string, bool) {
	e.emitPlannerTrace(result, intent.CutoverStatusDispatchedAgent, latency)
	e.recordLastIntentFromPlan(result.Plan)
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
}

// matchDeployImage queries the live image catalog (platform framework bases +
// community ready-to-run apps), asks the TierAgent model to pick the best fit,
// and sizes the GPU deterministically. The LLM does the fuzzy judgment (which
// image, which model/quantization); knowledge.RecommendGPUType does the VRAM
// arithmetic. The chosen image is grounded against the queried catalog so a
// hallucinated name cannot reach the saga.
func (e *Engine) matchDeployImage(ctx context.Context, userMsg string, onStep func(StepEvent)) (deployPlan, error) {
	client := e.agentLLMClient
	if client == nil {
		client = e.llmClient // NewWithDeps test path / no tier_routing.agent configured
	}
	if client == nil {
		return deployPlan{}, fmt.Errorf("deploy_model: no LLM client available for image matching")
	}
	e.emitDeployStep(onStep, StepToolCall, "deploy_match", "正在查询可用镜像并匹配最合适的部署方案…")

	platform := e.queryImageCandidates(ctx, "DescribeCompShareImages", map[string]any{"Limit": 30})
	community := e.queryImageCandidates(ctx, "DescribeCommunityImages", map[string]any{"Limit": 30, "ExcludeReadme": true})
	platformNames := platformImageNames(platform)
	communityNames := communityGroupNames(community)

	prompt := buildImageMatchPrompt(userMsg, platform, community)
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: prompt})
	if err != nil {
		return deployPlan{}, fmt.Errorf("deploy_model: image-match LLM call failed: %w", err)
	}
	e.emitTokenUsage(resp.Usage)

	var decision struct {
		ImageSource  string `json:"image_source"`
		ImageName    string `json:"image_name"`
		ModelName    string `json:"model_name"`
		Quantization string `json:"quantization"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(resp.Content)), &decision); err != nil {
		return deployPlan{}, fmt.Errorf("deploy_model: cannot parse image-match decision: %w", err)
	}

	plan := deployPlan{
		ImageSource: strings.ToLower(strings.TrimSpace(decision.ImageSource)),
		ImageName:   strings.TrimSpace(decision.ImageName),
		ModelName:   strings.TrimSpace(decision.ModelName),
	}

	// Ground the choice against the live catalog. Community requires an exact-ish
	// name (FuzzySearch=ImageName must resolve); platform tolerates a loose name
	// because the saga's matchPlatformImage exact→contains→first-falls-back.
	var groundNote string
	switch plan.ImageSource {
	case "community":
		if matched, ok := matchCandidateName(plan.ImageName, communityNames); ok {
			plan.ImageName = matched
		} else {
			// Hallucinated / absent community image — fall back to a platform
			// framework base (always present), which is the safe default for a
			// "deploy a model" request anyway.
			plan.ImageSource = "platform"
			plan.ImageName = ""
			groundNote = "未在社区镜像中找到匹配项，已回退到平台框架镜像"
		}
	default:
		plan.ImageSource = "platform"
		if matched, ok := matchCandidateName(plan.ImageName, platformNames); ok {
			plan.ImageName = matched
		}
		// On no match keep the LLM's name; matchPlatformImage falls back to the
		// first base. (Empty name → first base, also fine.)
	}

	gpuType, gpuNote := knowledge.RecommendGPUType(plan.ModelName, decision.Quantization, userMsg)
	plan.GpuType = gpuType
	plan.MatchNote = gpuNote
	if groundNote != "" {
		plan.MatchNote = groundNote + "；" + plan.MatchNote
	}
	return plan, nil
}

// queryImageCandidates runs a read-only image-listing tool through the safe
// executor (OriginWorkflowInternal = no per-call confirm / registry churn) and
// returns the raw result map, or nil on error (matching degrades gracefully —
// the matcher still has the other source + the user message).
func (e *Engine) queryImageCandidates(ctx context.Context, action string, args map[string]any) map[string]any {
	res, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
		Action: action,
		Args:   args,
		Origin: tools.OriginWorkflowInternal,
	})
	if err != nil || res == nil {
		return nil
	}
	return res.RawResult
}

// pollInstanceRunning polls DescribeCompShareInstance until the instance reaches
// Running, hits a terminal install-failure state, the context is cancelled, or
// deployPollMaxRounds is exhausted. Exhaustion is NOT a failure — the caller
// reports the last observed state ("still provisioning"). Returns the last host
// map observed (nil if no describe ever succeeded) and its State.
func (e *Engine) pollInstanceRunning(ctx context.Context, uHostId string, onStep func(StepEvent)) (host map[string]any, state string) {
	for round := 0; round < deployPollMaxRounds; round++ {
		res, err := e.executeSafeTool(ctx, tools.SafeToolRequest{
			Action: "DescribeCompShareInstance",
			Args:   map[string]any{"UHostIds": []any{uHostId}},
			Origin: tools.OriginWorkflowInternal,
		})
		if err == nil && res != nil {
			if h := firstHost(res.RawResult); h != nil {
				host = h
				state = stringFromHost(h, "State")
			}
			e.emitDeployStep(onStep, StepToolResult, "deploy_poll",
				fmt.Sprintf("轮询实例状态 [%d/%d]：%s", round+1, deployPollMaxRounds, orUnknown(state)))
			if state == "Running" {
				return host, state
			}
			if isTerminalFailState(state) {
				return host, state
			}
		}
		if round < deployPollMaxRounds-1 {
			select {
			case <-ctx.Done():
				return host, state
			case <-time.After(deployPollInterval):
			}
		}
	}
	return host, state
}

// emitDeployStep emits a coarse user-facing StepEvent for the deploy milestones.
// The saga's fine-grained StepTraces go to e.stepSink (trace_json.steps[]); these
// are the progress lines the CLI/SSE shows, since RunAgentSaga does not bridge to
// onStep.
func (e *Engine) emitDeployStep(onStep func(StepEvent), typ StepType, action, msg string) {
	if onStep == nil {
		return
	}
	onStep(StepEvent{Type: typ, Action: action, Source: observability.ToolSourcePlannerHandler, Message: msg})
}

// captureStepResult wraps (never replaces) a named step's CheckResult so the arm
// can recover that step's output map. The wrapper invokes capture(result) then
// delegates to any original CheckResult; with none it passes the step. CreateInstanceDef
// returns a fresh Definition each call, so mutating its steps is race-free.
//
// Capture fires only AFTER the tool execute succeeds (step.go calls CheckResult
// only when execErr==nil). So a step that fails to capture means that step (or an
// earlier one) failed → sagaResult.Success==false, which tryDeployModel checks
// BEFORE reading createResult/describeResult. The captured vars are therefore only
// read on the success path, where capture is guaranteed to have run.
func captureStepResult(def *workflow.Definition, stepName string, capture func(map[string]any)) {
	for i := range def.Steps {
		if def.Steps[i].Name != stepName {
			continue
		}
		orig := def.Steps[i].CheckResult
		def.Steps[i].CheckResult = func(wfCtx *workflow.Context, result map[string]any) (bool, string) {
			capture(result)
			if orig != nil {
				return orig(wfCtx, result)
			}
			return true, ""
		}
		return
	}
}

// buildDeployReply renders the deterministic deploy result. It NEVER echoes the
// instance Password (a base64 secret on the describe response); SshLoginCommand
// (which embeds the IP + port) is the access info we surface.
func buildDeployReply(plan deployPlan, uHostId string, host map[string]any, state string) string {
	var b strings.Builder
	switch {
	case state == "Running":
		b.WriteString("✅ 实例已创建并进入运行状态。\n")
	case isTerminalFailState(state):
		b.WriteString(fmt.Sprintf("⚠️ 实例已创建，但初始化未成功（状态：%s），建议在控制台查看日志或重建。\n", state))
	case state == "":
		b.WriteString("实例已创建，正在初始化（暂未获取到运行状态）。\n")
	default:
		b.WriteString(fmt.Sprintf("实例已创建，仍在初始化中（当前状态：%s），可能还需要几分钟。\n", state))
	}

	b.WriteString(fmt.Sprintf("- 实例 ID：%s\n", uHostId))
	if plan.GpuType != "" {
		b.WriteString(fmt.Sprintf("- GPU：%s\n", plan.GpuType))
	}
	if plan.ImageName != "" {
		b.WriteString(fmt.Sprintf("- 镜像：%s（%s）\n", plan.ImageName, sourceLabel(plan.ImageSource)))
	}
	if name := stringFromHost(host, "Name"); name != "" {
		b.WriteString(fmt.Sprintf("- 名称：%s\n", name))
	}
	if ssh := stringFromHost(host, "SshLoginCommand"); ssh != "" {
		b.WriteString(fmt.Sprintf("- SSH 登录：%s\n", ssh))
	}
	if state != "Running" && !isTerminalFailState(state) {
		b.WriteString("\n你可以稍后用「查询我的实例」查看最新状态和登录信息。\n")
	}
	if plan.MatchNote != "" {
		b.WriteString("\n（选型说明：" + plan.MatchNote + "）")
	}
	return b.String()
}

// deployStopReply renders a saga that stopped before success (capacity / price /
// confirm / create). The saga already put a human message in Result.Message.
func deployStopReply(r *workflow.Result) string {
	if r.Message == "用户取消了操作" {
		return "好的，已取消创建实例。"
	}
	if r.Message != "" {
		return "创建未完成：" + r.Message
	}
	if r.StoppedAt != "" {
		return fmt.Sprintf("创建在「%s」步骤中止。", r.StoppedAt)
	}
	return "创建未完成。"
}

// ── small pure helpers ──

func sourceLabel(source string) string {
	if source == "community" {
		return "社区镜像"
	}
	return "平台镜像"
}

func orUnknown(state string) string {
	if state == "" {
		return "未知"
	}
	return state
}

// isTerminalFailState reports states from which the instance will not reach
// Running on its own (init failure). Other non-Running states (Install /
// Starting / Initializing) are transient and keep the poll going.
func isTerminalFailState(state string) bool {
	return strings.Contains(strings.ToLower(state), "fail")
}

// firstUHostID extracts UHostIds[0] from a CreateCompShareInstance result.
func firstUHostID(createResult map[string]any) string {
	if createResult == nil {
		return ""
	}
	ids, ok := createResult["UHostIds"].([]any)
	if !ok || len(ids) == 0 {
		return ""
	}
	if s, ok := ids[0].(string); ok {
		return s
	}
	return ""
}

// firstHost extracts UHostSet[0] from a DescribeCompShareInstance result.
func firstHost(describeResult map[string]any) map[string]any {
	if describeResult == nil {
		return nil
	}
	set, ok := describeResult["UHostSet"].([]any)
	if !ok || len(set) == 0 {
		return nil
	}
	host, _ := set[0].(map[string]any)
	return host
}

func stringFromHost(host map[string]any, key string) string {
	if host == nil {
		return ""
	}
	if v, ok := host[key].(string); ok {
		return v
	}
	return ""
}

// matchCandidateName resolves an LLM-proposed image name against the live
// catalog: case-insensitive exact match first, then either side containing the
// other (handles "PyTorch" ↔ "PyTorch 2.9.1 cuda128"). Returns the catalog's
// canonical name on a hit.
func matchCandidateName(proposed string, candidates []string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(proposed))
	if p == "" {
		return "", false
	}
	for _, c := range candidates {
		if strings.EqualFold(c, proposed) {
			return c, true
		}
	}
	for _, c := range candidates {
		lc := strings.ToLower(c)
		if strings.Contains(lc, p) || strings.Contains(p, lc) {
			return c, true
		}
	}
	return "", false
}

func platformImageNames(result map[string]any) []string {
	var out []string
	if result == nil {
		return out
	}
	set, _ := result["ImageSet"].([]any)
	for _, item := range set {
		if m, ok := item.(map[string]any); ok {
			if name, _ := m["Name"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

func communityGroupNames(result map[string]any) []string {
	var out []string
	if result == nil {
		return out
	}
	groups, _ := result["CompshareImageGroup"].([]any)
	for _, item := range groups {
		if m, ok := item.(map[string]any); ok {
			if name, _ := m["ImageName"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// extractJSONObject returns the first {...} block in s, stripping markdown code
// fences and surrounding prose the model may add around the JSON decision.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// buildImageMatchPrompt assembles the TierAgent image-match request: a system
// prompt explaining the two image sources + the strict JSON contract, and a user
// prompt with the request and a compact catalog digest.
func buildImageMatchPrompt(userMsg string, platform, community map[string]any) []openai.ChatCompletionMessage {
	var sys strings.Builder
	sys.WriteString("你是优云智算 GPU 平台的部署选型助手。用户想创建一台 GPU 实例来运行某个模型或应用。\n")
	sys.WriteString("优云有两类现成镜像（均已预装环境，无需手动安装）：\n")
	sys.WriteString("1. 平台镜像(platform)：框架/系统底座，如 PyTorch、CUDA、ComfyUI、Ubuntu。适合“我要自己部署某个模型”（如部署 Qwen/Llama 用 PyTorch 或带 vLLM/Ollama 的底座）。\n")
	sys.WriteString("2. 社区镜像(community)：针对具体应用/模型打包好的开箱即用镜像，如数字人(LiveTalking/InfiniteTalk)、视频生成(LTX)等。适合“我要直接跑某个现成应用”。\n\n")
	sys.WriteString("从下面给出的候选镜像中选最合适的一个。只能选候选清单里真实存在的镜像名，不要编造。\n")
	sys.WriteString("严格只输出一个 JSON 对象，不要任何额外文字：\n")
	sys.WriteString(`{"image_source":"platform|community","image_name":"候选清单中的镜像名","model_name":"用户要运行的模型全称或留空","quantization":"留空或 fp16/int8/int4"}` + "\n")
	sys.WriteString("model_name 用于按显存推荐 GPU：用户明确提到模型(如 Qwen2.5-32B)就填，纯应用类(如数字人)留空。")

	var usr strings.Builder
	usr.WriteString("用户需求：" + strings.TrimSpace(userMsg) + "\n\n")
	usr.WriteString("【平台镜像候选】\n")
	usr.WriteString(platformDigest(platform))
	usr.WriteString("\n【社区镜像候选】\n")
	usr.WriteString(communityDigest(community))

	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sys.String()},
		{Role: openai.ChatMessageRoleUser, Content: usr.String()},
	}
}

func platformDigest(result map[string]any) string {
	if result == nil {
		return "（查询失败或无数据）\n"
	}
	set, _ := result["ImageSet"].([]any)
	if len(set) == 0 {
		return "（无）\n"
	}
	var b strings.Builder
	for _, item := range set {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["Name"].(string)
		if name == "" {
			continue
		}
		framework := ""
		if sw, ok := m["Softwares"].(map[string]any); ok {
			framework, _ = sw["Framework"].(string)
		}
		b.WriteString("- " + name)
		if framework != "" {
			b.WriteString(" [" + framework + "]")
		}
		if desc, _ := m["Description"].(string); desc != "" {
			b.WriteString("：" + truncateRunes(desc, 50))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func communityDigest(result map[string]any) string {
	if result == nil {
		return "（查询失败或无数据）\n"
	}
	groups, _ := result["CompshareImageGroup"].([]any)
	if len(groups) == 0 {
		return "（无）\n"
	}
	var b strings.Builder
	for _, item := range groups {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["ImageName"].(string)
		if name == "" {
			continue
		}
		b.WriteString("- " + name)
		if desc, _ := m["ImageDesc"].(string); desc != "" {
			b.WriteString("：" + truncateRunes(desc, 50))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
