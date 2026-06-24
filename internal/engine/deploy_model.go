package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

// deploy_model.go is the B8.3 agent-tier dispatch handler. deploy_model = "按用户需求
// 选优云已有镜像建实例并轮询到 Running" (the lead's 2026-05-31 reframe): the platform
// image already bakes in the framework/model, so this neither installs a model
// over SSH nor uses UserData — it MATCHES a need to an existing image, sizes the
// GPU, and creates the instance through the orchestrator saga.
//
// Why a dedicated handler and not a deterministic route: route handlers (DispatchRoute)
// reach only the ToolExecutor and cannot call e.RunAgentSaga, so routing
// deploy_model as a deterministic route would force a raw CreateCompShareInstance that
// bypasses the saga entirely — defeating B6.2/B8.2. This handler owns the engine, so
// it drives the saga (step-trace + StepConfirm HITL + L2-refuse).
//
// The saga reuses workflow.CreateInstanceDef() verbatim (no fork) — the handler only
// produces the param dict it already consumes (GpuType / ImageSource / ImageName)
// and recovers the new UHostId via a capture CheckResult, because workflow.Result
// carries only StepSummary, not step outputs. The handler replies on the saga's own
// post-create describe (step "查看状态"); it does NOT block the turn waiting for the
// instance to reach Running — a GPU instance can take minutes to initialize, and the
// manual CreateInstanceWorkflow path replies after a single describe too.

// deployPreferredZones is only the last-resort fallback when the live availability
// response carries no zone data. Normal deploy planning uses the zones returned by
// DescribeAvailableCompShareInstanceTypes, so newly added upstream zones participate
// without changing this list.
//
// The handler sizes the GPU against a zone's live cards and only creates there if
// that card has REAL stock (CheckCompShareResourceCapacity.ResourceEnough); a
// sold-out primary zone falls back BEFORE create (a pre-create selection, not an
// ADR-006-forbidden create-retry). DescribeAvailableCompShareInstanceTypes returns
// cards across multiple zones, so per-zone filtering is via AvailableType.Zone
// (knowledge.ParseAvailableGPUs(result, zone)).
var deployPreferredZones = []string{"cn-wlcb-01", "cn-sh2-02"}

// deployReadmeExcerptRunes caps the community-author Readme excerpt surfaced in
// the deploy reply. Live Readmes run ~1-2K runes of markdown+HTML; a short
// excerpt + a pointer to the console image-detail page keeps the reply readable.
const deployReadmeExcerptRunes = 400

// deployConsoleInstancesURL is the console's instance-list ("我的资源") page —
// where the user manages the just-created instance (state / login / billing).
// Static (no per-instance id needed) per the console's light-gpu resources route.
const deployConsoleInstancesURL = "https://console.compshare.cn/light-gpu/console/resources"

// deployPlan is the resolved deploy specification the matcher produces and the
// saga consumes. ImageSource/ImageName/GpuType are CreateInstanceDef params.
type deployPlan struct {
	ImageSource  string // "platform" | "community"
	ImageName    string // image Name (platform) / group ImageName (community); may be "" for platform
	ImageID      string // resolved CompShareImageId of the chosen image; threaded to the saga so it creates EXACTLY this image (may be "")
	GpuType      string // CreateInstance GpuType, e.g. "A100"
	ModelName    string // model the user wants to run, for the reply; may be ""
	MatchKind    string // the matcher's judgment of the image↔model fit: "exact" (a ready-made image FOR the named model / the named app) | "base" (a framework base to self-host the model on); drives the self-deploy hint. May be "" (legacy/app → treated as exact, no hint).
	MatchNote    string // human-readable selection rationale (GPU sizing + any fallback)
	ChosenZone   string // resolved create-zone (preference + per-zone stock); "" → saga default
	ZoneLabel    string // display-only zone name, e.g. 华北一C. ChosenZone remains the create API value.
	FallbackNote string // set when the create-zone differs from the primary (sold-out fallback / user zone)

	// Quantization + SupportedGPUs are retained so the stock-shortage reply can
	// size and image-filter alternative cards (knowledge.FittingGPUAlternatives)
	// when the recommended card is sold out at create time.
	Quantization  string   // the matcher's chosen quantization (drives VRAM math); may be ""
	SupportedGPUs []string // the chosen image's SupportedGpuTypes (image↔GPU compat); may be empty
	UserPinnedGPU string   // GPU explicitly named by the user, resolved from the live availability catalog

	// CandidateGPUs is the small, already-validated GPU set the guided create card
	// should show for model deploys. GPUReasons carries display-only rationale.
	CandidateGPUs []string
	GPUReasons    map[string]string

	// StockConfirmed means the pre-create capacity probe confirmed the selected
	// single-GPU config has real stock. When false, guided copy must not claim
	// inventory is sufficient; the workflow capacity gate will continue checking.
	StockConfirmed bool
}

// tryDeployModel handles an IntentDeployModel turn end-to-end. It ALWAYS returns
// handled=true — deploy_model is a dedicated skill and never falls through to
// the generic ReAct loop; failures surface as a friendly reply, not a fallback.
//
// Advise-first (deploy v2): the matcher runs FIRST, unconditionally, producing a
// GPU + image + zone recommendation. In read-only mode (shipped default) the handler
// stops there and returns that advice — so "跑X用哪个卡 / 帮我搭个能跑Y的环境" gives
// a useful answer instead of a flat refusal. Only when writes are enabled does the
// recommendation flow into the create saga, gated by the confirm card.
func (e *Engine) tryDeployModel(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	result := dispatch.result

	// A short refinement follow-up ("A800可以吗" / "换上海") carries no model on its
	// own; rehydrate it from the previous deploy target so the matcher keeps sizing
	// the SAME model instead of treating it as a new generic deploy request.
	effectiveUserMsg := e.effectiveDeployUserMsg(userMsg)
	if e.createPreferenceThisTurn != nil {
		effectiveUserMsg = deployMessageWithCreatePreference(effectiveUserMsg, *e.createPreferenceThisTurn)
	}

	// (1a) Resolve a user-named zone against the live support-zone catalog so a
	// Chinese display name ("华北一C") maps to its zone id (cn-bj2-03) instead of
	// being silently dropped to the platform default. A partial/ambiguous/unsupported
	// mention ("华北一区") returns a clarify reply — we stop and ask rather than guess.
	userZone, zoneClarify := e.resolveRequestedZone(ctx, effectiveUserMsg)
	if zoneClarify != "" {
		return e.deployReply(result, dispatch.latency, zoneClarify)
	}

	// (1) Match an existing image + size the GPU + pick the create-zone (TierAgent
	// judgment + deterministic VRAM/stock arithmetic). Runs in read-only mode too —
	// it is all read-only queries, and its output IS the advice. A zone the user
	// named in the request (e.g. "在上海部署") is honored strictly; a GPU the user
	// names is honored strictly by selectDeployZoneAndGPU (extractDeployGPU).
	plan, err := e.matchDeployImage(ctx, effectiveUserMsg, userZone, onStep)
	if err != nil {
		// A deployUserError carries a specific, actionable message (e.g. "the zone
		// you named has no suitable in-stock card") — surface it verbatim instead of
		// masking it with the generic "tell me what to deploy" clarification.
		var ue deployUserError
		if errors.As(err, &ue) {
			return e.deployReply(result, dispatch.latency, ue.Error())
		}
		return e.deployReply(result, dispatch.latency,
			"抱歉，我没能确定合适的镜像和配置。可以告诉我你想部署的模型（如 Qwen2.5-32B）或应用（如 ComfyUI / 数字人）吗？")
	}
	e.emitDeployStep(onStep, StepToolResult, "deploy_match",
		fmt.Sprintf("已选型：%s 镜像 %s / GPU %s。%s", sourceLabel(plan.ImageSource), plan.ImageName, plan.GpuType, plan.MatchNote))

	// (2) Advice gate. deploy_model creates a billable instance, so we return the
	// matcher's recommendation as ADVICE (no saga) in two cases:
	//   - writes disabled (shipped read-only default) — the handler cannot create; and
	//   - the request only ASKS for a recommendation / how-to ("推荐我用哪种卡部署",
	//     "怎么部署") rather than commanding a create — it must not silently enter the
	//     create saga (real session s_fd7f1b9669fd: a "which card?" question hit a
	//     stock-out create instead of advising). The matcher already produced the
	//     GPU+image recommendation either way; buildAdviseReply's footer adapts (the
	//     mutating-on advice case offers to proceed on an explicit restate).
	if !e.mutatingToolsEnabled || deployIsAdviceOnly(effectiveUserMsg) {
		return e.deployReply(result, dispatch.latency, buildAdviseReply(plan, e.mutatingToolsEnabled))
	}

	// (3) Drive CreateInstanceDef through the orchestrator saga. Reuse the shipped
	// definition verbatim; inject capture hooks to recover the created instance id
	// (Result carries only StepSummary, not step outputs).
	def := workflow.CreateInstanceDef()
	if e.guidedCreate && e.confirmEditsFn != nil {
		def = workflow.CreateInstanceGuidedDef()
	}
	var createResult, describeResult map[string]any
	captureStepResult(def, "创建实例", func(r map[string]any) { createResult = r })
	captureStepResult(def, "查看状态", func(r map[string]any) { describeResult = r })

	params := map[string]any{
		"GpuType":     plan.GpuType,
		"ImageSource": plan.ImageSource,
	}
	// deploy_model always reaches here with a matcher-sized GPU + image, so the
	// guided cards should present them AS a recommendation (default-selected +
	// 推荐 badge), not as a blank pick. Distinct from GuidedGpuLocked, which fires
	// only when the user named a GPU explicitly and hard-filters the GPU card.
	params["GuidedRecommended"] = true
	if len(plan.CandidateGPUs) > 0 {
		params["GuidedCandidateGPUs"] = plan.CandidateGPUs
	}
	if len(plan.GPUReasons) > 0 {
		params["GuidedGpuReasons"] = plan.GPUReasons
	}
	if plan.UserPinnedGPU != "" {
		params["GuidedGpuLocked"] = true
	}
	if plan.ImageName != "" {
		params["ImageName"] = plan.ImageName
	}
	// Thread the resolved image id so the saga creates EXACTLY the image the matcher
	// chose + sized the GPU against, instead of re-resolving independently (platform's
	// CJK Name filter and community's index-0 pick can both diverge from the choice).
	// Empty ImageID → not threaded → saga uses its own resolution + fail-loud guard.
	if plan.ImageID != "" {
		params["CompShareImageId"] = plan.ImageID
	}
	// Thread the chosen zone (preference + per-zone stock) so the saga creates in the
	// zone the handler confirmed, not its hardcoded default; and the fallback note so the
	// confirm card can tell the user a sold-out primary zone was switched.
	if plan.ChosenZone != "" {
		params["Zone"] = plan.ChosenZone
		if isPod, ok := e.zoneIsPod(ctx, plan.ChosenZone); ok {
			params["ZoneIsPod"] = isPod
		}
	}
	if plan.FallbackNote != "" {
		params["FallbackNote"] = plan.FallbackNote
	}
	// Display-only: label the confirm form's zone options with the console's
	// Chinese names ("华北一C") instead of bare zone ids. Ignored by every API
	// step (which read specific keys); consumed only by buildCreateConfirmForm.
	if descMap := e.zoneDescribeMap(ctx); len(descMap) > 0 {
		params["ZoneDescribes"] = descMap
	}
	if idMap := e.zoneIDMap(ctx); len(idMap) > 0 {
		params["ZoneIds"] = idMap
	}
	if podMap := e.zoneIsPodMap(ctx); len(podMap) > 0 {
		params["ZoneIsPods"] = podMap
	}
	if u, ok := tools.UserFrom(ctx); ok {
		if u.TopOrganizationID != 0 {
			params["top_organization_id"] = u.TopOrganizationID
		}
		if u.OrganizationID != 0 {
			params["organization_id"] = u.OrganizationID
		}
	}

	sagaResult, sagaErr := e.RunAgentSaga(ctx, def, params, "deploy_model")
	if sagaErr != nil {
		// Programming/validation error (nil def / L2 in def) — not a step failure.
		return e.deployReply(result, dispatch.latency,
			fmt.Sprintf("创建流程未能启动：%v", sagaErr))
	}
	if !sagaResult.Success {
		return e.deployReply(result, dispatch.latency, e.deployStopReplyWithAlternatives(ctx, sagaResult, plan))
	}

	// (4) Recover the new instance id and reply on the saga's own post-create
	// describe (step "查看状态"). We deliberately do NOT block the turn polling
	// until the instance reaches Running: a GPU instance can take several minutes
	// to finish initializing, and holding the turn open that long stalls the SSE
	// stream and the frontend's post-create jump to the console. The manual
	// CreateInstanceWorkflow path replies after a single describe too, so the two
	// create paths stay symmetric. Login/usage details that only exist once the
	// instance is Running (SSH command, public IP) are surfaced on the console's
	// instance-list page, which the reply links to.
	uHostId := firstUHostID(createResult)
	if uHostId == "" {
		// Grounding guard: saga succeeded but capture didn't fire (step renamed?).
		// Fail loud rather than silently skip the status read.
		return e.deployReply(result, dispatch.latency,
			"实例已创建，但未能解析实例 ID。请用「查询我的实例」查看最新状态。")
	}
	host := firstHost(describeResult)
	state := stringFromHost(host, "State")

	// (5) Fetch the chosen image's usage guidance (which apps on which ports +
	// the community author's Readme) so the reply can tell the user HOW to use
	// the instance — an SSH command alone doesn't say "ComfyUI is on :8188".
	// Read-only, success-path only; degrades to no-guidance on any error.
	usage := e.fetchImageUsage(ctx, plan)

	return e.deployReply(result, dispatch.latency, buildDeployReply(plan, uHostId, host, state, usage))
}

// deployReply emits the planner trace and appends the assistant message, then
// returns (reply, true). The status is always RouteStatusDispatchedAgent: the
// agent-tier deploy handler owned the turn (TierAgent match + orchestrator saga), so
// DeriveActualExecutionTier labels it the agent tier — mirroring how route dispatch
// emits "dispatched"→fast even on refusal. Centralizes the three return-side
// concerns so every exit path of tryDeployModel stays consistent.
func (e *Engine) deployReply(result intent.IntentRouterResult, latency time.Duration, reply string) (string, bool) {
	e.emitPlannerTrace(result, intent.RouteStatusDispatchedAgent, latency)
	e.recordLastIntentFromPlan(result.Plan)
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
}

// effectiveDeployUserMsg keeps a short deploy follow-up grounded in the previous
// deploy target. Example: after "deploy Qwen2.5 32B", a follow-up "A800可以吗" must
// be matched as "Qwen2.5-32B on A800", NOT as a fresh generic deploy request that
// drops the model. It only rewrites when the current message (a) names no sized
// model of its own, AND (b) DOES refine a GPU or zone — i.e. it reads like a
// follow-up — and a previous turn carried a sized-model target. Otherwise it
// returns the message unchanged, so a self-contained request is never altered.
func (e *Engine) effectiveDeployUserMsg(userMsg string) string {
	msg := strings.TrimSpace(userMsg)
	if msg == "" {
		return userMsg
	}
	sized := extractDeploySizedModelName(msg)

	// A BARE size follow-up ("32B" answering a "which size?" clarify) names a size
	// but no model family. Re-attach the model the previous turn was clarifying — read
	// from our OWN clarify message in history — so "DeepSeek R1" + "32B" sizes & picks
	// for DeepSeek-R1-32B, not a generic 32B base. (Real session s_c05ecbeccce4: the
	// "32B" answer lost the DeepSeek-R1 identity.) A sized name that already carries a
	// family ("Qwen2.5-32B") is self-contained and returned unchanged.
	if isBareDeploySize(sized) {
		if model := e.previousDeployClarifyModel(); model != "" {
			return "继续部署 " + model + " " + sized
		}
		return userMsg
	}
	if sized != "" {
		return userMsg
	}
	if extractDeployGPU(msg) == "" && extractDeployZone(msg) == "" {
		return userMsg
	}

	model, zone := e.previousDeployTarget(msg)
	if model == "" {
		return userMsg
	}

	parts := []string{"继续部署 " + model}
	if extractDeployZone(msg) == "" && zone != "" {
		parts = append(parts, "沿用可用区 "+zone)
	}
	parts = append(parts, "用户追问："+msg)
	return strings.Join(parts, "；")
}

// previousDeployTarget scans conversation history backward for the most recent USER
// message that named a sized model, returning that model and any zone it named. The
// follow-up reuses these so "A800可以吗" continues to size the earlier model. Skips
// the current message itself (it is already in history by dispatch time).
func (e *Engine) previousDeployTarget(currentMsg string) (model, zone string) {
	currentMsg = strings.TrimSpace(currentMsg)
	for i := len(e.messages) - 1; i >= 0; i-- {
		m := e.messages[i]
		if m.Role != openai.ChatMessageRoleUser {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" || content == currentMsg {
			continue
		}
		if target := extractDeploySizedModelName(content); target != "" {
			return target, extractDeployZone(content)
		}
	}
	return "", ""
}

// bareDeploySizeRE matches a size-only token ("32B", "1.5B") — the shape
// extractDeploySizedModelName returns when the user names a parameter count with no
// model family (a bare "32B" answer to a size-clarify).
var bareDeploySizeRE = regexp.MustCompile(`(?i)^\d+(?:\.\d+)?b$`)

// isBareDeploySize reports whether a normalized deploy model name is size-only (no
// family). Such a follow-up must inherit the model family from the clarify it answers.
func isBareDeploySize(name string) bool {
	return bareDeploySizeRE.MatchString(strings.TrimSpace(name))
}

// deployClarifyModelRE pulls the model family out of OUR OWN size-clarify message
// ("「DeepSeek R1」有多个参数规模…"). We control that message's format
// (deployClarifyModelSizeMsg), so this is a reliable in-band carry — no fragile
// re-extraction of an unsized family from the user's free text. Keep this anchor in
// sync with deployClarifyModelSizeMsg.
var deployClarifyModelRE = regexp.MustCompile(`「(.+?)」有多个参数规模`)

// previousDeployClarifyModel returns the model named by the most recent assistant
// size-clarify, or "" when the last assistant turn was not a size-clarify. It stops
// at the first assistant message scanned backward, so a stale, already-answered
// clarify cannot leak into an unrelated later "<n>B" message.
func (e *Engine) previousDeployClarifyModel() string {
	for i := len(e.messages) - 1; i >= 0; i-- {
		m := e.messages[i]
		if m.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		if mm := deployClarifyModelRE.FindStringSubmatch(m.Content); mm != nil {
			return strings.TrimSpace(mm[1])
		}
		return "" // most recent assistant turn wasn't a size-clarify → no carry
	}
	return ""
}

// deployCreateCommandRE matches an explicit "do it now" create command ("帮我部署X" /
// "现在创建") — it overrides an advice marker so "帮我部署你推荐的那台" still creates.
var deployCreateCommandRE = regexp.MustCompile(`(帮我|为我|给我|替我|直接|立即|立刻|马上|现在|赶紧)[的来]?(部署|创建)`)

// deployAdviceOnlyRE matches a deploy request that ASKS for a recommendation / how-to
// rather than commanding a create: a bare 推荐/建议, a "which/what 卡/GPU/机型/配置/镜像"
// question, or a "怎么/如何…部署" how-to.
var deployAdviceOnlyRE = regexp.MustCompile(`(?i)推荐|建议|(哪种|哪个|哪款|哪张|哪台|什么|啥)[^。！？\n]{0,6}(卡|gpu|显卡|机型|规格|配置|镜像)|(怎么|怎样|如何|咋)[^。！？\n]{0,4}部署`)

// deployPriceQuestionRE catches price/billing questions in deploy phrasing. It
// intentionally excludes a bare "按量/包月" because those can be charge-type
// choices in an explicit create request ("部署一台 A100，按量").
var deployPriceQuestionRE = regexp.MustCompile(`(?i)(多少钱|价格|费用|计费|每小时|一小时|小时价|贵不贵|包月价|包日价|按量价)`)

// deployTrainingAdviceRE catches exploratory training / fine-tuning tasks. These
// usually need a recommendation or a follow-up question before provisioning; only
// an explicit create command should turn them into a billable instance flow.
var deployTrainingAdviceRE = regexp.MustCompile(`(?i)(微调|fine[-_ ]?tun(?:e|ing)|训练|train(?:ing)?)[^。！？\n]{0,24}(模型|model|rvc|lora|数据集|dataset)?`)

// deployIsAdviceOnly reports whether a deploy request only wants a recommendation /
// how-to ("推荐我用哪种卡部署" / "用什么显卡" / "怎么部署") rather than a create command
// ("帮我部署X" / "部署一台A100"). Such a request must get the matcher's recommendation
// as advice, never silently enter the create saga — real session s_fd7f1b9669fd:
// "LiveTalking 推荐我用哪种卡部署" hit a stock-out create instead of advising which
// card. An explicit create command wins over an incidental advice marker. Safe-biased:
// when in doubt it advises (one extra "部署X" turn) rather than create an unwanted
// billable instance.
func deployIsAdviceOnly(userMsg string) bool {
	s := strings.ToLower(strings.TrimSpace(userMsg))
	if deployPriceQuestionRE.MatchString(s) {
		return true
	}
	if deployCreateCommandRE.MatchString(s) {
		return false
	}
	if deployTrainingAdviceRE.MatchString(s) {
		return true
	}
	return deployAdviceOnlyRE.MatchString(s) || deployBareWorkloadAdviceOnly(s)
}

func deployBareWorkloadAdviceOnly(userMsg string) bool {
	s := strings.TrimSpace(userMsg)
	if s == "" || deploySizedModelRE.MatchString(s) {
		return false
	}
	for _, marker := range []string{"部署", "创建", "搭", "跑", "运行", "启动", "开一台", "搞台", "抢一台", "买一台", "租一台"} {
		if strings.Contains(s, marker) {
			return false
		}
	}
	compact := normalizeResourceText(s)
	return compact != "" && len([]rune(compact)) <= 14
}

// deploySizedModelRE matches a parameter-sized model mention ("Qwen2.5-32B",
// "32B", "Llama3 70B") — an optional name prefix followed by a number and a 'B'
// (billions of params) on a word boundary. Deterministic (Rule 5: a structured
// token, no LLM needed). It is intentionally loose on the prefix so it recognizes
// the size even when the family name varies; the size is the part the GPU sizer
// needs to carry across a follow-up.
var deploySizedModelRE = regexp.MustCompile(`(?i)([a-z][a-z0-9._/-]*?)?\s*[-_ ]?\s*(\d+(?:\.\d+)?)\s*b\b`)

// extractDeploySizedModelName returns the first parameter-sized model name in text,
// normalized ("Qwen2.5 32B" → "Qwen2.5-32B"; "32B 模型" → "32B"), or "" if none.
func extractDeploySizedModelName(text string) string {
	matches := deploySizedModelRE.FindAllStringSubmatch(strings.TrimSpace(text), -1)
	if len(matches) == 0 {
		return ""
	}
	m := matches[0]
	prefix := strings.Trim(m[1], "-_/. ")
	size := strings.ToUpper(strings.TrimSpace(m[2]) + "B")
	if prefix == "" {
		return size
	}
	return prefix + "-" + size
}

// shouldClarifyDeployModelSize reports whether a deploy request named a model
// whose parameter size is ambiguous and the user pinned no GPU — in which case
// the flow asks which size instead of sizing for a silent default. App deploys
// (empty model_name per the match prompt — ComfyUI/数字人) and explicit GPUs
// return false. Pure / deterministic.
func shouldClarifyDeployModelSize(modelName, userMsg, userPinnedGPU string) bool {
	return strings.TrimSpace(modelName) != "" &&
		strings.TrimSpace(userPinnedGPU) == "" &&
		extractDeployGPU(userMsg) == "" &&
		!knowledge.ModelParamCountResolvable(modelName)
}

// deployClarifyModelSizeMsg asks which size of a multi-size model family to deploy.
// It now fires ONLY for a genuine multi-size family (the call site ANDs the matcher's
// size_ambiguous judgment), so the message can state "有多个参数规模" directly without
// the earlier hedging — a single specific model the table doesn't know
// ("Fish Audio S2-Pro") no longer reaches here. Keep the "「%s」有多个参数规模" prefix in
// sync with deployClarifyModelRE (the bare-size follow-up combine reads it back).
func deployClarifyModelSizeMsg(modelName string) string {
	name := strings.TrimSpace(modelName)
	return fmt.Sprintf("「%s」有多个参数规模（如 7B / 32B / 70B 等），你想部署哪个？直接回参数量（如「32B」）或完整型号（如「%s-32B」）即可，我就帮你选机型和镜像。", name, name)
}

// matchDeployImage queries the live image catalog, asks the TierAgent model to
// pick the best fit, and sizes the GPU image-aware. The LLM does the fuzzy
// judgment (a keyword to search + which image + which model/quantization);
// knowledge.RecommendGPUTypeWithin does the VRAM arithmetic constrained to what
// the chosen image supports. The chosen image is grounded against the queried
// catalog so a hallucinated name cannot reach the saga.
//
// Catalog handling is asymmetric by design (verified by live recon 2026-05-31):
//   - Platform (DescribeCompShareImages, ~68 images) is small AND its server-side
//     Name filter does not match the CJK canonical names ("comfyui"→0 hits) — so
//     we fetch the WHOLE catalog (Limit=100) and let the model read it. Note: the
//     platform catalog contains BOTH framework bases AND app images (ComfyUI/vLLM/
//     Ollama/SGLang are App-type platform images by Name) — there is NO
//     platform=framework / community=app dichotomy.
//   - Community (DescribeCommunityImages, ~743 groups) is too large to show whole,
//     but its FuzzySearch (name+author) works well — so we extract a keyword first
//     (the lead's Q1: let the model understand an imprecise request) and search
//     with it, falling back to an unfiltered sample if the keyword finds nothing.
func (e *Engine) matchDeployImage(ctx context.Context, userMsg, userZone string, onStep func(StepEvent)) (deployPlan, error) {
	client := e.agentLLMClient
	if client == nil {
		client = e.llmClient // NewWithDeps test path / no tier_routing.agent configured
	}
	if client == nil {
		return deployPlan{}, fmt.Errorf("deploy_model: no LLM client available for image matching")
	}
	e.emitDeployStep(onStep, StepToolCall, "deploy_match", "正在理解你的需求并查询可用镜像…")

	// (a) Extract a community search keyword from the (possibly vague) request.
	search := e.extractDeploySearch(ctx, client, userMsg)

	// (b) Query both catalogs: platform whole (small + broken Name filter),
	// community keyword-filtered (large + working FuzzySearch). Community uses
	// multiple deterministic terms as well as the LLM keyword so an exact model
	// image like DeepSeek-R1:32b is present even when the first keyword is too broad.
	platform := e.querySafeRead(ctx, "DescribeCompShareImages", map[string]any{"Limit": 100})
	community := e.queryCommunityCandidates(ctx, deployCommunitySearchTerms(userMsg, search)...)
	platformNames := platformImageNames(platform)
	communityNames := communityGroupNames(community)

	// (c) Final pick over the real candidate lists.
	prompt := buildImageMatchPrompt(userMsg, platform, community)
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: prompt})
	if err != nil {
		return deployPlan{}, fmt.Errorf("deploy_model: image-match LLM call failed: %w", err)
	}
	e.emitTokenUsage(resp.Usage)

	var decision struct {
		ImageSource   string `json:"image_source"`
		ImageName     string `json:"image_name"`
		ModelName     string `json:"model_name"`
		MatchKind     string `json:"match_kind"`
		SizeAmbiguous bool   `json:"size_ambiguous"`
		Quantization  string `json:"quantization"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(resp.Content)), &decision); err != nil {
		return deployPlan{}, fmt.Errorf("deploy_model: cannot parse image-match decision: %w", err)
	}

	plan := deployPlan{
		ImageSource: strings.ToLower(strings.TrimSpace(decision.ImageSource)),
		ImageName:   strings.TrimSpace(decision.ImageName),
		ModelName:   strings.TrimSpace(decision.ModelName),
		MatchKind:   strings.ToLower(strings.TrimSpace(decision.MatchKind)),
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
			// base (always present), which is a safe default for a deploy request.
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

	// (c.1) Ambiguous model size → ask which one instead of silently sizing for a
	// default variant. The user's complaint: "部署 DeepSeek R1" (spans 1.5B–671B)
	// shouldn't dead-pick the 671B/H20 config. Fires only for a multi-size FAMILY
	// the user didn't pin a size for: the deterministic check (size unresolvable from
	// name + no GPU) is ANDed with the matcher's size_ambiguous judgment, so a single
	// specific model the table doesn't know ("Fish Audio S2-Pro") is NOT asked a
	// confusing size question — it falls through to a base deploy + self-host hint.
	// App deploys (empty model_name) and explicit GPUs/sizes proceed unchanged.
	// Surfaced via deployUserError so tryDeployModel returns it verbatim (it's an
	// actionable clarification, not a failure).
	availResult := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
	userPinnedGPU := extractDeployGPUFromCatalog(userMsg, availResult)
	if decision.SizeAmbiguous && shouldClarifyDeployModelSize(plan.ModelName, userMsg, userPinnedGPU) {
		return deployPlan{}, deployUserError{msg: deployClarifyModelSizeMsg(plan.ModelName)}
	}

	// Exact community model images beat framework bases. The LLM may reasonably pick
	// Ollama/vLLM as a base, but if the real community catalog contains a ready-made
	// image for the same model+size, that is the better grounded deploy target.
	if exactName, ok := exactCommunityModelImageName(plan.ModelName, userMsg, community); ok {
		plan.ImageSource = "community"
		plan.ImageName = exactName
		plan.MatchKind = "exact"
		if strings.TrimSpace(plan.ModelName) == "" {
			plan.ModelName = extractDeploySizedModelName(userMsg)
		}
		groundNote = fmt.Sprintf("发现现成 %s 镜像，优先使用社区镜像", exactName)
	}

	// (d) Resolve the chosen image's id (threaded to the saga so it creates EXACTLY
	// this image, not a re-resolved one) and size the GPU constrained to that same
	// image's recommended cards (M2) — against the LIVE available-card set, so a
	// stale static table can't recommend a retired/sold-out card or miss a new one.
	// The static gpuSpecs table is only the offline fallback (empty live set).
	imageID, supported := chosenImage(plan, platform, community)
	plan.ImageID = imageID
	plan.UserPinnedGPU = userPinnedGPU
	var imagePreferenceNote string
	userZoneIsPod, _ := e.zoneIsPod(ctx, userZone)
	if updated, updatedSupported, note := preferDeployPlatformImageForGPU(plan, platform, userPinnedGPU, userZone, userZoneIsPod); note != "" {
		plan = updated
		supported = updatedSupported
		imagePreferenceNote = note
	}
	// (e) Size the GPU AND pick the create-zone together (interdependent: zones
	// offer different cards, and a card must have real stock in the zone we create
	// in). DescribeAvailableCompShareInstanceTypes takes no Zone request param and
	// its response spans MULTIPLE zones (region cn-wlcb includes Shanghai cn-sh2-02);
	// selectDeployZoneAndGPU filters per-zone via AvailableType.Zone, sizes against
	// each zone's live cards, and prefers the first zone with confirmed stock.
	sizingPlan := plan
	if plan.ImageSource == "community" && plan.MatchKind == "exact" && strings.TrimSpace(decision.Quantization) == "" {
		// A ready-made community model image is usually pre-packaged in its own
		// runtime/quantization form. Do not force generic fp16 VRAM sizing unless the
		// matcher explicitly chose a quantization; rank by image support + live stock.
		sizingPlan.ModelName = ""
	}
	zone, gpuType, gpuNote, fallbackNote, zerr := e.selectDeployZoneAndGPU(ctx, availResult, sizingPlan, supported, decision.Quantization, userMsg, userZone)
	if zerr != nil {
		// User pinned a zone we can't satisfy — surface, never silently override.
		return deployPlan{}, zerr
	}
	plan.GpuType = gpuType
	plan.ChosenZone = zone
	plan.ZoneLabel = e.zoneDisplayName(ctx, zone)
	plan.MatchNote = gpuNote
	plan.FallbackNote = fallbackNote
	finalZoneIsPod, _ := e.zoneIsPod(ctx, zone)
	if updated, updatedSupported, note := alignDeployPlatformImageForGPU(plan, platform, plan.GpuType, zone, finalZoneIsPod); note != "" {
		plan = updated
		supported = updatedSupported
		if imagePreferenceNote != "" {
			imagePreferenceNote += "；" + note
		} else {
			imagePreferenceNote = note
		}
	}
	plan.StockConfirmed = e.zoneStockState(ctx, zone, gpuType, plan.ImageID) == zoneInStock
	// Retain the sizing inputs so a stock-shortage reply can offer image-compatible,
	// VRAM-sufficient alternative cards (see deployAlternativesNote).
	plan.Quantization = decision.Quantization
	plan.SupportedGPUs = supported
	plan.CandidateGPUs, plan.GPUReasons = deployGuidedGPUCandidates(plan, supported, availResult, sizingPlan.ModelName, decision.Quantization, userMsg)
	if groundNote != "" {
		plan.MatchNote = groundNote + "；" + plan.MatchNote
	}
	if imagePreferenceNote != "" {
		plan.MatchNote = imagePreferenceNote + "；" + plan.MatchNote
	}

	// (f) GPU↔image compatibility gate. Capacity preflight may catch image status
	// and adaptive-image failures, but SupportedGpuTypes is still not a complete
	// upstream hard gate. Keep it as an agent-side risk gate before create: if the
	// image declares supported types and the sized card isn't among them, the image
	// is not a good match for this workload. On the auto path this only happens when
	// no supported card had enough VRAM (RecommendGPUType kept the
	// VRAM-correct-but-unsupported card), so we surface that as an actionable message
	// instead of letting the create fail late.
	if !gpuImageCompatible(plan.GpuType, supported) {
		if updated, updatedSupported, note := alignDeployPlatformImageForGPU(plan, platform, plan.GpuType, userZone, userZoneIsPod); note != "" && gpuImageCompatible(plan.GpuType, updatedSupported) {
			plan = updated
			supported, plan.SupportedGPUs = updatedSupported, updatedSupported
			plan.MatchNote = note + "；" + plan.MatchNote
		} else {
			return deployPlan{}, deployUserError{msg: fmt.Sprintf(
				"所选镜像「%s」支持的机型为 %s，其显存不足以运行该工作负载。建议换一个支持更大显存机型的镜像，或选择更小的模型 / 量化版本。",
				plan.ImageName, strings.Join(supported, "、"))}
		}
	}
	return plan, nil
}

// gpuImageCompatible reports whether gpuType is allowed by the image's declared
// SupportedGpuTypes. An empty list means "no per-image constraint" → compatible.
// This MUST be checked before create: capacity preflight is not a complete
// image↔GPU compatibility proof, so an incompatible combo may still only fail at
// CreateCompShareInstance.
//
// It compares against the RAW SupportedGpuTypes (not the static-table-normalized
// subset RecommendGPUTypeWithin uses) on purpose: the create API validates against
// the image's literal supported list, including cards the static gpuSpecs table
// doesn't know about, so the gate must too.
func gpuImageCompatible(gpuType string, supported []string) bool {
	if strings.TrimSpace(gpuType) == "" || len(supported) == 0 {
		return true
	}
	for _, s := range supported {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(gpuType)) {
			return true
		}
	}
	return false
}

func preferDeployPlatformImageForGPU(plan deployPlan, platform map[string]any, requestedGPU, zone string, zoneIsPod bool) (deployPlan, []string, string) {
	return chooseDeployPlatformImageForGPU(plan, platform, requestedGPU, zone, zoneIsPod,
		func(gpu, name string) string {
			return fmt.Sprintf("你指定了 %s，已切换到支持该 GPU 的镜像「%s」", gpu, name)
		})
}

func alignDeployPlatformImageForGPU(plan deployPlan, platform map[string]any, requestedGPU, zone string, zoneIsPod bool) (deployPlan, []string, string) {
	return chooseDeployPlatformImageForGPU(plan, platform, requestedGPU, zone, zoneIsPod,
		func(gpu, name string) string {
			return fmt.Sprintf("所选镜像不适配 %s，已切换到支持该 GPU 的同类镜像「%s」", gpu, name)
		})
}

func chooseDeployPlatformImageForGPU(plan deployPlan, platform map[string]any, requestedGPU, zone string, zoneIsPod bool, note func(gpu, name string) string) (deployPlan, []string, string) {
	if plan.ImageSource != "platform" || strings.TrimSpace(plan.ImageName) == "" || strings.TrimSpace(requestedGPU) == "" {
		return plan, nil, ""
	}
	images := deployPlatformImagesMatchingName(platform, plan.ImageName)
	if len(images) == 0 {
		return plan, nil, ""
	}
	candidates, byID := deployPlatformImageCandidates(images)
	selected := deployment.SelectImageCandidates(deployment.ImageSelectionInput{
		Images:       candidates,
		RequestedGPU: requestedGPU,
		Zone:         deployment.ZoneConstraint{Zone: zone, IsPod: zoneIsPod},
	})
	if len(selected.Viable) == 0 || containsString(selected.Viable[0].Warnings, deployment.WarningSupportedGPUMismatch) {
		return plan, nil, ""
	}
	best := selected.Viable[0].Image
	img := byID[best.ID]
	if img == nil || best.ID == "" || best.ID == plan.ImageID {
		return plan, nil, ""
	}
	name, _ := img["Name"].(string)
	updated := plan
	updated.ImageName = name
	updated.ImageID = best.ID
	return updated, best.SupportedGPUTypes, note(requestedGPU, name)
}

func deployPlatformImagesMatchingName(platform map[string]any, imageName string) []map[string]any {
	set, _ := platform["ImageSet"].([]any)
	if len(set) == 0 {
		return nil
	}
	lowerName := strings.ToLower(strings.TrimSpace(imageName))
	var out []map[string]any
	for _, item := range set {
		img, _ := item.(map[string]any)
		if img == nil {
			continue
		}
		name, _ := img["Name"].(string)
		if deployImageNamesRelated(lowerName, strings.ToLower(strings.TrimSpace(name))) {
			out = append(out, img)
		}
	}
	return out
}

func deployImageNamesRelated(target, candidate string) bool {
	if target == "" || candidate == "" {
		return false
	}
	if target == candidate || strings.Contains(candidate, target) {
		return true
	}
	// Generic sibling relation: "vLLM v0.12.0-<gpu>" and "vLLM v0.12.0"
	// share a prefix. This avoids card-specific suffix tables while still keeping
	// the swap scoped to the selected image family.
	if len([]rune(candidate)) >= 6 && strings.HasPrefix(target, candidate) {
		return true
	}
	nt, nc := normalizeDeployComparableName(target), normalizeDeployComparableName(candidate)
	if nt == "" || nc == "" {
		return false
	}
	return nt == nc || strings.Contains(nc, nt) || (len([]rune(nc)) >= 6 && strings.HasPrefix(nt, nc))
}

func normalizeDeployComparableName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func deployPlatformImageCandidates(images []map[string]any) ([]deployment.ImageCandidate, map[string]map[string]any) {
	candidates := make([]deployment.ImageCandidate, 0, len(images))
	byID := make(map[string]map[string]any, len(images))
	for i, img := range images {
		id, _ := img["CompShareImageId"].(string)
		if id == "" {
			id = fmt.Sprintf("__image_%d", i)
		}
		name, _ := img["Name"].(string)
		imageType, _ := img["ImageType"].(string)
		status, _ := img["Status"].(string)
		candidates = append(candidates, deployment.ImageCandidate{
			ID:                id,
			Name:              name,
			ImageType:         imageType,
			Container:         boolFromAny(img["Container"]) || boolFromAny(img["IsContainer"]),
			Status:            status,
			SupportedGPUTypes: stringSliceFromAny(img["SupportedGpuTypes"]),
		})
		byID[id] = img
	}
	return candidates, byID
}

func containsString(list []string, needle string) bool {
	for _, item := range list {
		if item == needle {
			return true
		}
	}
	return false
}

func boolFromAny(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	default:
		return false
	}
}

// deployUserError is a matchDeployImage error whose message is safe + actionable
// to show the user verbatim (vs an internal failure that gets a generic
// clarification). Used for the user-specified-zone-unsatisfiable case.
type deployUserError struct{ msg string }

func (e deployUserError) Error() string { return e.msg }

// selectDeployZoneAndGPU sizes the GPU and chooses the create-zone in one pass,
// because the two are interdependent. A user-specified zone (userZone != "") is
// honored STRICTLY: if no card can host the workload there, or it is sold out, it
// returns an error rather than quietly moving the user to another zone. Otherwise
// it tries deployPreferredZones in order, sizing against each zone's live cards
// and selecting the first zone whose recommended card has REAL stock
// (CheckCompShareResourceCapacity.ResourceEnough). If no zone has confirmed stock
// it falls through to the first sizeable zone and lets the saga's capacity gate
// have the final word (so behavior degrades to today's, never worse). An empty
// live set (query failed) degrades to the static-table sizing on the primary zone.
func (e *Engine) selectDeployZoneAndGPU(ctx context.Context, availResult map[string]any, plan deployPlan, supported []string, quant, userMsg, userZone string) (zone, gpuType, gpuNote, fallbackNote string, err error) {
	if strings.TrimSpace(userZone) != "" {
		return e.selectDeployZoneAndGPUInZones(ctx, availResult, plan, supported, quant, userMsg, strings.TrimSpace(userZone), []string{strings.TrimSpace(userZone)}, true)
	}
	candidates := e.deployCandidateZones(ctx, availResult)
	return e.selectDeployZoneAndGPUInZones(ctx, availResult, plan, supported, quant, userMsg, "", candidates, false)
}

func (e *Engine) selectDeployZoneAndGPUInZones(ctx context.Context, availResult map[string]any, plan deployPlan, supported []string, quant, userMsg, userZone string, candidates []string, strictUserZone bool) (zone, gpuType, gpuNote, fallbackNote string, err error) {
	primary := candidates[0]

	// A GPU the user explicitly named is honored STRICTLY (same contract as a
	// user-named zone): deploy THAT card or surface an actionable error — never
	// silently auto-size to a different one. Auto-sizing below only runs when the
	// user did not pin a card.
	if userGPU := extractDeployGPUFromCatalog(userMsg, availResult); userGPU != "" {
		return e.selectPinnedGPUZone(ctx, availResult, plan, supported, candidates, userZone, userGPU)
	}

	var firstZone, firstGPU, firstNote string // first sizeable zone, for stock-unconfirmed fall-through
	primarySoldOut := false
	for i, z := range candidates {
		cards := knowledge.ParseAvailableGPUs(availResult, z)
		if len(cards) == 0 {
			continue // no live cards offered in this zone
		}
		gt, note := knowledge.RecommendGPUTypeLive(plan.ModelName, quant, userMsg, supported, cards)
		if gt == "" {
			continue
		}
		if firstZone == "" {
			firstZone, firstGPU, firstNote = z, gt, note
		}
		switch e.zoneStockState(ctx, z, gt, plan.ImageID) {
		case zoneInStock:
			// Only claim a "sold-out fallback" when the primary was CONFIRMED
			// sold out (zoneSoldOut). If we reached a non-primary zone because the
			// primary was merely unconfirmable (zoneUnknown), don't emit a note that
			// implies the primary was checked and rejected — that would mislead.
			fb := ""
			if z != primary && primarySoldOut {
				fb = fmt.Sprintf("%s 暂时售罄，已自动切换到 %s 创建。", e.zoneDisplayName(ctx, primary), e.zoneDisplayName(ctx, z))
			}
			return z, gt, note, fb, nil
		case zoneSoldOut:
			if i == 0 {
				primarySoldOut = true
			}
		}
	}

	if strictUserZone {
		return "", "", "", "", deployUserError{msg: fmt.Sprintf("你指定的可用区 %s 当前没有可承载该工作负载且有货的机型，可换一个有货的可用区或换一个机型再试。", e.zoneDisplayName(ctx, strings.TrimSpace(userZone)))}
	}
	if firstZone != "" {
		// Sizeable but no zone confirmed in stock → proceed on the preferred zone;
		// the saga's capacity gate will halt gracefully if it is genuinely sold out.
		return firstZone, firstGPU, firstNote, "", nil
	}
	// Availability query failed/empty → static-table sizing on the primary zone.
	gt, note := knowledge.RecommendGPUTypeLive(plan.ModelName, quant, userMsg, supported, nil)
	return primary, gt, note, "", nil
}

func deployCandidateZones(availResult map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range anySlice(availResult["AvailableInstanceTypes"]) {
		mt, _ := item.(map[string]any)
		if mt == nil {
			continue
		}
		if status, _ := mt["Status"].(string); status != "" && !strings.EqualFold(status, "Normal") {
			continue
		}
		zone, _ := mt["Zone"].(string)
		if zone == "" {
			zone = deployPreferredZones[0]
		}
		if seen[zone] {
			continue
		}
		seen[zone] = true
		out = append(out, zone)
	}
	if len(out) == 0 {
		return deployPreferredZones
	}
	return out
}

func (e *Engine) deployCandidateZones(ctx context.Context, availResult map[string]any) []string {
	candidates := deployCandidateZones(availResult)
	list, err := e.supportZoneList(ctx)
	if err != nil || len(list) == 0 {
		return candidates
	}
	return sortDeployCandidateZonesBySupportZones(candidates, list)
}

func sortDeployCandidateZonesBySupportZones(candidates []string, support []zones.ZoneInfo) []string {
	if len(candidates) == 0 || len(support) == 0 {
		return candidates
	}
	order := make(map[string]int, len(support))
	for i, z := range support {
		if z.Zone != "" {
			order[strings.ToLower(z.Zone)] = i
		}
	}
	out := append([]string(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		oi, okI := order[strings.ToLower(out[i])]
		oj, okJ := order[strings.ToLower(out[j])]
		switch {
		case okI && okJ:
			return oi < oj
		case okI:
			return true
		case okJ:
			return false
		default:
			return false
		}
	})
	return out
}

func deployGuidedGPUCandidates(plan deployPlan, supported []string, availResult map[string]any, sizingModelName, quant, userMsg string) ([]string, map[string]string) {
	selected := strings.TrimSpace(plan.GpuType)
	if selected == "" {
		return nil, nil
	}
	seen := map[string]bool{strings.ToLower(selected): true}
	candidates := []string{selected}
	cards := knowledge.ParseAvailableGPUs(availResult, plan.ChosenZone)
	for _, alt := range knowledge.FittingGPUAlternatives(sizingModelName, quant, supported, cards, selected, 2) {
		if alt.Name == "" || seen[strings.ToLower(alt.Name)] {
			continue
		}
		seen[strings.ToLower(alt.Name)] = true
		candidates = append(candidates, alt.Name)
	}

	stockText := "将继续校验库存"
	if plan.StockConfirmed {
		stockText = "当前库存可满足"
	}
	reasons := map[string]string{}
	switch {
	case plan.ImageSource == "community" && plan.MatchKind == "exact" && plan.ImageName != "":
		reasons[selected] = fmt.Sprintf("现成 %s 镜像 · %s", plan.ImageName, stockText)
	case plan.MatchKind == "base" && strings.TrimSpace(plan.ModelName) != "":
		reasons[selected] = fmt.Sprintf("可承载 %s 的基础环境 · %s", plan.ModelName, stockText)
	default:
		reasons[selected] = "推荐配置 · " + stockText
	}
	return candidates, reasons
}

// selectPinnedGPUZone honors a GPU the user explicitly named. It threads that exact
// card through the SAME per-zone stock selection as the auto path, but never sizes a
// different one:
//   - image↔GPU incompatible → hard stop with a pin-specific message (the auto
//     path's gate assumes a VRAM shortfall, the wrong explanation for an explicit pin);
//   - offered with real stock in a candidate zone → create there (prefer the primary;
//     fall back on a sold-out primary exactly like the auto path);
//   - offered but stock unconfirmable (e.g. no image id) → proceed on the first
//     offering zone and let the saga's capacity gate decide;
//   - live catalog usable but the named card is sold out / not offered in every
//     candidate zone → grounded deployUserError (never a silent substitution);
//   - live catalog empty/unusable → can't disprove the pin, so honor it on the
//     primary zone and let the saga's capacity gate have the final word.
func (e *Engine) selectPinnedGPUZone(ctx context.Context, availResult map[string]any, plan deployPlan, supported, candidates []string, userZone, userGPU string) (zone, gpuType, gpuNote, fallbackNote string, err error) {
	primary := candidates[0]
	note := fmt.Sprintf("按你指定的机型 %s 部署", userGPU)

	if !gpuImageCompatible(userGPU, supported) {
		img := plan.ImageName
		if img == "" {
			img = "所选平台镜像"
		}
		return "", "", "", "", deployUserError{msg: fmt.Sprintf(
			"你指定的机型 %s 不在镜像「%s」支持的机型（%s）内，请换一个机型或换一个镜像再试。",
			userGPU, img, strings.Join(supported, "、"))}
	}

	var firstZone string // first zone offering the card with UNCONFIRMED stock
	primarySoldOut := false
	confirmedSoldOut := false // the card is offered but ResourceEnough=false somewhere
	sawAnyCard := false       // any candidate zone returned any live cards
	for i, z := range candidates {
		cards := knowledge.ParseAvailableGPUs(availResult, z)
		if len(cards) > 0 {
			sawAnyCard = true
		}
		if !availableContainsGPU(cards, userGPU) {
			continue // the named card is not offered (sellable) in this zone
		}
		switch e.zoneStockState(ctx, z, userGPU, plan.ImageID) {
		case zoneInStock:
			fb := ""
			if z != primary && primarySoldOut {
				fb = fmt.Sprintf("%s 暂时售罄，已自动切换到 %s 创建。", primary, z)
			}
			return z, userGPU, note, fb, nil
		case zoneSoldOut:
			confirmedSoldOut = true
			if i == 0 {
				primarySoldOut = true
			}
		case zoneUnknown:
			if firstZone == "" {
				firstZone = z
			}
		}
	}

	if firstZone != "" {
		return firstZone, userGPU, note, "", nil
	}
	if confirmedSoldOut {
		return "", "", "", "", deployUserError{msg: fmt.Sprintf(
			"你指定的机型 %s 当前暂无可用配比（可能已售罄），请稍后再试，或换一个机型。", userGPU)}
	}
	if !sawAnyCard {
		// Live catalog empty/unusable → degrade to the primary zone + the named card.
		return primary, userGPU, note, "", nil
	}
	where := ""
	if strings.TrimSpace(userZone) != "" {
		where = "在可用区 " + strings.TrimSpace(userZone) + " "
	}
	return "", "", "", "", deployUserError{msg: fmt.Sprintf(
		"你指定的机型 %s 当前%s不在可部署机型内。当前可部署的机型：%s。可换一个机型再试。",
		userGPU, where, availableGPUNames(availResult, candidates))}
}

// availableContainsGPU reports whether the named card is among a zone's live,
// sellable cards (case-insensitive, matching the catalog's GpuType key form).
func availableContainsGPU(cards []knowledge.AvailableGPU, gpu string) bool {
	for _, g := range cards {
		if strings.EqualFold(g.Name, gpu) {
			return true
		}
	}
	return false
}

// availableGPUNames lists the distinct, currently-sellable GPU names across the
// given candidate zones, for the "your card isn't available; these are" grounded
// error. Order-preserving across zones; deduped.
func availableGPUNames(availResult map[string]any, zones []string) string {
	seen := map[string]bool{}
	var names []string
	for _, z := range zones {
		for _, g := range knowledge.ParseAvailableGPUs(availResult, z) {
			if seen[g.Name] {
				continue
			}
			seen[g.Name] = true
			names = append(names, g.Name)
		}
	}
	if len(names) == 0 {
		return "（暂无可用机型）"
	}
	return strings.Join(names, "、")
}

// deployZoneAliases is a small deterministic floor for common legacy mentions.
// Full zone ids and display names, including newly added zones, are resolved from
// the live support-zone catalog in resolveRequestedZone.
var deployZoneAliases = []struct {
	keys []string
	zone string
}{
	{[]string{"cn-sh2-02", "cn-sh2", "上海", "sh2"}, "cn-sh2-02"},
	{[]string{"cn-wlcb-01", "cn-wlcb", "乌兰察布", "wlcb"}, "cn-wlcb-01"},
}

// extractDeployZone returns the create-zone the user explicitly named in the
// request, or "" if none. Deterministic (Rule 5: code answers a structured signal
// — zone ids/aliases are exact tokens, no LLM needed). A non-empty result is
// honored strictly downstream (error rather than silent move if unsatisfiable).
func extractDeployZone(userMsg string) string {
	lower := strings.ToLower(userMsg)
	for _, a := range deployZoneAliases {
		for _, k := range a.keys {
			if strings.Contains(lower, strings.ToLower(k)) {
				return a.zone
			}
		}
	}
	return ""
}

// resolveRequestedZone resolves the availability zone the user named, matching the
// live support-zone catalog so a Chinese display name ("华北一C") maps to its
// zone id (cn-bj2-03) — the upstream catalog carries that mapping but the agent
// previously had no way to read it, so "华北一C" was silently dropped to the
// platform default. Returns:
//   - zone != "" : a confident match → honored strictly downstream.
//   - clarify != "": the mention was partial/ambiguous/unsupported ("华北一区" →
//     "是华北一C吗？") — the caller stops and asks instead of guessing a default.
//   - both "" : no zone referenced → existing default-zone behavior.
//
// Shared by both instance-create entry points — the deploy_model saga (here) and
// the ReAct CreateInstanceWorkflow tool (engine.applyCreateZoneResolution) — so a
// user-named zone resolves identically regardless of which path the turn took.
//
// It is strictly additive over the deterministic alias floor (extractDeployZone):
// when the live catalog is unavailable (CLI/no tenant identity) or the model
// declines, it degrades to that floor, never worse than before.
func (e *Engine) resolveRequestedZone(ctx context.Context, userMsg string) (zone, clarify string) {
	aliasZone := extractDeployZone(userMsg)
	list, err := e.supportZoneList(ctx)
	if err != nil || len(list) == 0 {
		return aliasZone, "" // degrade to the deterministic alias floor
	}
	// Unambiguous literal (zone id or full display name) → no LLM needed.
	if z, ok := zones.ExactZone(list, userMsg); ok {
		return z, ""
	}
	// No zone-ish mention at all → keep the alias floor (e.g. a bare city alias).
	if !zones.Mentions(userMsg) {
		return aliasZone, ""
	}
	// A zone mention with no exact literal → LLM judgment (partial/ambiguous).
	switch d := e.matchZoneLLM(ctx, userMsg, list); d.Kind {
	case "exact":
		return d.Zone, ""
	case "clarify":
		return "", d.Clarify
	default:
		return aliasZone, ""
	}
}

func (e *Engine) supportZoneList(ctx context.Context) ([]zones.ZoneInfo, error) {
	if e.externalExecutor == nil {
		return nil, nil
	}
	cat := e.zoneCatalog
	if cat == nil {
		cat = zones.Default()
	}
	u, _ := tools.UserFrom(ctx)
	return cat.Get(ctx, e.externalExecutor, u.TopOrganizationID, u.OrganizationID)
}

// matchZoneLLM asks the TierAgent model to match a fuzzy zone mention against the
// live zone list, returning a structured decision (exact / clarify / none).
// Mirrors extractDeploySearch: small focused prompt, JSON out, hallucinated
// zones rejected by zones.ParseDecision against the live list.
func (e *Engine) matchZoneLLM(ctx context.Context, userMsg string, list []zones.ZoneInfo) zones.Decision {
	client := e.agentLLMClient
	if client == nil {
		client = e.llmClient
	}
	if client == nil {
		return zones.Decision{Kind: "none"}
	}
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: zones.MatchSystemPrompt(list)},
		{Role: openai.ChatMessageRoleUser, Content: "用户消息：" + strings.TrimSpace(userMsg)},
	}})
	if err != nil || resp == nil {
		return zones.Decision{Kind: "none"}
	}
	e.emitTokenUsage(resp.Usage)
	return zones.ParseDecision(extractJSONObject(resp.Content), list,
		func(s string, v any) error { return json.Unmarshal([]byte(s), v) })
}

// zoneDescribeMap returns a zone-id → display-name map ("cn-bj2-03"→"华北一C")
// from the live catalog, used to label the create confirm form's zone options
// with the names the console shows. Empty when the catalog is unavailable — the
// form then falls back to bare zone ids (current behavior).
func (e *Engine) zoneDescribeMap(ctx context.Context) map[string]string {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(list))
	for _, z := range list {
		if z.Describe != "" {
			m[z.Zone] = z.Describe
		}
	}
	return m
}

func (e *Engine) zoneDisplayName(ctx context.Context, zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return ""
	}
	list, err := e.supportZoneList(ctx)
	if err == nil {
		if d := zones.DescribeFor(list, zone); d != "" {
			return d
		}
	}
	return zone
}

func (e *Engine) zoneIDMap(ctx context.Context) map[string]uint32 {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]uint32, len(list))
	for _, z := range list {
		if z.Zone != "" && z.ZoneID != 0 {
			m[z.Zone] = z.ZoneID
		}
	}
	return m
}

func (e *Engine) zoneRegionIDMap(ctx context.Context) map[string]uint32 {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]uint32, len(list))
	for _, z := range list {
		if z.Zone != "" && z.RegionID != 0 {
			m[z.Zone] = z.RegionID
		}
	}
	return m
}

func (e *Engine) zoneIDFor(ctx context.Context, zone string) uint32 {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return 0
	}
	for z, id := range e.zoneIDMap(ctx) {
		if strings.EqualFold(z, zone) {
			return id
		}
	}
	return 0
}

func (e *Engine) zoneIsPodMap(ctx context.Context) map[string]bool {
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]bool, len(list))
	for _, z := range list {
		if z.Zone != "" {
			m[z.Zone] = z.IsPod
		}
	}
	return m
}

func (e *Engine) zoneIsPod(ctx context.Context, zone string) (bool, bool) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return false, false
	}
	list, err := e.supportZoneList(ctx)
	if err != nil {
		return false, false
	}
	for _, z := range list {
		if strings.EqualFold(z.Zone, zone) {
			return z.IsPod, true
		}
	}
	return false, false
}

// applyCreateZoneResolution resolves a user-named zone for the ReAct
// CreateInstanceWorkflow path, mutating args in place. It overrides args["Zone"]
// with the resolved zone id (e.g. "华北一C" → cn-bj2-03) and injects
// args["ZoneDescribes"] (zone-id → 显示名) so the confirm form labels each zone
// with the console's Chinese name. It returns a non-empty clarify question when
// the zone mention is partial/ambiguous ("华北一区" → "是华北一C吗？") so the
// caller stops before creating; otherwise "". Reuses the deploy saga's
// resolveRequestedZone so both create entry points behave identically. Degrades
// to no-op (LLM Zone untouched, no ZoneDescribes) when the live catalog is
// unavailable — e.g. on the CLI path with no tenant identity.
func (e *Engine) applyCreateZoneResolution(ctx context.Context, args map[string]any) (clarify string) {
	userZone, clarify := e.resolveRequestedZone(ctx, e.lastUserMsg)
	if clarify != "" {
		return clarify
	}
	if userZone != "" {
		args["Zone"] = userZone
		args["GuidedZoneLocked"] = true
	}
	targetZone, _ := args["Zone"].(string)
	if isPod, ok := e.zoneIsPod(ctx, targetZone); ok {
		args["ZoneIsPod"] = isPod
	}
	if descMap := e.zoneDescribeMap(ctx); len(descMap) > 0 {
		args["ZoneDescribes"] = descMap
	}
	if idMap := e.zoneIDMap(ctx); len(idMap) > 0 {
		args["ZoneIds"] = idMap
	}
	if regionIDMap := e.zoneRegionIDMap(ctx); len(regionIDMap) > 0 {
		args["ZoneRegionIds"] = regionIDMap
	}
	if podMap := e.zoneIsPodMap(ctx); len(podMap) > 0 {
		args["ZoneIsPods"] = podMap
	}
	return ""
}

// deployGPUAliases maps a GPU the user names in the request to its canonical
// CreateInstance GpuType (the gpuSpecs / catalog key). Each pattern is
// boundary-anchored — (?:^|[^0-9a-z]) … (?:[^0-9a-z]|$) — so a card token only
// matches when it is a standalone word, NOT a digit-run inside a model name
// ("Llama100" must NOT match A100; "4090" inside "4090Pro" must NOT match the bare
// 4090). CJK characters count as non-[0-9a-z], so "用A100部署" matches A100 while a
// model like "Qwen2.5-72B" matches nothing. More specific variants (4090_48G /
// 4090Pro / 5090D) precede the bare token so an equal-start tie resolves to the
// specific card. "V100" canonicalizes to the only V100-class card the platform
// sells, V100S (same as knowledge.CanonicalGPUType).
//
// STOP-GROW: keep this to the cards the platform actually offers (gpuSpecs keys).
// A broader table belongs in config, not a hand-grown literal.
var deployGPUAliases = []struct {
	pattern *regexp.Regexp
	gpu     string
}{
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])4090[\s_-]*48g(?:[^0-9a-z]|$)`), "4090_48G"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])4090\s*pro(?:[^0-9a-z]|$)`), "4090Pro"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])4090(?:[^0-9a-z]|$)`), "4090"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])5090d(?:[^0-9a-z]|$)`), "5090D"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])5090(?:[^0-9a-z]|$)`), "5090"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])3090(?:[^0-9a-z]|$)`), "3090"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])3080\s*ti(?:[^0-9a-z]|$)`), "3080Ti"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])2080\s*ti(?:[^0-9a-z]|$)`), "2080Ti"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])2080(?:[^0-9a-z]|$)`), "2080"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])v100s?(?:[^0-9a-z]|$)`), "V100S"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])a100(?:[^0-9a-z]|$)`), "A100"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])a800(?:[^0-9a-z]|$)`), "A800"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])h20(?:[^0-9a-z]|$)`), "H20"},
	{regexp.MustCompile(`(?i)(?:^|[^0-9a-z])p40(?:[^0-9a-z]|$)`), "P40"},
}

// extractDeployGPU returns the canonical GpuType the user explicitly named in the
// request, or "" if none. Deterministic (Rule 5: code answers a structured signal —
// GPU names are exact tokens; using the LLM here would be non-deterministic and is
// unnecessary). When a card is named, it is honored STRICTLY downstream (the same
// contract as extractDeployZone): deploy THAT card or surface an actionable error,
// never silently auto-size to a different one. When two cards are named, the one
// appearing FIRST in the text wins (so "A100 或 4090" pins A100); equal-start ties
// resolve to the more specific token via the alias-table order.
func extractDeployGPU(userMsg string) string {
	best, bestStart := "", -1
	for _, a := range deployGPUAliases {
		loc := a.pattern.FindStringIndex(userMsg)
		if loc == nil {
			continue
		}
		if bestStart == -1 || loc[0] < bestStart {
			best, bestStart = a.gpu, loc[0]
		}
	}
	return best
}

func extractDeployGPUFromCatalog(userMsg string, availResult map[string]any) string {
	if gpu := extractGPUNameFromText(userMsg, gpuNamesFromAvailability(availResult)); gpu != "" {
		return gpu
	}
	return extractDeployGPU(userMsg)
}

func gpuNamesFromAvailability(availResult map[string]any) []string {
	if availResult == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, item := range anySlice(availResult["AvailableInstanceTypes"]) {
		mt, _ := item.(map[string]any)
		if mt == nil {
			continue
		}
		name, _ := mt["Name"].(string)
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		names = append(names, name)
	}
	return names
}

func extractGPUNameFromText(userMsg string, gpuNames []string) string {
	if strings.TrimSpace(userMsg) == "" || len(gpuNames) == 0 {
		return ""
	}
	type match struct {
		name  string
		start int
		size  int
	}
	var matches []match
	for _, name := range gpuNames {
		token := normalizeGPUNameForMatch(name)
		if token == "" {
			continue
		}
		pattern := gpuNameMatchPattern(token)
		loc := pattern.FindStringIndex(userMsg)
		if loc == nil {
			continue
		}
		matches = append(matches, match{name: name, start: loc[0], size: len([]rune(token))})
	}
	if len(matches) == 0 {
		return ""
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		if matches[i].size != matches[j].size {
			return matches[i].size > matches[j].size
		}
		return matches[i].name < matches[j].name
	})
	return matches[0].name
}

func gpuNameMatchPattern(token string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?i)(?:^|[^0-9a-z])`)
	for i, r := range token {
		if i > 0 {
			b.WriteString(`[\s_-]*`)
		}
		b.WriteString(regexp.QuoteMeta(string(r)))
	}
	b.WriteString(`(?:[^0-9a-z]|$)`)
	return regexp.MustCompile(b.String())
}

func normalizeGPUNameForMatch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type zoneStock int

const (
	zoneUnknown zoneStock = iota // could not determine (no image id / API error / no matching spec)
	zoneInStock                  // single-card config confirmed available
	zoneSoldOut                  // single-card config present but ResourceEnough=false
)

// zoneStockState checks whether gpuType's single-card config has real stock in a
// zone, the same gate the saga's stepCheckCapacity uses (Specs[].{Gpu==1,
// ResourceEnough}). It needs the resolved CompShareImageId (capacity is image-
// scoped); without one it returns zoneUnknown so the caller falls back to the
// preferred zone rather than skipping it. Read-only (works in read-only mode too).
func (e *Engine) zoneStockState(ctx context.Context, zone, gpuType, imageID string) zoneStock {
	if imageID == "" || gpuType == "" {
		return zoneUnknown
	}
	capArgs := deployment.BuildCapacityArgs(deployment.DeploymentDraft{
		Zone:             zone,
		GPUType:          gpuType,
		CompShareImageID: imageID,
	})
	// Non-default zones reject a Zone without its Region (RetCode=230); add it so
	// the per-zone stock probe works in cn-bj2-03 / cn-sh2-02, not just the default.
	if r := workflow.RegionFromZone(zone); r != "" {
		capArgs["Region"] = r
	}
	if id := e.zoneIDFor(ctx, zone); id != 0 {
		capArgs["zone_id"] = id
	}
	res := e.querySafeRead(ctx, "CheckCompShareResourceCapacity", capArgs)
	if res == nil {
		return zoneUnknown
	}
	specs, _ := res["Specs"].([]any)
	sawSingleCard := false
	for _, s := range specs {
		m, _ := s.(map[string]any)
		if m == nil {
			continue
		}
		if g, _ := m["Gpu"].(float64); g != 1 {
			continue
		}
		sawSingleCard = true
		if enough, _ := m["ResourceEnough"].(bool); enough {
			return zoneInStock
		}
	}
	if sawSingleCard {
		return zoneSoldOut
	}
	return zoneUnknown
}

// extractDeploySearch asks the model for ONE short keyword to drive the community
// FuzzySearch (the lead's Q1: understand an imprecise request → a searchable term,
// e.g. "我想跑个数字人" → "数字人"). Best-effort: any error or unparseable / empty
// result yields "" and the caller falls back to an unfiltered community sample, so
// a flaky extraction never blocks the deploy. Uses the same TierAgent client.
func (e *Engine) extractDeploySearch(ctx context.Context, client LLMClient, userMsg string) string {
	resp, err := client.Chat(ctx, llm.ChatRequest{Messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "你是优云智算部署助手的检索词提取器。用户想部署或运行某个模型/应用。请从需求中提取一个最适合用于社区镜像库模糊搜索的简短关键词（应用名/模型名/任务类型，如 \"数字人\"、\"ComfyUI\"、\"Qwen\"、\"视频生成\"、\"语音克隆\"）。只输出一个 JSON 对象：{\"search\":\"关键词\"}，无法确定时输出 {\"search\":\"\"}，不要任何额外文字。"},
		{Role: openai.ChatMessageRoleUser, Content: "用户需求：" + strings.TrimSpace(userMsg)},
	}})
	if err != nil || resp == nil {
		return ""
	}
	e.emitTokenUsage(resp.Usage)
	var out struct {
		Search string `json:"search"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(resp.Content)), &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Search)
}

// queryCommunityCandidates fetches community images filtered by the extracted
// keyword (FuzzySearch matches name+author). When the keyword is empty or finds
// nothing, it falls back to an unfiltered sample so the matcher still sees options.
func (e *Engine) queryCommunityCandidates(ctx context.Context, searches ...string) map[string]any {
	merged := map[string]any{"CompshareImageGroup": []any{}}
	for _, search := range searches {
		search = strings.TrimSpace(search)
		if search == "" {
			continue
		}
		res := e.querySafeRead(ctx, "DescribeCommunityImages",
			map[string]any{"Limit": 30, "ExcludeReadme": true, "FuzzySearch": search})
		mergeCommunityGroups(merged, res)
	}
	if len(communityGroupNames(merged)) > 0 {
		return merged
	}
	return e.querySafeRead(ctx, "DescribeCommunityImages",
		map[string]any{"Limit": 30, "ExcludeReadme": true})
}

func mergeCommunityGroups(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	dstGroups, _ := dst["CompshareImageGroup"].([]any)
	seen := map[string]bool{}
	for _, item := range dstGroups {
		if m, _ := item.(map[string]any); m != nil {
			name, _ := m["ImageName"].(string)
			seen[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for _, item := range anySlice(src["CompshareImageGroup"]) {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["ImageName"].(string)
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		dstGroups = append(dstGroups, item)
	}
	dst["CompshareImageGroup"] = dstGroups
}

func deployCommunitySearchTerms(userMsg, llmSearch string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		key := strings.ToLower(s)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	add(llmSearch)
	if sized := extractDeploySizedModelName(userMsg); sized != "" && !isBareDeploySize(sized) {
		add(sized)
		add(strings.ReplaceAll(sized, "-", " "))
		add(strings.ReplaceAll(sized, "-", ":"))
		if family, size := splitDeployModelSize(sized); family != "" && size != "" {
			add(family)
			add(family + ":" + strings.ToLower(size))
			add(family + " " + size)
		}
	}
	return out
}

func exactCommunityModelImageName(modelName, userMsg string, community map[string]any) (string, bool) {
	keys := exactDeployModelKeys(modelName, userMsg)
	if len(keys) == 0 {
		return "", false
	}
	for _, item := range anySlice(community["CompshareImageGroup"]) {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["ImageName"].(string)
		desc, _ := m["ImageDesc"].(string)
		hay := normalizeDeployModelIdentity(name + " " + desc)
		for _, key := range keys {
			if key != "" && strings.Contains(hay, key) {
				return name, true
			}
		}
	}
	return "", false
}

func exactDeployModelKeys(modelName, userMsg string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || !deployTextHasModelFamilyAndSize(s) {
			return
		}
		key := normalizeDeployModelIdentity(s)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	add(modelName)
	add(extractDeploySizedModelName(userMsg))
	return out
}

func deployTextHasModelFamilyAndSize(s string) bool {
	hasSize := regexp.MustCompile(`(?i)\d+(?:\.\d+)?b`).MatchString(s)
	hasLetter := regexp.MustCompile(`(?i)[a-z]`).MatchString(s)
	return hasSize && hasLetter && !isBareDeploySize(strings.TrimSpace(s))
}

func normalizeDeployModelIdentity(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func splitDeployModelSize(s string) (family, size string) {
	re := regexp.MustCompile(`(?i)(\d+(?:\.\d+)?b)`)
	loc := re.FindStringIndex(s)
	if loc == nil {
		return "", ""
	}
	family = strings.Trim(strings.TrimSpace(s[:loc[0]]), "-_: ")
	size = strings.TrimSpace(s[loc[0]:loc[1]])
	return family, size
}

func anySlice(v any) []any {
	arr, _ := v.([]any)
	return arr
}

// chosenImage returns BOTH the CompShareImageId AND the SupportedGpuTypes of the
// image the matcher picked, looked up ONCE from the catalog the matcher itself
// queried (so the id and the GPU constraint reference the SAME image). The id is
// threaded to the saga (params["CompShareImageId"]) so the saga creates exactly
// this image rather than re-resolving — otherwise the saga's independent re-query
// (platform: Limit:20 + CJK-broken Name filter → imageSet[0] fallback; community:
// index-0 of a FuzzySearch=ImageName query) can build a DIFFERENT image than the
// one the GPU was sized against. An empty id ("" — name not found, or community
// group without Data[]) means "let the saga resolve it", preserving the saga's own
// fallback + the community fail-loud guard. SupportedGpuTypes is deduped, may be
// empty (then RecommendGPUTypeWithin applies no constraint).
func chosenImage(plan deployPlan, platform, community map[string]any) (imageID string, supportedGPUs []string) {
	if plan.ImageName == "" {
		return "", nil
	}
	if plan.ImageSource == "community" {
		groups, _ := community["CompshareImageGroup"].([]any)
		for _, item := range groups {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if name, _ := m["ImageName"].(string); strings.EqualFold(name, plan.ImageName) {
				data, ok := m["Data"].([]any)
				if !ok || len(data) == 0 {
					return "", nil
				}
				d0, _ := data[0].(map[string]any)
				id, _ := d0["CompShareImageId"].(string)
				return id, stringSliceFromAny(d0["SupportedGpuTypes"])
			}
		}
		return "", nil
	}
	set, _ := platform["ImageSet"].([]any)
	for _, item := range set {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if name, _ := m["Name"].(string); strings.EqualFold(name, plan.ImageName) {
			id, _ := m["CompShareImageId"].(string)
			return id, stringSliceFromAny(m["SupportedGpuTypes"])
		}
	}
	return "", nil
}

// stringSliceFromAny converts a JSON-decoded []any of strings to []string,
// skipping non-string and duplicate entries (the live SupportedGpuTypes contains
// duplicates, e.g. "V100S" twice).
func stringSliceFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]bool, len(arr))
	var out []string
	for _, x := range arr {
		s, ok := x.(string)
		if !ok || s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// querySafeRead runs a read-only tool through the safe executor
// (OriginWorkflowInternal = no per-call confirm / registry churn) and returns the
// raw result map, or nil on error (matching degrades gracefully — the matcher still
// has the other source + the user message + the static-table GPU fallback).
func (e *Engine) querySafeRead(ctx context.Context, action string, args map[string]any) map[string]any {
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

// captureStepResult wraps (never replaces) a named step's CheckResult so the handler
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
// instance Password / FileBrowserPassword (base64 secrets on the describe
// response); SshLoginCommand (which embeds the IP + port) is the SSH access info
// we surface. The usage block (访问地址 + 使用说明) tells the user HOW to use what
// was deployed — an SSH command alone doesn't say "ComfyUI is on :8188".
func buildDeployReply(plan deployPlan, uHostId string, host map[string]any, state string, usage imageUsage) string {
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
	if zone := deployPlanZoneDisplay(plan); zone != "" {
		b.WriteString(fmt.Sprintf("- 可用区：%s\n", zone))
	}
	if name := stringFromHost(host, "Name"); name != "" {
		b.WriteString(fmt.Sprintf("- 名称：%s\n", name))
	}
	if ssh := stringFromHost(host, "SshLoginCommand"); ssh != "" {
		b.WriteString(fmt.Sprintf("- SSH 登录：%s\n", ssh))
	}

	writeUsageGuidance(&b, host, usage)

	// Jump to the console's instance-list page to manage the new instance (state /
	// login / billing). Static URL — no per-instance id needed.
	b.WriteString("\n🔗 管理实例（状态 / 登录信息 / 计费）：" + deployConsoleInstancesURL + "\n")

	// base match: no ready-made image for the named model → tell the user the base
	// was deployed and how to self-host, so they're not left thinking the wrong
	// model was baked in (the real-session bug: a same-size sibling silently shipped).
	if hint := deploySelfDeployHint(plan); hint != "" {
		b.WriteString("\nℹ️ " + hint + "\n")
	}

	if plan.FallbackNote != "" {
		b.WriteString("\nℹ️ " + plan.FallbackNote + "\n")
	}
	if state != "Running" && !isTerminalFailState(state) {
		b.WriteString("\n你可以稍后用「查询我的实例」查看最新状态和登录信息。\n")
	}
	if plan.MatchNote != "" {
		b.WriteString("\n（选型说明：" + plan.MatchNote + "）")
	}
	return b.String()
}

// buildAdviseReply renders the recommendation: which GPU + image the handler would
// deploy, and how to proceed. It NEVER creates anything — it turns "跑X用哪个卡 /
// 帮我搭个能跑Y的环境 / 推荐我用哪种卡部署" into a useful answer. Deterministic render of
// the resolved deployPlan (the matcher already did the LLM judgment + live
// sizing/stock). The footer depends on whether writes are enabled:
//   - mutating OFF (shipped read-only default): tell the user to ask an admin to
//     enable writes — the handler cannot create.
//   - mutating ON (advice-only request, e.g. "推荐哪种卡"): the recommendation is
//     ready and the handler COULD create, so it offers to proceed on a concrete
//     restate ("部署 <image>") instead of silently entering the create saga.
//
// Secret boundary: every field rendered here (GpuType / ImageName / ChosenZone /
// MatchNote / FallbackNote) is derived from API metadata or constructed from zone
// ids + status strings — none carries a secret. Do NOT thread instance-level
// secrets (Password / FileBrowserPassword / Jupyter token) through deployPlan into
// this reply.
func buildAdviseReply(plan deployPlan, mutatingEnabled bool) string {
	var b strings.Builder
	b.WriteString("根据你的需求，建议如下配置：\n")
	if plan.GpuType != "" {
		b.WriteString(fmt.Sprintf("- 推荐 GPU：%s\n", plan.GpuType))
	}
	if plan.ImageName != "" {
		b.WriteString(fmt.Sprintf("- 推荐镜像：%s（%s）\n", plan.ImageName, sourceLabel(plan.ImageSource)))
	}
	if zone := deployPlanZoneDisplay(plan); zone != "" {
		b.WriteString(fmt.Sprintf("- 可用区：%s\n", zone))
	}
	if plan.MatchNote != "" {
		b.WriteString("- 选型说明：" + plan.MatchNote + "\n")
	}
	if plan.FallbackNote != "" {
		b.WriteString("- " + plan.FallbackNote + "\n")
	}
	if hint := deploySelfDeployHint(plan); hint != "" {
		b.WriteString("- " + hint + "\n")
	}
	if !mutatingEnabled {
		b.WriteString("\n助手当前为只读模式，未自动为你创建实例。如需我直接部署，请联系管理员开启写操作权限后再说一次。")
		return b.String()
	}
	// Writes are on: this is an advice-only request (a recommendation/how-to), so we
	// deliberately did NOT auto-create. Offer to proceed on an explicit restate.
	if plan.ImageName != "" {
		b.WriteString(fmt.Sprintf("\n以上为推荐配置，尚未为你创建实例。确认后回复「部署 %s」我就开始创建（也可指定机型/可用区，如「部署 %s 用 A100」）。", plan.ImageName, plan.ImageName))
	} else {
		b.WriteString("\n以上为推荐配置，尚未为你创建实例。确认后回复「帮我部署」我就开始创建。")
	}
	return b.String()
}

func deployPlanZoneDisplay(plan deployPlan) string {
	if strings.TrimSpace(plan.ZoneLabel) != "" {
		return strings.TrimSpace(plan.ZoneLabel)
	}
	return strings.TrimSpace(plan.ChosenZone)
}

// deploySelfDeployHint returns the "no ready-made image → here's how to self-host"
// note for a BASE match: the matcher judged that no ready-made image exists for the
// user's named model and deployed a framework base instead (match_kind="base"), so
// the user must pull/load the model themselves. Returns "" for an exact match (a
// ready-made model/app image needs no self-pull) or when the user named no model.
//
// It deliberately does NOT fabricate an exact model tag — a maintained model→tag
// table is unrealistic (the same reason the matching itself is an LLM judgment, not
// a lookup). It names the model the user asked for and the base that was deployed,
// then points at where to find the real tag. The framework line is chosen from the
// deployed image's OWN name (ollama / vllm / …), so it renders the already-made
// decision rather than making a second matching decision.
func deploySelfDeployHint(plan deployPlan) string {
	if plan.MatchKind != "base" || strings.TrimSpace(plan.ModelName) == "" {
		return ""
	}
	img := strings.ToLower(plan.ImageName)
	var how string
	switch {
	case strings.Contains(img, "ollama"):
		how = "登录后用 Ollama 自行拉取：`ollama pull <模型标签>`（到 ollama.com/library 查“" + plan.ModelName + "”的准确标签）"
	case strings.Contains(img, "vllm"), strings.Contains(img, "sglang"), strings.Contains(img, "lmdeploy"), strings.Contains(img, "tensorrt"):
		how = "登录后从 HuggingFace / ModelScope 下载“" + plan.ModelName + "”的权重，再用镜像内的推理框架加载启动"
	default:
		how = "登录后自行拉取“" + plan.ModelName + "”的权重（HuggingFace / ModelScope / Ollama）并用对应框架加载"
	}
	return fmt.Sprintf("平台暂无与「%s」完全匹配的现成镜像，已为你部署可承载它的框架底座「%s」。%s。", plan.ModelName, plan.ImageName, how)
}

// writeUsageGuidance appends the "how to use it" section: the app→endpoint map
// (constructed from the image's SoftwarePorts + the running instance's public IP,
// NOT from the instance's Softwares URLs — those can embed a Jupyter ?token=,
// which is treated as a secret), the auto-start hint,
// and a rune-sanitized excerpt of the (untrusted) community author's Readme. Each
// piece is emitted only when its data is present, so a base OS image adds nothing.
func writeUsageGuidance(b *strings.Builder, host map[string]any, usage imageUsage) {
	ip := hostPublicIP(host)
	hasJupyter := false
	if len(usage.ports) > 0 {
		b.WriteString("- 访问地址：\n")
		for _, p := range usage.ports {
			if strings.Contains(strings.ToLower(p.name), "jupyter") {
				hasJupyter = true
			}
			switch {
			case ip != "":
				b.WriteString(fmt.Sprintf("    · %s：http://%s:%d\n", p.name, ip, p.port))
			default:
				b.WriteString(fmt.Sprintf("    · %s：端口 %d（实例就绪后用 http://<公网IP>:%d 访问）\n", p.name, p.port, p.port))
			}
		}
		if hasJupyter {
			b.WriteString("    （JupyterLab 等需要访问令牌的服务，令牌请在控制台获取，不要在此处明文传播。）\n")
		}
	}
	// Extra open TCP ports not already covered by an app mapping (e.g. vLLM's
	// OpenAI-compatible API on :8000, SGLang on :30000 — the real service ports).
	if extra := extraFirewallPorts(usage); len(extra) > 0 {
		b.WriteString(fmt.Sprintf("- 额外开放端口：%s（应用 API/服务端口，可用 http://<公网IP>:<端口> 访问）\n", joinInts(extra)))
	}
	if usage.autoStart {
		b.WriteString("- 镜像服务已配置自启动，实例进入 Running 后稍候即可直接访问上面的地址。\n")
	}
	if ex := plainTextExcerpt(usage.readme, deployReadmeExcerptRunes); ex != "" {
		b.WriteString("\n📖 使用说明（社区镜像作者提供，节选，请自行甄别）：\n")
		b.WriteString(ex)
		b.WriteString("\n完整使用说明见控制台「镜像详情」页。\n")
	}
}

// deployStopReply renders a saga that stopped before success (capacity / price /
// confirm / create). The saga already put a human message in Result.Message.
func deployStopReply(r *workflow.Result) string {
	// An unresolved confirm (timeout / disconnect / user typed instead of
	// clicking the card) reaches here as "用户取消了操作" too — narrate it
	// honestly as not-executed, never as a false "已取消创建实例".
	if r.Message == "用户取消了操作" {
		return "好的，本次创建未执行。如需继续，请重新发送指令并确认。"
	}
	if deployment.ClassifyCreateFailure(r.Message).Kind == deployment.FailureImageZoneNotAdapted {
		return "创建未完成：所选镜像在当前可用区暂未适配。请更换镜像，或选择其他可用区后重试。"
	}
	if r.Message != "" {
		return "创建未完成：" + r.Message
	}
	if r.StoppedAt != "" {
		return fmt.Sprintf("创建在「%s」步骤中止。", r.StoppedAt)
	}
	return "创建未完成。"
}

// deployStopReplyWithAlternatives wraps deployStopReply. When the saga halted on a
// real stock shortage (the recommended card's specific spec is sold out), it
// re-queries availability and appends the image-compatible cards that still fit the
// model and are currently offered — so the user gets concrete next options instead
// of a bare "换一个规格". The user can then reply "用 5090" and the pinned-GPU path
// (selectPinnedGPUZone) deploys that card. Type-level availability is advisory (the
// create call is the only ground truth), which the note states. Falls back to the
// plain reply when nothing fits or the model size is unknown.
func (e *Engine) deployStopReplyWithAlternatives(ctx context.Context, r *workflow.Result, plan deployPlan) string {
	reply := deployStopReply(r)
	if !isDeployStockShortage(r) {
		return reply
	}
	avail := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
	cards := knowledge.ParseAvailableGPUs(avail, plan.ChosenZone)
	if note := deployAlternativesNote(plan, cards); note != "" {
		return reply + "\n" + note
	}
	return reply
}

// isDeployStockShortage reports whether the saga stopped because the chosen spec is
// sold out. The capacity gate's message (create_instance.go: "...当前库存不足（售罄）...")
// is the signal; matching its stable "库存不足" substring keeps this decoupled from
// the gate's exact wording while still firing only on the stock case.
func isDeployStockShortage(r *workflow.Result) bool {
	return r != nil && strings.Contains(r.Message, "库存不足")
}

// deployAlternativesNote builds the "these cards still fit and are offered" line
// from already-parsed availability. Pure (no ctx/API) so it is unit-testable; the
// query lives in deployStopReplyWithAlternatives. Returns "" when no compatible,
// VRAM-sufficient alternative is offered (caller keeps the bare reply).
func deployAlternativesNote(plan deployPlan, cards []knowledge.AvailableGPU) string {
	alts := knowledge.FittingGPUAlternatives(plan.ModelName, plan.Quantization, plan.SupportedGPUs, cards, plan.GpuType, 3)
	if len(alts) == 0 {
		return ""
	}
	names := make([]string, 0, len(alts))
	for _, a := range alts {
		names = append(names, fmt.Sprintf("%s(%dGB)", a.Name, a.VRAMGB))
	}
	// LLM deploys name the model ("够跑 Qwen2.5-7B"); app/image deploys (ComfyUI /
	// SD-WebUI / 数字人) have no model name, so the line stays image-scoped.
	head := "当前镜像支持的可用机型还有"
	if model := strings.TrimSpace(plan.ModelName); model != "" {
		head = "当前镜像支持、且够跑 " + model + " 的机型还有"
	}
	return fmt.Sprintf("%s：%s。回复「用 %s」我就帮你换上重建（实际是否有货以创建结果为准）。",
		head, strings.Join(names, " / "), alts[0].Name)
}

// ── small pure helpers ──

func sourceLabel(source string) string {
	if source == "community" {
		return "社区镜像"
	}
	return "平台镜像"
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
	sys.WriteString("下面提供两个来源的现成镜像（均已预装环境，无需手动安装）：\n")
	sys.WriteString("- 平台镜像(platform)：由优云官方维护。既有框架/系统底座(PyTorch、CUDA、Ubuntu)，也有打包好的应用镜像(如 ComfyUI、vLLM、Ollama、SGLang)。\n")
	sys.WriteString("- 社区镜像(community)：由社区作者发布，多为面向具体应用/模型/工作流打包好的开箱即用镜像(如数字人、视频生成、TTS、特定工作流)。\n\n")
	sys.WriteString("注意：两个来源都可能同时含有框架底座和应用镜像，不要假设“平台只有框架、社区只有应用”。请只依据下面候选清单中每个镜像的真实名称(Name)、框架(Framework)与描述(Description)来判断。\n")
	sys.WriteString("选型规则（按顺序判断，并据此填写 match_kind）：\n")
	sys.WriteString("1. 用户点名了某个模型时：只有当某镜像的名称/描述指向同一个模型(同系列且同变体)，才算它的现成镜像。⚠️ 同品牌或同参数量都不等于同一个模型——DeepSeek-R1-32B、QwQ-32B、Janus-Pro 都是 32B 或同公司，但属于不同模型，绝不能互相顶替；拿不准就当作“没有现成镜像”。\n")
	sys.WriteString("2. 有该模型的现成镜像 → 选它，match_kind 填 exact。\n")
	sys.WriteString("3. 没有该模型的现成镜像 → 选一个能承载它的框架底座(部署 LLM 用带 vLLM/Ollama/SGLang/PyTorch 的镜像)，match_kind 填 base；这是完全正常的方案，用户登录后自行拉取模型，绝不要硬塞一个名字相近的别的模型来冒充。\n")
	sys.WriteString("4. 纯应用类需求(数字人/视频生成/TTS/某工作流) → 按应用名匹配对应镜像，match_kind 填 exact。\n")
	sys.WriteString("只能选候选清单里真实存在的镜像名，不要编造。\n")
	sys.WriteString("严格只输出一个 JSON 对象，不要任何额外文字：\n")
	sys.WriteString(`{"image_source":"platform|community","image_name":"候选清单中的镜像名","model_name":"用户要运行的模型全称或留空","match_kind":"exact|base","size_ambiguous":true,"quantization":"留空或 fp16/int8/int4"}` + "\n")
	sys.WriteString("model_name 用于按显存推荐 GPU、并在参数规模不明时先向用户追问，按以下规则填写：\n")
	sys.WriteString("- 用户点名了要跑的模型(如 Qwen、Llama3、DeepSeek-R1、Qwen2.5-32B)就填该模型名；即使你选的是 Ollama 或社区应用镜像，也照填用户说的那个模型名，不要省略。\n")
	sys.WriteString("- 严格按用户原话填：用户没给参数规模就别自己补(别把 Llama3 写成 Llama3-8B、别把 Qwen 写成 Qwen2.5-72B)；用户给了就带上(如 Qwen2.5-32B)。\n")
	sys.WriteString("- 仅当用户没点名任何具体模型、纯应用类需求(数字人/视频生成/TTS/某工作流)时才留空。\n")
	sys.WriteString("size_ambiguous：仅当用户点名的模型是一个有多个参数规模的系列、且没说要哪个(如 DeepSeek-R1 / Qwen 没带 7B/32B)时填 true；具体的单一模型(如 Fish Audio S2-Pro、Janus-Pro)、已带规模、或纯应用需求都填 false。")

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

// ── post-create usage guidance (B8.5: tell the user HOW to use the instance) ──

// imageUsage is the chosen image's usage guidance, fetched read-only AFTER a
// successful create. ports = app→port (the access endpoints); firewall = extra
// open TCP ports; autoStart = services come up on their own; readme = the
// community author's rich-text guide (platform Readme is always empty — verified
// 2026-05-31, so only community populates it).
type imageUsage struct {
	ports     []softwarePort
	firewall  []int
	autoStart bool
	readme    string
}

// softwarePort is one app↔port mapping from an image's SoftwarePorts.
type softwarePort struct {
	name string
	port int
}

// fetchImageUsage reads the usage guidance for the deployed image so the reply can
// describe EXACTLY the created image. It is source-aware (community carries Readme,
// ExcludeReadme MUST be off here; the matcher's shortlist query sets
// ExcludeReadme=true for token thrift, but this post-create read wants the Readme).
// Best-effort: empty id or any read error yields an empty imageUsage and the reply
// simply omits the usage block.
//
// Source asymmetry (verified live 2026-06-05): DescribeCompShareImages honors the
// CompShareImageId filter (returns the one image), but DescribeCommunityImages
// IGNORES it in production — a keyed community query returns the default popular
// list, not the keyed image (a DeepSeek-R1 deploy got an LTX-2.3 video image's
// Readme this way). So community is queried by FuzzySearch on the resolved image
// name (the matcher already proved that name resolves to this group) and the exact
// id is strict-matched across the returned groups; the id filter is used only for
// platform, where it works.
func (e *Engine) fetchImageUsage(ctx context.Context, plan deployPlan) imageUsage {
	if plan.ImageID == "" {
		return imageUsage{}
	}
	if plan.ImageSource == "community" {
		res := e.querySafeRead(ctx, "DescribeCommunityImages", map[string]any{"FuzzySearch": plan.ImageName, "Limit": 30})
		return communityImageUsage(res, plan.ImageID)
	}
	res := e.querySafeRead(ctx, "DescribeCompShareImages", map[string]any{"CompShareImageId": plan.ImageID})
	return platformImageUsage(res, plan.ImageID)
}

// platformImageUsage extracts usage from a DescribeCompShareImages response,
// returning the entry whose CompShareImageId matches (or empty when absent — the
// keyed platform query returns exactly that image, verified live 2026-06-05).
func platformImageUsage(result map[string]any, imageID string) imageUsage {
	if result == nil {
		return imageUsage{}
	}
	set, _ := result["ImageSet"].([]any)
	m := pickByImageID(set, imageID)
	return imageUsageFromImage(m)
}

// communityImageUsage extracts usage from a DescribeCommunityImages response by
// scanning CompshareImageGroup[].Data[] for the EXACT CompShareImageId. It returns
// empty (the reply omits the usage block) when the id is absent — it must NEVER
// substitute an arbitrary image's Readme. Because DescribeCommunityImages ignores
// the CompShareImageId filter in production, fetchImageUsage queries by FuzzySearch
// (which returns the target group plus possibly others); this strict scan picks the
// created image out of that result, so a query that fails to include it surfaces no
// usage block rather than a wrong one.
func communityImageUsage(result map[string]any, imageID string) imageUsage {
	if result == nil {
		return imageUsage{}
	}
	groups, _ := result["CompshareImageGroup"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		if gm == nil {
			continue
		}
		data, _ := gm["Data"].([]any)
		if m := pickByImageID(data, imageID); m != nil {
			return imageUsageFromImage(m)
		}
	}
	return imageUsage{}
}

// pickByImageID returns the entry in items whose CompShareImageId == imageID, or
// nil when none matches (or imageID is "", or items is empty). It deliberately does
// NOT fall back to the first entry: a post-create usage block must describe EXACTLY
// the created image, and a first-entry fallback once surfaced a completely unrelated
// image's Readme (an LTX-2.3 video image shown for a DeepSeek-R1 deploy) when the
// upstream query returned a default list instead of the keyed image. Omitting the
// block is correct when the exact image is absent; never substitute an arbitrary one.
func pickByImageID(items []any, imageID string) map[string]any {
	if imageID == "" {
		return nil
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if id, _ := m["CompShareImageId"].(string); id == imageID {
			return m
		}
	}
	return nil
}

// imageUsageFromImage projects one image map (CompShareImage shape) into imageUsage.
func imageUsageFromImage(m map[string]any) imageUsage {
	if m == nil {
		return imageUsage{}
	}
	auto, _ := m["AutoStart"].(bool)
	readme, _ := m["Readme"].(string)
	return imageUsage{
		ports:     parseSoftwarePorts(m["SoftwarePorts"]),
		firewall:  parseFirewallPorts(m["FirewallPorts"]),
		autoStart: auto,
		readme:    readme,
	}
}

// parseSoftwarePorts converts SoftwarePorts ([]{Software,Port}) to []softwarePort,
// skipping entries without a usable port. Port arrives as a JSON float64.
func parseSoftwarePorts(v any) []softwarePort {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []softwarePort
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		port := intFromAny(m["Port"])
		if port <= 0 {
			continue
		}
		name, _ := m["Software"].(string)
		if name == "" {
			name = "服务"
		}
		out = append(out, softwarePort{name: name, port: port})
	}
	return out
}

// parseFirewallPorts converts FirewallPorts ([]number) to []int.
func parseFirewallPorts(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []int
	for _, it := range arr {
		if p := intFromAny(it); p > 0 {
			out = append(out, p)
		}
	}
	return out
}

// intFromAny coerces a JSON-decoded number (float64) or int to int; 0 otherwise.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// extraFirewallPorts returns the firewall ports not already listed as an app
// port (those are shown under 访问地址), deduped and order-preserving.
func extraFirewallPorts(usage imageUsage) []int {
	seen := make(map[int]bool, len(usage.ports))
	for _, p := range usage.ports {
		seen[p.port] = true
	}
	var out []int
	for _, fp := range usage.firewall {
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, fp)
	}
	return out
}

// joinInts renders ints as a comma-separated string.
func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ", ")
}

// hostPublicIP returns the instance's public-facing IP from UHostSet[].IPSet,
// preferring a non-Private entry with the highest Weight (the current 出口IP).
// Falls back to any non-empty IP. Empty when none is assigned yet (provisioning).
func hostPublicIP(host map[string]any) string {
	if host == nil {
		return ""
	}
	ips, _ := host["IPSet"].([]any)
	best, bestWeight, fallback := "", -1, ""
	for _, it := range ips {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		ip, _ := m["IP"].(string)
		if ip == "" {
			continue
		}
		if fallback == "" {
			fallback = ip
		}
		if t, _ := m["Type"].(string); t == "Private" {
			continue
		}
		w := intFromAny(m["Weight"])
		if w > bestWeight {
			best, bestWeight = ip, w
		}
	}
	if best != "" {
		return best
	}
	return fallback
}

var (
	mdImageRe      = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`) // markdown image: ![alt](url)
	htmlTagRe      = regexp.MustCompile(`(?s)<[^>]+>`)          // any HTML tag incl. <iframe ...>
	multiNewlineRe = regexp.MustCompile(`\n{3,}`)
	multiSpaceRe   = regexp.MustCompile(` {2,}`)
)

// plainTextExcerpt turns a markdown+HTML Readme into a compact plain-text excerpt
// for the CLI/chat reply: drop image embeds + HTML tags (iframes/imgs are terminal
// noise), then rune-sanitize and collapse whitespace before truncating to maxRunes.
//
// The Readme is UNTRUSTED community-author content shown in a terminal, so the
// rune pass drops control chars (ANSI ESC sequences, bell, VT/FF/CR) and Unicode
// format/bidi chars (U+202E & friends can spoof link direction; zero-width chars
// hide text) — only '\n' survives as structure — and folds every other Unicode
// whitespace (tab, NBSP, …) to a plain space. It does NOT redact secrets: the
// Readme is the author's own public content, and OUR secrets (Password /
// FileBrowserPassword / Jupyter token) never flow into it. The excerpt IS placed
// in the reply, which becomes an assistant turn in history, so a later-turn LLM
// can see it — acceptable because it is public content, capped, attributed
// ("请自行甄别"), and not used for routing; the rune pass + cap bound the blast
// radius. Returns "" for empty/whitespace-only input.
func plainTextExcerpt(s string, maxRunes int) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = mdImageRe.ReplaceAllString(s, "")
	s = htmlTagRe.ReplaceAllString(s, "")

	var clean strings.Builder
	clean.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			clean.WriteRune('\n')
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			// drop: ESC/bell/VT/FF/CR + bidi overrides/isolates + zero-width (Cf)
		case unicode.IsSpace(r):
			clean.WriteRune(' ')
		default:
			clean.WriteRune(r)
		}
	}
	s = clean.String()

	// Collapse intra-line space runs + trim line ends, then collapse blank runs.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(multiSpaceRe.ReplaceAllString(ln, " "), " ")
	}
	s = strings.Join(lines, "\n")
	s = multiNewlineRe.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	return truncateRunes(s, maxRunes)
}
