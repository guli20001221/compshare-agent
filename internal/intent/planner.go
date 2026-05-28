package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/llm"
)

type OutputMode string

const (
	OutputModeJSONSchema       OutputMode = "json_schema"
	OutputModeJSONObject       OutputMode = "json_object"
	OutputModeStrictPromptJSON OutputMode = "strict_prompt_json"
)

type PlannerLLM interface {
	CompleteIntentPlan(ctx context.Context, req PlannerLLMRequest) (string, error)
}

type PlannerLLMWithUsage interface {
	CompleteIntentPlanWithUsage(ctx context.Context, req PlannerLLMRequest) (PlannerLLMResponse, error)
}

type PlannerLLMResponse struct {
	Content string
	Usage   llm.TokenUsage
}

type PlannerLLMRequest struct {
	Mode         OutputMode
	SystemPrompt string
	UserPrompt   string
}

type PlannerOptions struct {
	BaseURL          string
	Model            string
	MaxRetries       int
	LookupCapability func(baseURL, model string) llm.Capability
}

type PlannerInput struct {
	UserText     string
	ImageContext string
	LastIntent   string
	// PriorText is retained as the validator's `source:prior_turn` span
	// haystack (see ValidationContext.PriorText). The planner USER prompt
	// no longer dumps it verbatim — PR1 hotfix Bug 2 (2026-05-28): structured
	// signals (LastSelectedInstanceID + LastAssistantSnippet) replace it to
	// avoid the 5k→11k input_tok avalanche that broke ds-v4-flash JSON
	// schema reliability across turns. See memory:planner-input-only-needs-
	// structured-signals.
	PriorText string
	// LastSelectedInstanceID surfaces SessionState.SelectedInstanceID so the
	// planner can resolve "那台机" / "它" cross-turn references without seeing
	// the full transcript.
	LastSelectedInstanceID string
	// LastAssistantSnippet is the prefix of the most recent assistant reply
	// (capped at ~200 chars) used as a low-token topic continuity hint.
	LastAssistantSnippet string
	Resolver             EntityResolver
	// Deprecated: use Resolver so production shadow mode can pass immutable
	// registry snapshots without exposing EntityRegistry internals.
	Registry *entity.EntityRegistry
}

type PlannerResult struct {
	Plan               Plan
	Mode               OutputMode
	Attempts           int
	Fallback           bool
	LastValidationCode ErrorCode
	Usage              llm.TokenUsage
}

type Planner struct {
	llm              PlannerLLM
	baseURL          string
	model            string
	maxRetries       int
	lookupCapability func(baseURL, model string) llm.Capability
}

func NewPlanner(client PlannerLLM, opts PlannerOptions) *Planner {
	lookup := opts.LookupCapability
	if lookup == nil {
		lookup = llm.LookupCapability
	}
	maxRetries := opts.MaxRetries
	if maxRetries == 0 {
		maxRetries = 1
	}
	return &Planner{
		llm:              client,
		baseURL:          opts.BaseURL,
		model:            opts.Model,
		maxRetries:       maxRetries,
		lookupCapability: lookup,
	}
}

func SelectOutputMode(cap llm.Capability) OutputMode {
	if cap.SupportsJSONSchema && !cap.IsThinkingMode {
		return OutputModeJSONSchema
	}
	if cap.SupportsJSONObject {
		return OutputModeJSONObject
	}
	return OutputModeStrictPromptJSON
}

func (p *Planner) Plan(ctx context.Context, input PlannerInput) (PlannerResult, error) {
	mode := SelectOutputMode(p.lookupCapability(p.baseURL, p.model))
	result := PlannerResult{
		Plan:     unknownFallbackPlan(),
		Mode:     mode,
		Fallback: true,
	}
	if p.llm == nil {
		return result, fmt.Errorf("intent planner LLM is nil")
	}

	systemPrompt := buildSystemPrompt()
	userPrompt := buildUserPrompt(input, "")
	attempts := p.maxRetries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		result.Attempts = attempt
		raw, usage, err := p.completeIntentPlan(ctx, PlannerLLMRequest{
			Mode:         mode,
			SystemPrompt: systemPrompt,
			UserPrompt:   userPrompt,
		})
		if err != nil {
			return result, err
		}
		result.Usage = addTokenUsage(result.Usage, usage)

		plan, parseErr := parsePlanJSON(raw)
		if parseErr == nil {
			err = ValidatePlan(plan, ValidationContext{
				UserText:  input.UserText,
				PriorText: input.PriorText,
				Resolver:  input.entityResolver(),
				Registry:  input.Registry,
			})
			if err == nil {
				return PlannerResult{
					Plan:     plan,
					Mode:     mode,
					Attempts: attempt,
					Usage:    result.Usage,
				}, nil
			}
			var validationErr *ValidationError
			if errorAsValidation(err, &validationErr) {
				result.LastValidationCode = validationErr.Code
			}
		}

		userPrompt = buildUserPrompt(input, "上一轮输出不是合法 IntentPlan JSON，必须只返回符合 schema v1.0 的 JSON 对象。")
	}
	return result, nil
}

func (p *Planner) completeIntentPlan(ctx context.Context, req PlannerLLMRequest) (string, llm.TokenUsage, error) {
	if withUsage, ok := p.llm.(PlannerLLMWithUsage); ok {
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

func (input PlannerInput) entityResolver() EntityResolver {
	if input.Resolver != nil {
		return input.Resolver
	}
	if input.Registry != nil {
		return input.Registry
	}
	return nil
}

func parsePlanJSON(raw string) (Plan, error) {
	var plan Plan
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

type plannerPromptExample struct {
	Question string
	PlanJSON string
	Source   string
}

type plannerPromptExampleGroup struct {
	Intent   Intent
	Source   string
	Examples []plannerPromptExample
	// compact renders the group as a shared plan JSON + question list instead
	// of repeating the full JSON per example. Use for groups where all examples
	// share the same output structure (e.g. knowledge_qa: empty slots, empty
	// required_tools). Saves ~1,100 tokens for the 20-example knowledge_qa group.
	compact bool
}

func plannerPromptExampleGroups() []plannerPromptExampleGroup {
	return []plannerPromptExampleGroup{
		{
			Intent: IntentResourceInfo,
			Source: "Phase 1 baseline resource inventory cutover",
			Examples: []plannerPromptExample{
				{
					Question: "show resource info for my-test-agent",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"name","value":"my-test-agent","source":"user_text","source_span":"my-test-agent"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Phase 1 baseline: named instance resource lookup",
				},
				{
					Question: "which machines are running",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"state=running"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Phase 1 baseline: state filter",
				},
				{
					Question: "which 4090 machines are stopped",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"state=stopped"},{"type":"filter","value":"gpu_type=4090"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Phase 1 baseline: compound inventory filters",
				},
				{
					Question: "我账号下有哪些 4090 实例",
					PlanJSON: `{"schema_version":"1.0","intent":"resource_info","slots":{"target_refs":[{"type":"filter","value":"gpu_type=4090"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Phase 1 baseline: account inventory filter",
				},
			},
		},
		{
			Intent: IntentUnknown,
			Source: "Phase 1 demo boundary: unsupported non-platform requests",
			Examples: []plannerPromptExample{
				{
					Question: "今天北京天气怎么样",
					PlanJSON: `{"schema_version":"1.0","intent":"unknown","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.7}`,
					Source:   "Phase 1 boundary: unsupported general knowledge",
				},
				{
					Question: "帮我写一首和平台无关的诗",
					PlanJSON: `{"schema_version":"1.0","intent":"unknown","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.7}`,
					Source:   "Phase 1 boundary: unrelated creative request",
				},
			},
		},
		{
			Intent: IntentMonitorQuery,
			Source: "Phase 1 baseline monitor cutover",
			Examples: []plannerPromptExample{
				{
					Question: "show current CPU and GPU monitor for my-test-agent",
					PlanJSON: `{"schema_version":"1.0","intent":"monitor_query","slots":{"target_refs":[{"type":"name","value":"my-test-agent","source":"user_text","source_span":"my-test-agent"}],"metrics":["cpu","gpu"],"time_window":{"type":"preset","value":"now"}},"required_tools":["GetCompShareInstanceMonitor"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Phase 1 baseline: current monitor query",
				},
				{
					Question: "CPU is high, what should I do",
					PlanJSON: `{"schema_version":"1.0","intent":"monitor_query","slots":{"target_refs":[],"metrics":["cpu"],"time_window":{"type":"preset","value":"now"}},"required_tools":["GetCompShareInstanceMonitor"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Phase 1 baseline: performance symptom maps to current monitor",
				},
			},
		},
		// IntentKnowledgeQA migrated to internal/intent/planner_examples/knowledge_qa.md
		// in C5 Phase B (PR #6, 2026-05-22). Same byte-equal contract as the
		// Phase A diagnosis migration — see TestPlannerExamples_KnowledgeQADisk
		// LoaderEqualsLegacy + the SHA hash in TestPlannerExamples_FullSystem
		// PromptStable. Editorial review of knowledge_qa anchors now happens
		// in the markdown file; planner.go retains structural code only.
		diskPlannerExampleGroups[IntentKnowledgeQA],
		{
			Intent: IntentBillingAccountUnsupported,
			Source: "PR #52 finance process vs personal-status hard-block split",
			Examples: []plannerPromptExample{
				{
					Question: "account balance",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_account_unsupported","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":true,"confidence":0.9}`,
					Source:   "PR #52: personal realtime account data hard block",
				},
				{
					Question: "what is my invoice status",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_account_unsupported","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":true,"confidence":0.9}`,
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
			// already declares "instance-scoped billing questions should emit
			// billing_instance" but planner had no one-shot example anchoring
			// the colloquial personal-complaint phrasing. Trace evidence:
			// F:/compshare-agent-runs/q04-jitter-20260520-165129.
			Intent: IntentBillingInstance,
			Source: "Stable routing for personal billing complaints with vague cause (2026-05-20 N=5 jitter check)",
			Examples: []plannerPromptExample{
				{
					Question: "充值 10 块就被扣完了 我啥也没干啊",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance","DiagnoseBilling"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Colloquial personal billing complaint — diagnose own instances",
				},
				{
					Question: "我账单怎么这么高",
					PlanJSON: `{"schema_version":"1.0","intent":"billing_instance","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance","DiagnoseBilling"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.8}`,
					Source:   "Personal billing complaint — high bill diagnostic",
				},
			},
		},
		{
			// Batch 1 (2026-05-28) jitter fix. CLI trace at N=6 over the
			// question "帮我关机 uhost-xxx" showed planner.intent split
			// 33% operation_lifecycle / 67% unknown (or schema_valid=false).
			// Root cause: operation_lifecycle had ZERO one-shot examples in
			// the planner prompt, so the classifier had no anchor for the
			// UHostId+action-verb pattern. Examples cover the five most
			// common workflows the user hit in 2026-05-28 integration:
			// 关机/启动/重启/加盘/变配, with both UHostId and Name target
			// refs and colloquial verbs (停了/重启一下).
			//
			// PR1 hotfix Bug 1 (2026-05-28): add a ZERO-target-ref anchor so
			// "帮我关机" (no UHostId, no name) classifies as
			// operation_lifecycle. Engine then lists the user's instances
			// and lets the user pick — this is the dominant path when users
			// say "关机" without specifying which one. See memory:
			// target-ref-required-for-operation-lifecycle.
			Intent: IntentOperationLifecycle,
			Source: "PR1 hotfix (2026-05-28): anchor action-verb chats including ZERO-target so 'help me shutdown' stops drifting to unknown",
			Examples: []plannerPromptExample{
				{
					Question: "帮我关机",
					PlanJSON: `{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.7}`,
					Source:   "PR1 hotfix Bug 1: ZERO target — engine lists instances and prompts for selection",
				},
				{
					Question: "帮我关机 uhost-1qx1qsw4b1pk",
					PlanJSON: `{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-1qx1qsw4b1pk","source":"user_text","source_span":"uhost-1qx1qsw4b1pk"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
					Source:   "Batch 1: 关机 + UHostId — direct from 2026-05-28 jitter trace",
				},
				{
					Question: "uhost-test 停了",
					PlanJSON: `{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-test","source":"user_text","source_span":"uhost-test"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Batch 1: 口语化 '停了' verb — anchors shutdown via colloquial speech",
				},
				{
					Question: "启动 train-gpu",
					PlanJSON: `{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"name","value":"train-gpu","source":"user_text","source_span":"train-gpu"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Batch 1: 启动 + Name target_ref — exercises name-typed resolution",
				},
				{
					Question: "把 uhost-xxx 重启一下",
					PlanJSON: `{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-xxx","source":"user_text","source_span":"uhost-xxx"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Batch 1: 重启 + 口语化 '一下'",
				},
				{
					Question: "给 uhost-1qx1qsw4b1pk 加 200G 数据盘",
					PlanJSON: `{"schema_version":"1.0","intent":"operation_lifecycle","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-1qx1qsw4b1pk","source":"user_text","source_span":"uhost-1qx1qsw4b1pk"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.82}`,
					Source:   "Batch 1: 加盘 — CreateDiskWorkflow trigger, same intent as start/stop",
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

func renderPlannerPromptExampleGroups(groups []plannerPromptExampleGroup) []string {
	lines := []string{}
	for _, group := range groups {
		lines = append(lines, fmt.Sprintf("Example group: %s (source: %s)", group.Intent, group.Source))
		if group.compact && len(group.Examples) > 0 {
			lines = append(lines, "Plan output:")
			lines = append(lines, group.Examples[0].PlanJSON)
			lines = append(lines, fmt.Sprintf("Classify as %s:", group.Intent))
			for _, example := range group.Examples {
				lines = append(lines, "- "+example.Question)
			}
		} else {
			for _, example := range group.Examples {
				lines = append(lines, "Example source: "+example.Source)
				lines = append(lines, "User question: "+example.Question)
				lines = append(lines, example.PlanJSON)
			}
		}
	}
	return lines
}

func buildSystemPrompt() string {
	// Keep the planner scaffold ASCII/English. Earlier Windows console/source
	// encoding issues made non-ASCII prompt labels fragile, while the baseline
	// model handles bilingual user text with English JSON-field instructions.
	base := strings.Join([]string{
		"You are the IntentPlan planner for the CompShare console agent.",
		"Return exactly one JSON object. Do not output Markdown, prose, or tool calls.",
		"Required top-level fields: schema_version, intent, slots, required_tools, retrieval, hard_block_hint, confidence.",
		"schema_version must be \"1.0\". confidence must be a number in [0,1]. retrieval.enabled must be false for the current demo slice.",
		"Allowed intent enum: monitor_query, monitor_history, resource_info, billing_instance, billing_account_unsupported, expiry_renewal, diagnosis, vague_failure, operation_lifecycle, recommendation, knowledge_qa, gpu_specs_query, stock_availability, platform_image_list, custom_image_list, community_image_list, pricing_query, unknown.",
		"Phase 1 demo focus: classify clear resource inventory questions as resource_info and clear current monitoring questions as monitor_query.",
		"Treat performance questions like CPU high, GPU busy/idle, memory high, VRAM high, or whether a machine is idle as monitor_query first, unless the user states a concrete SSH, init, billing, lifecycle, or instance-internal operation problem.",
		"Historical monitor phrases like yesterday, last night, today morning, or X点到Y点 must use monitor_history or a non-current time_window, never preset now/today. EXCEPTION: when these phrases appear ONLY in the Screenshot summary (not in User question), they are UI labels or navigation text from a screenshot — do NOT classify as monitor_history based on screenshot content alone. Classify based on the User question.",
		"Screenshot summary is contextual evidence (what the user sees on screen), not user intent. Use it to refine diagnosis or identify the page context, but the intent classification must be driven by the User question text. Screenshot content must never be the sole trigger for hard-block intents (monitor_history, billing_account_unsupported). Screenshot content must never be used as parameter source for mutating operations (create/stop/start/reboot) — those require explicit user input or confirmation.",
		"Stage 2B retrieval focus: classify clear platform usage / FAQ questions as knowledge_qa.",
		"For diagnosis questions that also reference platform FAQ or usage docs should still emit diagnosis.",
		"Platform how-to/config/error-code questions like how to configure remote desktop audio, how to install drivers, what does error code 226601 mean, how to publish a community image, or how to set BaseURL should emit knowledge_qa, even if phrased as a problem.",
		"The distinction is: 'how do I do X on the platform' = knowledge_qa; 'my specific instance has problem X' with target_refs = diagnosis. Without a concrete instance target, default to knowledge_qa for usage/config/error-code questions.",
		"Direct runtime/list/user price questions like 4090 多少钱, H20 按月包多少钱, 折后价多少, or actual purchase price should emit pricing_query — the capability handler runs DescribeAvailableCompShareInstanceTypes + GetCompShareInstancePrice deterministically. Personal-billing complaints (我账单怎么这么高 / 充值就被扣完了) stay as billing_instance.",
		"Comparison questions ('X 和 Y 哪个划算' / 'X vs Y'), yes-no feasibility questions ('X 可以 Y 吗' / 'can I X'), and procedure-description questions ('X 流程是怎样的' / 'how does X work') about platform usage, pricing rules, image, instance, or billing should emit knowledge_qa unless they reference a specific instance target.",
		"Inventory availability questions like whether a GPU model has stock, is available, is sold out, or has data-center inventory are not resource_info. resource_info is only for the user's own CompShare instances. Platform stock questions should emit stock_availability.",
		"For billing-specific FAQ plus instance facts should emit billing_instance; unsupported account totals still use billing_account_unsupported.",
		"finance policy/how-to questions like invoice issuance, refund rules, arrears handling, why am I still charged after shutdown, billing mode differences, or package expiry should emit knowledge_qa.",
		"account realtime finance/status questions about THE USER'S OWN ACCOUNT data — balance, total bills, transaction records, charge records, payable bills, my invoice status (e.g. 我的发票开好了吗 / 我账单还剩多少), my refund progress, recharge amount on my account — emit billing_account_unsupported.",
		"FAQ/process questions about HOW the system works — invoice issuance schedule (什么时候开发票 / 开票周期), refund process flow, arrears policy (欠费几天回收), expiry rules — emit knowledge_qa, not billing_account_unsupported.",
		"When ambiguous between process-question and personal-status (e.g. 我的发票什么时候开 — could be either), default to knowledge_qa unless the user explicitly asks for the realtime state of a specific personal record (我的 X 开好了吗 / 寄了吗 / 进度 / 多少).",
		"Diagnostic phrasings that pair a finance topic with non-finance symptoms (e.g. 下载速度突然变慢 是欠费了吗 还是网络高峰) emit knowledge_qa — the user is asking for root-cause checklist, not their own balance amount.",
		"If a single question mixes finance FAQ with account realtime personal-status data, emit billing_account_unsupported for the whole turn.",
		"instance-scoped billing questions should emit billing_instance, but do not promise account ledger amounts or transaction exports.",
		"Personal billing complaints with vague cause — 充值 10 块就被扣完了 / 我账单怎么这么高 / 钱怎么扣这么快 / 我啥也没干怎么就扣费了 — emit billing_instance (NOT billing_account_unsupported, which is reserved for explicit balance / total-bill / transaction-record queries; and NOT knowledge_qa, because the user wants a personal diagnostic, not a process FAQ).",
		"Billing navigation questions like where do I find / how do I view / how to check / from which page can I see my bills, invoices, expense, balance, charges, or recharge history should emit knowledge_qa - they ask for a UI navigation path, not actual finance numbers, and the docs cover the path.",
		"Resource operation commands — any phrase whose primary verb is a CompShare instance lifecycle / configuration action emits operation_lifecycle, REGARDLESS of whether the user specifies a target instance. Action verbs include 关机 / 停机 / 停了 / 启动 / 开机 / 重启 / 加盘 / 加数据盘 / 变配 / 升级配置 / 重装 / 重置密码 / 改名. When a target is given, populate target_refs (UHostId, name, or filter). When the user omits the target (e.g. 帮我关机, 启动一下, 重启那台), still emit operation_lifecycle with target_refs:[] — the engine will list the user's instances and prompt for selection. Concrete anchors: 帮我关机 uhost-xxx, uhost-test 停了, 启动 train-gpu, 把 uhost-xxx 重启一下, 给 uhost-xxx 加 200G 数据盘. Do NOT route bare action verbs to resource_info (that intent is for listing/inspecting only) or unknown (the action is on-platform).",
		"Use unknown when the user asks unsupported general knowledge, operations, or anything outside the demo focus.",
		"slots must contain target_refs, metrics, and time_window. Use [] for missing target_refs or metrics, and null for missing time_window.",
		"For a user-written instance name, output target_refs item {\"type\":\"name\",\"value\":\"<exact name>\",\"source\":\"user_text\",\"source_span\":\"<exact substring>\"}.",
		"For a user-written UHostId, output target_refs item {\"type\":\"uhost_id_user_input\",\"value\":\"<exact id>\",\"source\":\"user_text\",\"source_span\":\"<exact substring>\"}.",
		"For resource_info inventory filters, output target_refs items with {\"type\":\"filter\",\"value\":\"state=running\"}, {\"type\":\"filter\",\"value\":\"state=stopped\"}, or {\"type\":\"filter\",\"value\":\"gpu_type=<gpu type>\"}.",
		"Resource filters are ANDed across different fields. Do not mix filter target_refs with name or UHostId target_refs.",
		"Never invent UHostIds or instance names that do not appear verbatim in the user question or prior turns.",
		"For monitor_query, metrics may be [] when the metric words are unclear; the handler can render all returned current monitor values.",
		"Set hard_block_hint=true only for unsupported account-level billing questions such as account balance, total account bill, or transaction flow.",
		"Examples:",
	}, "\n")
	// Capability Registry v1 (PR A, 2026-05-18): append directives + one-shot
	// examples that come from internal/intent/capabilities/*.md frontmatter
	// metadata. Engine.go has a single generic dispatch hook; planner-side
	// directives + examples are the only place that "knows about" new
	// capabilities, so adding a capability stays data-only.
	directives, examples := CapabilityPromptFragments()
	plannerExamples := renderPlannerPromptExampleGroups(plannerPromptExampleGroups())
	parts := make([]string, 0, 1+len(plannerExamples)+len(directives)+len(examples))
	parts = append(parts, base)
	parts = append(parts, plannerExamples...)
	parts = append(parts, directives...)
	parts = append(parts, examples...)
	return strings.Join(parts, "\n")
}

// lastAssistantSnippetCap is the byte cap applied to LastAssistantSnippet
// before it is emitted into the planner user prompt. ~200 chars keeps each
// turn's prior-signal payload under ~100 tokens while preserving enough of
// the prior reply to disambiguate topic continuity (e.g. "刚才聊的 Suno").
const lastAssistantSnippetCap = 200

func buildUserPrompt(input PlannerInput, retryInstruction string) string {
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

func unknownFallbackPlan() Plan {
	return Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentUnknown,
		Retrieval:     Retrieval{Enabled: false},
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
