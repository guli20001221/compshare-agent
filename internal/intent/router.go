package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/compshare-agent/internal/boundarypacks"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/llm"
)

type OutputMode string

const (
	OutputModeJSONSchema       OutputMode = "json_schema"
	OutputModeJSONObject       OutputMode = "json_object"
	OutputModeStrictPromptJSON OutputMode = "strict_prompt_json"
)

type IntentRouterLLM interface {
	CompleteIntentPlan(ctx context.Context, req IntentRouterLLMRequest) (string, error)
}

type IntentRouterLLMWithUsage interface {
	CompleteIntentPlanWithUsage(ctx context.Context, req IntentRouterLLMRequest) (IntentRouterLLMResponse, error)
}

type IntentRouterLLMResponse struct {
	Content string
	Usage   llm.TokenUsage
}

type IntentRouterLLMRequest struct {
	Mode           OutputMode
	SystemPrompt   string
	UserPrompt     string
	ResponseSchema json.RawMessage
}

type IntentRouterOptions struct {
	BaseURL          string
	Model            string
	MaxRetries       int
	LookupCapability func(baseURL, model string) llm.Capability
	UnifiedCreate    bool
}

type IntentRouterInput struct {
	UserText     string
	ImageContext string
	LastIntent   string
	// PriorText is both the validator's `source:prior_turn` haystack and a bounded
	// recent-conversation block in the router prompt. The bound is reapplied by
	// buildUserPrompt so callers cannot reintroduce the historic unbounded prompt
	// growth that broke schema reliability.
	PriorText string
	// LastSelectedInstanceID surfaces SessionState.SelectedInstanceID so the
	// intent router can resolve "那台机" / "它" cross-turn references without
	// seeing the full transcript.
	LastSelectedInstanceID string
	// LastAssistantSnippet is the prefix of the most recent assistant reply
	// (capped at ~200 chars) used as a low-token topic continuity hint.
	LastAssistantSnippet string
	Resolver             EntityResolver
	// Deprecated: use Resolver so production shadow mode can pass immutable
	// registry snapshots without exposing EntityRegistry internals.
	Registry *entity.EntityRegistry
}

type IntentRouterResult struct {
	Plan               IntentRoute
	Mode               OutputMode
	Attempts           int
	Fallback           bool
	LastValidationCode ErrorCode
	// LastValidationField is the schema path the final attempt failed on
	// (e.g. "slots.target_refs[0].value"). Together with LastValidationCode it
	// answers "why did this turn fall back", which route_status=fallback_invalid
	// alone never could.
	LastValidationField string
	// LastRejectedIntent is the intent the model chose on the final FAILING
	// attempt. The engine's trace projection overwrites a failed plan's intent with
	// `unknown`, so without capturing it here a validator rejection of a perfectly
	// correct route is indistinguishable from a genuinely off-platform question.
	LastRejectedIntent Intent
	Usage              llm.TokenUsage
}

type IntentRouter struct {
	llm              IntentRouterLLM
	baseURL          string
	model            string
	maxRetries       int
	lookupCapability func(baseURL, model string) llm.Capability
	unifiedCreate    bool
}

// NewIntentRouter constructs an IntentRouter from an intent-router LLM client
// and options. It applies the default capability lookup (llm.LookupCapability)
// when none is supplied and a minimum of one retry. This is the canonical
// constructor.
func NewIntentRouter(client IntentRouterLLM, opts IntentRouterOptions) *IntentRouter {
	lookup := opts.LookupCapability
	if lookup == nil {
		lookup = llm.LookupCapability
	}
	maxRetries := opts.MaxRetries
	if maxRetries == 0 {
		maxRetries = 1
	}
	return &IntentRouter{
		llm:              client,
		baseURL:          opts.BaseURL,
		model:            opts.Model,
		maxRetries:       maxRetries,
		lookupCapability: lookup,
		unifiedCreate:    opts.UnifiedCreate,
	}
}

func SelectOutputMode(cap llm.Capability) OutputMode {
	// json_schema is preferred whenever the model supports it. The earlier
	// `&& !cap.IsThinkingMode` guard was over-conservative: a 2026-06-23 live
	// probe confirmed ds-v4-flash (a thinking model) enforces json_schema
	// (enum/const constrained decoding) in thinking mode, so excluding thinking
	// models needlessly fell them back to the weaker json_object.
	if cap.SupportsJSONSchema {
		return OutputModeJSONSchema
	}
	if cap.SupportsJSONObject {
		return OutputModeJSONObject
	}
	return OutputModeStrictPromptJSON
}

func (p *IntentRouter) Plan(ctx context.Context, input IntentRouterInput) (IntentRouterResult, error) {
	mode := SelectOutputMode(p.lookupCapability(p.baseURL, p.model))
	result := IntentRouterResult{
		Plan:     unknownFallbackPlan(),
		Mode:     mode,
		Fallback: true,
	}
	if p.llm == nil {
		return result, fmt.Errorf("intent router LLM is nil")
	}

	systemPrompt := buildSystemPromptWithUnifiedCreate(p.unifiedCreate)
	responseSchema := IntentRouteResponseSchemaForIntents(routerRuntimeIntents(p.unifiedCreate))
	userPrompt := buildUserPrompt(input, "")
	attempts := p.maxRetries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		result.Attempts = attempt
		raw, usage, err := p.completeIntentPlan(ctx, IntentRouterLLMRequest{
			Mode:           mode,
			SystemPrompt:   systemPrompt,
			UserPrompt:     userPrompt,
			ResponseSchema: responseSchema,
		})
		if err != nil {
			return result, err
		}
		result.Usage = addTokenUsage(result.Usage, usage)

		var validationErr *ValidationError
		plan, parseErr := parsePlanJSON(raw)
		if parseErr == nil {
			plan = normalizePlannerControlFields(plan)
			err = ValidateRoute(plan, ValidationContext{
				UserText:  input.UserText,
				PriorText: input.PriorText,
				Resolver:  input.entityResolver(),
				Registry:  input.Registry,
			})
			if err == nil {
				plan = withDerivedSelectedSkills(plan)
				return IntentRouterResult{
					Plan:     plan,
					Mode:     mode,
					Attempts: attempt,
					Usage:    result.Usage,
				}, nil
			}
			if errorAsValidation(err, &validationErr) {
				result.LastValidationCode = validationErr.Code
				result.LastValidationField = validationErr.Field
				// Keep the route the model actually chose. Everything downstream is
				// about to call this turn `unknown`; without this we could never tell
				// a validator rejection of a CORRECT intent from a real off-platform
				// question, and those need opposite fixes.
				result.LastRejectedIntent = plan.Intent
			}
		} else {
			// A parse failure is a different disease from a validation rejection:
			// unparseable JSON means the model's schema compliance broke down (the
			// 2026-05-28 avalanche failure mode), while a validation rejection means
			// the model emitted a well-formed plan we then refused. Both land in
			// fallback_invalid, so the bucket is useless unless they are labelled
			// apart. Fallback is already true here, so this only adds a label.
			result.LastValidationCode = ErrUnparseableJSON
			result.LastValidationField = ""
			result.LastRejectedIntent = ""
		}

		userPrompt = buildUserPrompt(input, buildRetryInstruction(parseErr, validationErr))
	}
	return result, nil
}

func buildRetryInstruction(parseErr error, validationErr *ValidationError) string {
	if validationErr != nil {
		field := validationErr.Field
		if field == "" {
			field = "<unknown>"
		}
		return fmt.Sprintf("上一轮 IntentPlan 校验失败：code=%s field=%s。修复：%s。只返回一个符合 schema v1.0 的 JSON 对象，不要解释。",
			validationErr.Code, field, repairInstructionForValidationCode(validationErr.Code))
	}
	if parseErr != nil {
		return "上一轮输出不是合法 IntentPlan JSON。只返回一个符合 schema v1.0 的 JSON 对象，不要 Markdown、代码块或解释。"
	}
	return "上一轮输出未通过校验。只返回一个符合 schema v1.0 的 JSON 对象。"
}

func repairInstructionForValidationCode(code ErrorCode) string {
	switch code {
	case ErrInvalidSchemaVersion:
		return `schema_version 必须是 "1.0"`
	case ErrInvalidIntent:
		return "intent 必须使用允许的运行时意图枚举"
	case ErrInvalidConfidence:
		return "confidence 必须在 0 到 1 之间"
	case ErrInvalidTargetRefType:
		return "slots.target_refs 必须使用允许的目标类型和值"
	case ErrAttemptedHallucinatedEntity:
		return "目标引用必须来自用户原文或上一轮文本，source_span 必须能在对应文本中找到"
	case ErrEntityNotFound:
		return "不要引用账号内不存在的实例；找不到目标时使用空 target_refs 或请求澄清"
	case ErrNameTooShort:
		return "name target_ref 至少 2 个字符"
	case ErrInvalidMetric:
		return "slots.metrics 只能使用 cpu、memory、gpu、vram"
	case ErrInvalidTimeWindow:
		return "slots.time_window.type 必须是 preset、relative 或 absolute"
	default:
		return "按失败字段修正，保持其它字段最小且有效"
	}
}

func (p *IntentRouter) completeIntentPlan(ctx context.Context, req IntentRouterLLMRequest) (string, llm.TokenUsage, error) {
	if withUsage, ok := p.llm.(IntentRouterLLMWithUsage); ok {
		resp, err := withUsage.CompleteIntentPlanWithUsage(ctx, req)
		return resp.Content, resp.Usage, err
	}
	raw, err := p.llm.CompleteIntentPlan(ctx, req)
	return raw, llm.TokenUsage{}, err
}

func addTokenUsage(left, right llm.TokenUsage) llm.TokenUsage {
	left.PromptTokens += right.PromptTokens
	left.CompletionTokens += right.CompletionTokens
	left.TotalTokens += right.TotalTokens
	return left
}

func (input IntentRouterInput) entityResolver() EntityResolver {
	if input.Resolver != nil {
		return input.Resolver
	}
	if input.Registry != nil {
		return input.Registry
	}
	return nil
}

func parsePlanJSON(raw string) (IntentRoute, error) {
	var plan IntentRoute
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return IntentRoute{}, err
	}
	return plan, nil
}

func normalizePlannerControlFields(plan IntentRoute) IntentRoute {
	plan.RequiredTools = nil
	plan.Retrieval = Retrieval{}
	plan.HardBlockHint = false
	return plan
}

type routerPromptExample struct {
	Question string
	PlanJSON string
	Source   string
}

type routerPromptExampleGroup struct {
	Intent   Intent
	Source   string
	Examples []routerPromptExample
	// compact renders the group as a shared plan JSON + question list instead
	// of repeating the full JSON per example. Use for groups where all examples
	// share the same output structure (e.g. knowledge_qa: empty slots). Saves
	// prompt tokens for the 21-example knowledge_qa group.
	compact bool
}

func routerPromptExampleGroups() []routerPromptExampleGroup {
	return []routerPromptExampleGroup{
		{
			Intent: IntentResourceInfo,
			Source: "Phase 1 baseline resource inventory routing",
			Examples: []routerPromptExample{
				{
					Question: "show resource info for my-test-agent",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"name","value":"my-test-agent","source":"user_text","source_span":"my-test-agent"}],"metrics":[],"time_window":null},"confidence":0.82}`,
					Source:   "Phase 1 baseline: named instance resource lookup",
				},
				{
					Question: "which machines are running",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"state=running"}],"metrics":[],"time_window":null},"confidence":0.82}`,
					Source:   "Phase 1 baseline: state filter",
				},
				{
					Question: "which 4090 machines are stopped",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"state=stopped"},{"type":"filter","value":"gpu_type=4090"}],"metrics":[],"time_window":null},"confidence":0.82}`,
					Source:   "Phase 1 baseline: compound inventory filters",
				},
				{
					Question: "我账号下有哪些 4090 实例",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"gpu_type=4090"}],"metrics":[],"time_window":null},"confidence":0.82}`,
					Source:   "Phase 1 baseline: account inventory filter",
				},
				{
					Question: "我有哪些实例",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}`,
					Source:   "PR2.5 Chinese anchor: bare inventory question (no filter)",
				},
				{
					Question: "列出我的机器",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}`,
					Source:   "PR2.5 Chinese anchor: alternate inventory phrasing",
				},
				{
					Question: "正在运行的实例",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"state=running"}],"metrics":[],"time_window":null},"confidence":0.85}`,
					Source:   "PR2.5 Chinese anchor: running state filter",
				},
				{
					Question: "我有几台机器",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.85}`,
					Source:   "PR2.5 Chinese anchor: count question (still inventory)",
				},
			},
		},
		// IntentDiskInfo migrated to internal/intent/planner_examples/disk_info.md.
		diskPlannerExampleGroups[IntentDiskInfo],
		{
			Intent: IntentUnknown,
			Source: "Phase 1 demo boundary: unsupported non-platform requests",
			Examples: []routerPromptExample{
				{
					Question: "今天北京天气怎么样",
					PlanJSON: `{"schema_version":"1.0","intent":"unknown","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.7}`,
					Source:   "Phase 1 boundary: unsupported general knowledge",
				},
				{
					Question: "帮我写一首和平台无关的诗",
					PlanJSON: `{"schema_version":"1.0","intent":"unknown","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.7}`,
					Source:   "Phase 1 boundary: unrelated creative request",
				},
			},
		},
		{
			Intent: IntentMonitorQuery,
			Source: "Phase 1 baseline monitor routing",
			Examples: []routerPromptExample{
				{
					Question: "show current CPU and GPU monitor for my-test-agent",
					PlanJSON: `{"schema_version":"1.0","intent":"monitor_query","slots":{"target_refs":[{"type":"name","value":"my-test-agent","source":"user_text","source_span":"my-test-agent"}],"metrics":["cpu","gpu"],"time_window":{"type":"preset","value":"now"}},"confidence":0.82}`,
					Source:   "Phase 1 baseline: current monitor query",
				},
				{
					Question: "CPU is high, what should I do",
					PlanJSON: `{"schema_version":"1.0","intent":"monitor_query","slots":{"target_refs":[],"metrics":["cpu"],"time_window":{"type":"preset","value":"now"}},"confidence":0.82}`,
					Source:   "Phase 1 baseline: performance symptom maps to current monitor",
				},
			},
		},
		// IntentKnowledgeQA migrated to internal/intent/planner_examples/knowledge_qa.md
		// in C5 Phase B (PR #6, 2026-05-22). Same byte-equal contract as the
		// Phase A diagnosis migration — see TestPlannerExamples_KnowledgeQADisk
		// LoaderEqualsLegacy + the SHA hash in TestPlannerExamples_FullSystem
		// PromptStable. Editorial review of knowledge_qa anchors now happens
		// in the markdown file; router.go retains structural code only.
		diskPlannerExampleGroups[IntentKnowledgeQA],
		{
			Intent: IntentBillingAccountUnsupported,
			Source: "PR #52 finance process vs personal-status hard-block split",
			Examples: []routerPromptExample{
				{
					Question: "account balance",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_account_unsupported","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.9}`,
					Source:   "PR #52: personal realtime account data hard block",
				},
				{
					Question: "what is my invoice status",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_account_unsupported","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.9}`,
					Source:   "PR #52: personal invoice status hard block",
				},
			},
		},
		{
			// Personal billing complaint with vague cause — N=5 jitter check on
			// 2026-05-20 showed planner randomly routed "充值 10 块就被扣完了 我啥
			// 也没干啊" to billing_account_unsupported (3/5) or knowledge_qa
			// (2/5); both wrong (account_unsupported is hard-block, knowledge_qa
			// has no chunks for personal complaints). billing_instance is the
			// correct route — the existing system-prompt directive at line ~410
			// routes instance-scoped billing questions to billing_instance,
			// but planner had no one-shot example anchoring
			// the colloquial personal-complaint phrasing. Trace evidence:
			// F:/compshare-agent-runs/q04-jitter-20260520-165129.
			Intent: IntentBillingInstance,
			Source: "Stable routing for personal billing complaints with vague cause (2026-05-20 N=5 jitter check)",
			Examples: []routerPromptExample{
				{
					Question: "充值 10 块就被扣完了 我啥也没干啊",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.82}`,
					Source:   "Colloquial personal billing complaint — diagnose own instances",
				},
				{
					Question: "我账单怎么这么高",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.8}`,
					Source:   "Personal billing complaint — high bill diagnostic",
				},
			},
		},
		// IntentOperationLifecycle migrated to internal/intent/planner_examples/operation_lifecycle.md.
		diskPlannerExampleGroups[IntentOperationLifecycle],
		{
			// deploy_model anchors workload-first creation. The user names a model,
			// framework, or application and wants a suitable instance created for it
			// (the engine picks the image + GPU). This is distinct from
			// create_instance, which covers hardware-first creation, and from
			// operation_lifecycle, which covers existing-instance operations.
			// target_refs stays empty: the workload name is extracted by the deploy
			// handler's matcher, not a planner slot.
			Intent: IntentDeployModel,
			Source: "workload-first create-family requests route to deploy_model",
			Examples: []routerPromptExample{
				{
					Question: "帮我部署一个 Qwen2.5-32B",
					PlanJSON: `{"schema_version":"1.0","intent":"deploy_model","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.8}`,
					Source:   "deploy model name — engine sizes GPU and picks a framework image",
				},
				{
					Question: "我想跑数字人",
					PlanJSON: `{"schema_version":"1.0","intent":"deploy_model","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.8}`,
					Source:   "run application workload — engine matches a ready-to-run image",
				},
				{
					Question: "搞一个能跑 ComfyUI 的环境",
					PlanJSON: `{"schema_version":"1.0","intent":"deploy_model","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.8}`,
					Source:   "set up framework or application environment — engine selects image",
				},
				{
					Question: "部署 Llama3-70B 做推理服务",
					PlanJSON: `{"schema_version":"1.0","intent":"deploy_model","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.8}`,
					Source:   "deploy large-model inference workload — exercises VRAM sizing path",
				},
			},
		},
		// IntentDiagnosis migrated to internal/intent/planner_examples/diagnosis.md
		// in C5 Phase A (PR #86, 2026-05-21). The disk-backed loader spliced in
		// here MUST produce byte-equal output to the prior inline literal — the
		// byte-equal test in planner_examples_test.go pins that contract.
		// Future Phase B/C/... migrate the remaining intents one per PR.
		diskPlannerExampleGroups[IntentDiagnosis],
	}
}

func routerPromptExampleGroupsWithUnifiedCreate(unified bool) []routerPromptExampleGroup {
	groups := routerPromptExampleGroups()
	if !unified {
		return groups
	}
	out := make([]routerPromptExampleGroup, 0, len(groups)+1)
	out = append(out, groups...)
	out = append(out, routerPromptExampleGroup{
		Intent: IntentCreateInstance,
		Source: "first-class hardware-create anchors",
		Examples: []routerPromptExample{
			{
				Question: "帮我搞台 4090",
				PlanJSON: `{"schema_version":"1.0","intent":"create_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.82}`,
				Source:   "hardware-first exact-GPU create routes to create_instance",
			},
			{
				Question: "帮我抢一台上海的 4090",
				PlanJSON: `{"schema_version":"1.0","intent":"create_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.82}`,
				Source:   "hardware-first create with zone preference routes to create_instance",
			},
			{
				Question: "部署一台 4090 跑 Qwen",
				PlanJSON: `{"schema_version":"1.0","intent":"create_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"confidence":0.8}`,
				Source:   "mixed hardware and workload request keeps both facets in the unified handler",
			},
		},
	})
	return out
}

func renderRouterPromptExampleGroups(groups []routerPromptExampleGroup) []string {
	lines := []string{}
	for _, group := range groups {
		lines = append(lines, fmt.Sprintf(`<examples intent="%s">`, group.Intent))
		if group.compact && len(group.Examples) > 0 {
			lines = append(lines, "<shared_plan>")
			lines = append(lines, group.Examples[0].PlanJSON)
			lines = append(lines, "</shared_plan>")
			lines = append(lines, "<questions>")
			for _, example := range group.Examples {
				lines = append(lines, fmt.Sprintf(`<question>%s</question>`, escapePromptXML(example.Question)))
			}
			lines = append(lines, "</questions>")
		} else {
			for _, example := range group.Examples {
				lines = append(lines, "<example>")
				lines = append(lines, "<user>"+escapePromptXML(example.Question)+"</user>")
				lines = append(lines, example.PlanJSON)
				lines = append(lines, "</example>")
			}
		}
		lines = append(lines, "</examples>")
	}
	return lines
}

func escapePromptXML(s string) string {
	return html.EscapeString(s)
}

func basePromptScaffold() string {
	return basePromptScaffoldWithUnifiedCreate(false)
}

func basePromptScaffoldWithUnifiedCreate(unified bool) string {
	// Keep the intent-router scaffold ASCII/English. Earlier Windows
	// console/source encoding issues made non-ASCII prompt labels fragile, while
	// the baseline model handles bilingual user text with English JSON-field
	// instructions.
	lines := []string{
		"You are the intent router for the CompShare console agent.",
		"Return exactly one JSON object. Do not output Markdown, prose, or tool calls.",
		"Required top-level fields: schema_version, intent, slots, confidence.",
		"schema_version must be \"1.0\". confidence must be a number in [0,1].",
		"Allowed intent enum: monitor_query, monitor_history, resource_info, billing_instance, billing_account_unsupported, diagnosis, vague_failure, operation_lifecycle, knowledge_qa, gpu_specs_query, stock_availability, image_list, image_tag_catalog, model_repository_browse, network_accelerator_status, refund_estimate, cfs_info, pricing_query, disk_info, deploy_model, unknown.",
		"Primary intents — all have working handlers on this platform: resource_info, monitor_query, operation_lifecycle, pricing_query, gpu_specs_query, stock_availability, network_accelerator_status, refund_estimate, cfs_info, billing_instance, knowledge_qa, diagnosis, disk_info. Prefer the closest matching primary intent over unknown whenever the question is about the CompShare platform, the user's own instances, or platform billing/pricing/usage.",
		"deploy_model: Classify execution requests that need a create confirmation flow as deploy_model. The user wants to RUN or DEPLOY a specific model, framework, or application (e.g. 部署 Qwen2.5-32B / 跑数字人 / 搭一个能跑 ComfyUI 的环境 / 部署 Llama3 做推理) and wants a suitable instance created for that workload — the agent picks the image and GPU. Use deploy_model with empty target_refs. Route recommendation, how-to, price, configuration-sizing, comparison, or feasibility questions about deployment to knowledge_qa, pricing_query, or gpu_specs_query as appropriate; do not use deploy_model. API-subscription developer tools and coding assistants used through an API key or platform package — Claude Code, Codex, Coding Plan, Cursor — are NOT deployable models (they call a hosted API; there are no weights to run on an instance); classify how-to-use/configure questions about them as knowledge_qa, not deploy_model. Use operation_lifecycle (NOT deploy_model) for operations on an EXISTING instance (start/stop/reboot/resize/add-disk).",
		"Treat performance questions like CPU high, GPU busy/idle, memory high, VRAM high, or whether a machine is idle as monitor_query first, unless the user states a concrete SSH, init, billing, lifecycle, or instance-internal operation problem.",
		"Historical monitor phrases like yesterday, last night, today morning, or X点到Y点 must use monitor_history or a non-current time_window, never preset now/today. EXCEPTION: when these phrases appear ONLY in the Screenshot summary (not in User question), they are UI labels or navigation text from a screenshot — do NOT classify as monitor_history based on screenshot content alone. Classify based on the User question.",
		"Screenshot summary is contextual evidence (what the user sees on screen), not user intent. Use it to refine diagnosis or identify the page context, but the intent classification must be driven by the User question text. Screenshot content must never be the sole trigger for hard-block intents (monitor_history, billing_account_unsupported). Screenshot content must never be used as parameter source for mutating operations (create/stop/start/reboot) — those require explicit user input or confirmation.",
		"Stage 2B retrieval focus: Classify clear platform usage / FAQ questions as knowledge_qa.",
		"Classify diagnosis questions that also reference platform FAQ or usage docs as diagnosis.",
		"Classify platform how-to, config, and error-code questions as knowledge_qa, even when phrased as a problem: remote desktop audio setup, driver installation, error code 226601, community image publishing, or BaseURL setup.",
		"Separate diagnosis from knowledge_qa by action shape, not target presence. Route 'how do I do X on the platform' to knowledge_qa. Route runtime failure reports such as cannot connect/open/access, port unreachable, SSH timeout, service not reachable, GPU not found/detected, or instance stuck/failed/init stuck to diagnosis even with target_refs:[]. When target_refs is empty, the engine will ask which instance. Default to knowledge_qa only for pure usage/config/error-code/how-to questions without a concrete instance target.",
		"Classify direct runtime/list/user price questions like 4090 多少钱, H20 按月包多少钱, 目录价多少, 标准价多少, 折后价多少, or actual purchase price as pricing_query — the route handler runs DescribeAvailableCompShareInstanceTypes plus the account/catalog price APIs deterministically. Route personal-billing complaints (我账单怎么这么高 / 充值就被扣完了) to billing_instance. Route named plan/package billing-RULE or management questions — how is the X plan/package billed, 套餐怎么计费 / 计费方式 / 按什么收费, 删除/取消/退订 Coding Plan — to knowledge_qa, not pricing_query or operation_lifecycle.",
		"Route comparison questions ('X 和 Y 哪个划算' / 'X vs Y'), yes-no feasibility questions ('X 可以 Y 吗' / 'can I X'), and procedure-description questions ('X 流程是怎样的' / 'how does X work') about platform usage, pricing rules, image, instance, or billing to knowledge_qa unless they reference a specific instance target.",
		"Route billing-specific FAQ plus instance facts to billing_instance; keep unsupported account totals on billing_account_unsupported.",
		"Route finance policy/how-to questions to knowledge_qa: invoice issuance, refund rules, arrears handling, why am I still charged after shutdown, billing mode differences, or package expiry.",
		"account realtime finance/status questions about THE USER'S OWN ACCOUNT data — balance, total bills, transaction records, charge records, payable bills, my invoice status (e.g. 我的发票开好了吗 / 我账单还剩多少), my refund progress, recharge amount on my account — emit billing_account_unsupported.",
		"Classify instance refund-estimate questions such as 这台现在释放能退多少钱 / 退订大概退多少 as refund_estimate with the target instance; this is read-only estimation, not a release operation. Classify CFS list/status/create-price/resize-price/refund-estimate questions as cfs_info; keep actual CFS create/resize on operation_lifecycle.",
		"FAQ/process questions about HOW the system works — invoice issuance schedule (什么时候开发票 / 开票周期), refund process flow, arrears policy (欠费几天回收), expiry rules — emit knowledge_qa, not billing_account_unsupported.",
		"When ambiguous between process-question and personal-status (e.g. 我的发票什么时候开 — could be either), default to knowledge_qa unless the user explicitly asks for the realtime state of a specific personal record (我的 X 开好了吗 / 寄了吗 / 进度 / 多少).",
		"Diagnostic phrasings that pair a finance topic with non-finance symptoms (e.g. 下载速度突然变慢 是欠费了吗 还是网络高峰) emit knowledge_qa — the user is asking for root-cause checklist, not their own balance amount.",
		"If a single question mixes finance FAQ with account realtime personal-status data, emit billing_account_unsupported for the whole turn.",
		"Classify instance-scoped billing questions as billing_instance, but do not promise account ledger amounts or transaction exports.",
		"Personal billing complaints with vague cause — 充值 10 块就被扣完了 / 我账单怎么这么高 / 钱怎么扣这么快 / 我啥也没干怎么就扣费了 — emit billing_instance (NOT billing_account_unsupported, which is reserved for explicit balance / total-bill / transaction-record queries; and NOT knowledge_qa, because the user wants a personal diagnostic, not a process FAQ).",
		"Route billing navigation questions to knowledge_qa: where do I find / how do I view / how to check / from which page can I see my bills, invoices, expense, balance, charges, or recharge history. They ask for a UI path, not actual finance numbers, and the docs cover the path.",
		"Classify resource operation commands on EXISTING instances as operation_lifecycle, regardless of whether the user specifies a target instance. Action verbs include 关机 / 停机 / 停了 / 启动 / 开机 / 重启 / 加盘 / 加数据盘 / 变配 / 升级配置 / 重装 / 重置密码 / 改名. Do not classify new instance creation as operation_lifecycle. Route workload-first requests naming a model/app/framework (e.g. 部署 DeepSeekR1 / 部署数字人 / 部署 ComfyUI) to deploy_model. When an existing-instance target is given, populate target_refs (UHostId, name, or filter). When the user omits the target (e.g. 帮我关机, 启动一下, 重启那台), use operation_lifecycle with target_refs:[] — the engine will list the user's instances and prompt for selection. Concrete anchors: 帮我关机 uhost-xxx, uhost-test 停了, 启动 train-gpu, 把 uhost-xxx 重启一下, 给 uhost-xxx 加 200G 数据盘. Do NOT route bare action verbs to resource_info (that intent is for listing/inspecting only) or unknown (the action is on-platform).",
		// Conversation state. The user prompt has ALWAYS carried `Last intent`,
		// `Last selected instance` and `Last assistant snippet` (buildUserPrompt), but until
		// now nothing in this prompt said what they MEAN — a grep across this scaffold, every
		// route.yaml planner_directives, the boundary packs and the planner examples found
		// zero references to them. The model was handed three bare labels and ~30 directives
		// that all key off the CURRENT message, so it classified every follow-up as if it
		// were an opening turn.
		//
		// On real 2026-06-26..07-09 traffic that cost: 8.3% of all follow-up turns (106/1280)
		// emitted `unknown` while holding a populated Last intent, and 39% of those then did
		// zero tool work. The single biggest class is a user mid-diagnosis pasting the exact
		// terminal evidence the agent asked for and being told, in effect, "I don't handle that".
		//
		// These directives are SYSTEM-prompt text: constant, cacheable, paid once per turn.
		// The recent transcript is separately bounded before it reaches the USER prompt;
		// the old unbounded form grew every turn until ds-v4-flash stopped returning
		// schema-valid JSON (PR1 hotfix Bug 2, 2026-05-28). Do not remove that bound.
		//
		// Phrasing is imperative and list-shaped on purpose: conditional prose makes
		// ds-v4-flash narrate its reasoning instead of emitting the JSON.
		//
		// INHERITANCE IS INTENT-AWARE, AND THAT IS THE WHOLE LESSON HERE.
		//
		// The first version of these directives said, simply, "inherit Last intent when the
		// turn cannot stand alone", and "classify pasted machine output as Last intent". A
		// blind A/B judge over 150 real follow-up turns showed it RESCUED 19 turns and BROKE
		// 11 — McNemar p=0.20, i.e. nothing. The breakages all had one shape:
		//
		//	「嗯嗯」                              last=knowledge_qa -> knowledge_qa -> 「当前知识库未覆盖该问题」
		//	「bash: start_app.sh: No such file」  last=knowledge_qa -> knowledge_qa -> 「当前知识库未覆盖该问题」
		//	「32」 (answering a create question)   last=deploy_model -> deploy_model -> a create card
		//
		// knowledge_qa is not an ordinary label: without durable verified prior evidence its
		// route forces SearchKnowledge and may refuse when retrieval comes back empty. Inheriting
		// it onto a turn that contains no question can therefore create a canned refusal. Same for the
		// create family, whose route ends in a confirmation card.
		//
		// And `unknown` was never the enemy. It falls through to ReAct, which carries
		// maxHistoryMessages of conversation — so for a bare 「嗯嗯」 it produces a perfectly
		// sensible reply. The metric that counted every `unknown` follow-up as amnesia was
		// measuring the wrong thing; the judge caught what the metric could not.
		//
		// So: a pasted error is diagnosis ABSOLUTELY, not "whatever was last". knowledge_qa
		// and the create family are never inherited. An acknowledgement goes to unknown on
		// purpose. Inheritance is for continuing a TROUBLESHOOT, which is what the router was
		// actually losing.
		"Conversation state. When a conversation is already in progress the user prompt carries Last intent, Last selected instance, and Last assistant snippet. They tell you the turn is a CONTINUATION, not a new standalone question. Read them before classifying.",
		"Pasted machine output is a RUNTIME FAILURE REPORT. Terminal prompts (root@host:~#), stack traces, pip/apt/conda logs, nvidia-smi output, SSH login banners, command-not-found and permission-denied lines, error text, error codes, and JSON API responses: emit diagnosis. Emit diagnosis for these NO MATTER what Last intent says — the user is showing you evidence of something that is broken, not asking a documentation question.",
		"A follow-up that continues a TROUBLESHOOT stays diagnosis. When Last intent is diagnosis, keep diagnosis for a continuing symptom, a report that the prior remedy failed, or a direct answer to the assistant's latest diagnostic question.",
		"Never INHERIT knowledge_qa. Emit knowledge_qa only when the CURRENT message is itself a platform question or a clear referential follow-up that the visible conversation can resolve. A turn carrying no question — an acknowledgement, a pasted error, a bare number — must never be sent there just because the previous turn was.",
		"Never INHERIT deploy_model or create_instance. Creating an instance needs an explicit request in the CURRENT message. A bare number or GPU name answering the assistant's question is not a new create request.",
		"A message that only acknowledges the prior reply asks nothing. Emit unknown for it so the agent can answer from the conversation itself; do not infer a new request from its wording.",
		"Override Last intent whenever the current message is a COMPLETE new request by itself — a new stock, price, inventory, lifecycle, or how-to question. Classify those on their own text.",
		"Use unknown ONLY when the question is clearly off-platform — other vendors' products, politics, weather, unrelated code, or generic creative writing — or when it is a bare acknowledgement per the rule above. When the question is on-platform but the exact intent is unclear, pick the closest primary intent (resource_info for inventory, knowledge_qa for usage/FAQ, diagnosis for problems on a specific instance) rather than unknown.",
		"slots must contain target_refs, metrics, and time_window. Use [] for missing target_refs or metrics, and null for missing time_window.",
		"For a user-written instance name, output target_refs item {\"type\":\"name\",\"value\":\"<exact name>\",\"source\":\"user_text\",\"source_span\":\"<exact substring>\"}.",
		"For a user-written UHostId, output target_refs item {\"type\":\"uhost_id_user_input\",\"value\":\"<exact id>\",\"source\":\"user_text\",\"source_span\":\"<exact substring>\"}.",
		"For resource_info inventory filters, output target_refs items with {\"type\":\"filter\",\"value\":\"state=running\"}, {\"type\":\"filter\",\"value\":\"state=stopped\"}, or {\"type\":\"filter\",\"value\":\"gpu_type=<gpu type>\"}.",
		"Resource filters are ANDed across different fields. Do not mix filter target_refs with name or UHostId target_refs.",
		"Never invent UHostIds or instance names that do not appear verbatim in the user question or prior turns.",
		"For monitor_query, metrics may be [] when the metric words are unclear; the handler can render all returned current monitor values.",
		"Examples:",
	}
	if unified {
		for i, line := range lines {
			switch {
			case strings.HasPrefix(line, "Allowed intent enum:"):
				lines[i] = "Allowed intent enum: monitor_query, monitor_history, resource_info, billing_instance, billing_account_unsupported, diagnosis, vague_failure, operation_lifecycle, knowledge_qa, gpu_specs_query, stock_availability, image_list, image_tag_catalog, model_repository_browse, network_accelerator_status, refund_estimate, cfs_info, pricing_query, disk_info, deploy_model, create_instance, unknown."
			case strings.HasPrefix(line, "deploy_model:"):
				lines[i] = "deploy_model: Classify execution requests that need a create confirmation flow as deploy_model. The user wants to RUN or DEPLOY a specific model, framework, or application (e.g. 部署 Qwen2.5-32B / 跑数字人 / 搭一个能跑 ComfyUI 的环境 / 部署 Llama3 做推理) and wants a suitable instance created for that workload — the agent picks the image and GPU. Use deploy_model with empty target_refs. Route recommendation, how-to, price, configuration-sizing, comparison, or feasibility questions about deployment to knowledge_qa, pricing_query, or gpu_specs_query as appropriate; do not use deploy_model. API-subscription developer tools and coding assistants used through an API key or platform package — Claude Code, Codex, Coding Plan, Cursor — are NOT deployable models (they call a hosted API; there are no weights to run on an instance); classify how-to-use/configure questions about them as knowledge_qa, not deploy_model. Use operation_lifecycle only for operations on an EXISTING instance (start/stop/reboot/resize/add-disk). Use create_instance for new GPU instance creation where the user dictates exact hardware or says to create/open/buy an instance."
			case strings.HasPrefix(line, "Resource operation commands"):
				lines[i] = "Classify resource operation commands on EXISTING instances as operation_lifecycle, regardless of whether the user specifies a target instance. Action verbs include 关机 / 停机 / 停了 / 启动 / 开机 / 重启 / 加盘 / 加数据盘 / 变配 / 升级配置 / 重装 / 重置密码 / 改名. Classify new GPU instance creation where the user asks to 创建 / 开一台 / 搞台 / 抢一台 / 买一台 a machine as create_instance, not operation_lifecycle. Route workload-first requests naming a model/app/framework (e.g. 部署 DeepSeekR1 / 部署数字人 / 部署 ComfyUI) to deploy_model. When an existing-instance target is given, populate target_refs (UHostId, name, or filter). When the user omits the target (e.g. 帮我关机, 启动一下, 重启那台), still emit operation_lifecycle with target_refs:[] — the engine will list the user's instances and prompt for selection. Concrete anchors: 帮我关机 uhost-xxx, uhost-test 停了, 启动 train-gpu, 把 uhost-xxx 重启一下, 给 uhost-xxx 加 200G 数据盘. Do NOT route bare action verbs to resource_info (that intent is for listing/inspecting only) or unknown (the action is on-platform)."
			}
		}
	}
	return strings.Join(lines, "\n")
}

func buildSystemPrompt() string {
	return buildSystemPromptWithUnifiedCreate(false)
}

func buildSystemPromptWithUnifiedCreate(unified bool) string {
	base := basePromptScaffoldWithUnifiedCreate(unified)
	// Routing Registry v1 (PR A, 2026-05-18): append directives + one-shot
	// examples that come from internal/routing/*/route.yaml
	// metadata. Engine.go has a single generic dispatch hook; router-side
	// directives + examples are the only place that "knows about" new
	// routes, so adding a route stays data-only.
	directives, examples := RoutingPromptFragments()
	// PR5: cross-intent classification tie-breakers (stock vs resource, ...) are
	// projected from internal/boundarypacks as a contiguous directive block,
	// after the routing directives and before the routing examples. The pack is
	// the single source of these rules; the base scaffold no longer carries them.
	boundaryDirectives := boundarypacks.BoundaryPromptFragments()
	routerExamples := renderRouterPromptExampleGroups(routerPromptExampleGroupsWithUnifiedCreate(unified))
	parts := make([]string, 0, 1+len(routerExamples)+len(directives)+len(boundaryDirectives)+len(examples))
	parts = append(parts, base)
	parts = append(parts, routerExamples...)
	parts = append(parts, directives...)
	parts = append(parts, boundaryDirectives...)
	parts = append(parts, examples...)
	return strings.Join(parts, "\n")
}

func routerRuntimeIntents(unified bool) []Intent {
	intents := RuntimeIntents()
	if unified {
		return intents
	}
	out := make([]Intent, 0, len(intents))
	for _, i := range intents {
		if i != IntentCreateInstance {
			out = append(out, i)
		}
	}
	return out
}

// lastAssistantSnippetCap is the byte cap applied to LastAssistantSnippet
// before it is emitted into the intent-router user prompt. ~200 chars keeps each
// turn's prior-signal payload under ~100 tokens while preserving enough of
// the prior reply to disambiguate topic continuity (e.g. "刚才聊的 Suno").
const lastAssistantSnippetCap = 200

// routerPriorContextCap is a hard prompt-side bound, independent from the
// engine's assembler. It is large enough for the newest two complete exchanges
// and small enough to keep router JSON reliability stable across long sessions.
const routerPriorContextCap = 2000

func buildUserPrompt(input IntentRouterInput, retryInstruction string) string {
	var b strings.Builder
	if retryInstruction != "" {
		b.WriteString(retryInstruction)
		b.WriteString("\n")
	}
	b.WriteString("User question: ")
	b.WriteString(input.UserText)
	if input.ImageContext != "" {
		b.WriteString("\nScreenshot summary: ")
		b.WriteString(input.ImageContext)
	}
	if input.LastIntent != "" {
		b.WriteString("\nLast intent: ")
		b.WriteString(input.LastIntent)
	}
	if prior := truncatePlannerSnippet(input.PriorText, routerPriorContextCap); prior != "" {
		b.WriteString("\nRecent conversation (bounded):\n")
		b.WriteString(prior)
	}
	if input.LastSelectedInstanceID != "" {
		b.WriteString("\nLast selected instance: ")
		b.WriteString(input.LastSelectedInstanceID)
	}
	if snippet := truncatePlannerSnippet(input.LastAssistantSnippet, lastAssistantSnippetCap); snippet != "" {
		b.WriteString("\nLast assistant snippet: ")
		b.WriteString(snippet)
	}
	return b.String()
}

func truncatePlannerSnippet(s string, cap int) string {
	s = strings.TrimSpace(s)
	if s == "" || cap <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= cap {
		return s
	}
	return string(runes[:cap])
}

func unknownFallbackPlan() IntentRoute {
	return IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentUnknown,
		Confidence:    0,
	}
}

func errorAsValidation(err error, target **ValidationError) bool {
	if err == nil {
		return false
	}
	if v, ok := err.(*ValidationError); ok {
		*target = v
		return true
	}
	return false
}
