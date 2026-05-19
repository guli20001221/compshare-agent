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
	UserText  string
	PriorText string
	Resolver  EntityResolver
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

func buildSystemPrompt() string {
	// Keep the planner scaffold ASCII/English. Earlier Windows console/source
	// encoding issues made non-ASCII prompt labels fragile, while the baseline
	// model handles bilingual user text with English JSON-field instructions.
	base := strings.Join([]string{
		"You are the IntentPlan planner for the CompShare console agent.",
		"Return exactly one JSON object. Do not output Markdown, prose, or tool calls.",
		"Required top-level fields: schema_version, intent, slots, required_tools, retrieval, hard_block_hint, confidence.",
		"schema_version must be \"1.0\". confidence must be a number in [0,1]. retrieval.enabled must be false for the current demo slice.",
		"Allowed intent enum: monitor_query, monitor_history, resource_info, billing_instance, billing_account_unsupported, expiry_renewal, diagnosis, vague_failure, operation_lifecycle, recommendation, knowledge_qa, gpu_specs_query, stock_availability, platform_image_list, custom_image_list, community_image_list, unknown.",
		"Phase 1 demo focus: classify clear resource inventory questions as resource_info and clear current monitoring questions as monitor_query.",
		"Treat performance questions like CPU high, GPU busy/idle, memory high, VRAM high, or whether a machine is idle as monitor_query first, unless the user states a concrete SSH, init, billing, lifecycle, or instance-internal operation problem.",
		"Historical monitor phrases like yesterday, last night, today morning, or X点到Y点 must use monitor_history or a non-current time_window, never preset now/today.",
		"Stage 2B retrieval focus: classify clear platform usage / FAQ questions as knowledge_qa.",
		"For diagnosis questions that also reference platform FAQ or usage docs should still emit diagnosis.",
		"Platform how-to/config/error-code questions like how to configure remote desktop audio, how to install drivers, what does error code 226601 mean, how to publish a community image, or how to set BaseURL should emit knowledge_qa, even if phrased as a problem.",
		"The distinction is: 'how do I do X on the platform' = knowledge_qa; 'my specific instance has problem X' with target_refs = diagnosis. Without a concrete instance target, default to knowledge_qa for usage/config/error-code questions.",
		"Comparison questions ('X 和 Y 哪个划算' / 'X vs Y'), yes-no feasibility questions ('X 可以 Y 吗' / 'can I X'), and procedure-description questions ('X 流程是怎样的' / 'how does X work') about platform usage, pricing, image, instance, or billing should emit knowledge_qa unless they reference a specific instance target.",
		"Inventory availability questions like whether a GPU model has stock, is available, is sold out, or has data-center inventory are not resource_info. resource_info is only for the user's own CompShare instances. Platform stock questions should emit unknown so the normal tool loop can choose inventory tools.",
		"For billing-specific FAQ plus instance facts should emit billing_instance; unsupported account totals still use billing_account_unsupported.",
		"finance policy/how-to questions like invoice issuance, refund rules, arrears handling, why am I still charged after shutdown, billing mode differences, or package expiry should emit knowledge_qa.",
		"account realtime finance/status questions about THE USER'S OWN ACCOUNT data — balance, total bills, transaction records, charge records, payable bills, my invoice status (e.g. 我的发票开好了吗 / 我账单还剩多少), my refund progress, recharge amount on my account — emit billing_account_unsupported.",
		"FAQ/process questions about HOW the system works — invoice issuance schedule (什么时候开发票 / 开票周期), refund process flow, arrears policy (欠费几天回收), expiry rules — emit knowledge_qa, not billing_account_unsupported.",
		"When ambiguous between process-question and personal-status (e.g. 我的发票什么时候开 — could be either), default to knowledge_qa unless the user explicitly asks for the realtime state of a specific personal record (我的 X 开好了吗 / 寄了吗 / 进度 / 多少).",
		"Diagnostic phrasings that pair a finance topic with non-finance symptoms (e.g. 下载速度突然变慢 是欠费了吗 还是网络高峰) emit knowledge_qa — the user is asking for root-cause checklist, not their own balance amount.",
		"If a single question mixes finance FAQ with account realtime personal-status data, emit billing_account_unsupported for the whole turn.",
		"instance-scoped billing questions should emit billing_instance, but do not promise account ledger amounts or transaction exports.",
		"Billing navigation questions like where do I find / how do I view / how to check / from which page can I see my bills, invoices, expense, balance, charges, or recharge history should emit knowledge_qa - they ask for a UI navigation path, not actual finance numbers, and the docs cover the path.",
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
		"User question: show resource info for my-test-agent",
		"{\"schema_version\":\"1.0\",\"intent\":\"resource_info\",\"slots\":{\"target_refs\":[{\"type\":\"name\",\"value\":\"my-test-agent\",\"source\":\"user_text\",\"source_span\":\"my-test-agent\"}],\"metrics\":[],\"time_window\":null},\"required_tools\":[\"DescribeCompShareInstance\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: which machines are running",
		"{\"schema_version\":\"1.0\",\"intent\":\"resource_info\",\"slots\":{\"target_refs\":[{\"type\":\"filter\",\"value\":\"state=running\"}],\"metrics\":[],\"time_window\":null},\"required_tools\":[\"DescribeCompShareInstance\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: which 4090 machines are stopped",
		"{\"schema_version\":\"1.0\",\"intent\":\"resource_info\",\"slots\":{\"target_refs\":[{\"type\":\"filter\",\"value\":\"state=stopped\"},{\"type\":\"filter\",\"value\":\"gpu_type=4090\"}],\"metrics\":[],\"time_window\":null},\"required_tools\":[\"DescribeCompShareInstance\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: \u6211\u8d26\u53f7\u4e0b\u6709\u54ea\u4e9b 4090 \u5b9e\u4f8b",
		"{\"schema_version\":\"1.0\",\"intent\":\"resource_info\",\"slots\":{\"target_refs\":[{\"type\":\"filter\",\"value\":\"gpu_type=4090\"}],\"metrics\":[],\"time_window\":null},\"required_tools\":[\"DescribeCompShareInstance\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: \u4e0a\u6d77\u673a\u623f\u8fd8\u5269\u6ca1\u5269 H100 \u5e93\u5b58",
		"{\"schema_version\":\"1.0\",\"intent\":\"unknown\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.7}",
		"User question: 4090 \u8fd8\u6709\u6ca1\u6709\u8d27",
		"{\"schema_version\":\"1.0\",\"intent\":\"unknown\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.7}",
		"User question: show current CPU and GPU monitor for my-test-agent",
		"{\"schema_version\":\"1.0\",\"intent\":\"monitor_query\",\"slots\":{\"target_refs\":[{\"type\":\"name\",\"value\":\"my-test-agent\",\"source\":\"user_text\",\"source_span\":\"my-test-agent\"}],\"metrics\":[\"cpu\",\"gpu\"],\"time_window\":{\"type\":\"preset\",\"value\":\"now\"}},\"required_tools\":[\"GetCompShareInstanceMonitor\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: CPU is high, what should I do",
		"{\"schema_version\":\"1.0\",\"intent\":\"monitor_query\",\"slots\":{\"target_refs\":[],\"metrics\":[\"cpu\"],\"time_window\":{\"type\":\"preset\",\"value\":\"now\"}},\"required_tools\":[\"GetCompShareInstanceMonitor\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: account balance",
		"{\"schema_version\":\"1.0\",\"intent\":\"billing_account_unsupported\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":true,\"confidence\":0.9}",
		"User question: how do I issue an invoice",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: what is my invoice status",
		"{\"schema_version\":\"1.0\",\"intent\":\"billing_account_unsupported\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":true,\"confidence\":0.9}",
		"User question: what image types does the platform provide",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.82}",
		"User question: \u8fdc\u7a0b\u684c\u9762\u6ca1\u58f0\u97f3\u8be5\u600e\u4e48\u5904\u7406",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u9519\u8bef\u7801 226601 \u662f\u4ec0\u4e48\u610f\u601d",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: Linux \u600e\u4e48\u88c5 NVIDIA \u9a71\u52a8",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: Coding Plan \u7684 BaseURL \u5e94\u8be5\u586b\u4ec0\u4e48",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u600e\u4e48\u5728 VSCode \u91cc\u8fde GPU \u5b9e\u4f8b",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u600e\u4e48\u67e5\u6211\u8fd9\u4e2a\u6708\u7684\u8d26\u5355",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u54ea\u91cc\u53ef\u4ee5\u770b\u53d1\u7968\u53d1\u8d77\u8bb0\u5f55",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u5305\u6708\u548c\u6309\u91cf\u54ea\u4e2a\u5212\u7b97",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u5b9e\u4f8b\u78c1\u76d8\u53ef\u4ee5\u6269\u5bb9\u5417",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: \u9000\u6b3e\u6d41\u7a0b\u662f\u600e\u6837\u7684",
		"{\"schema_version\":\"1.0\",\"intent\":\"knowledge_qa\",\"slots\":{\"target_refs\":[],\"metrics\":[],\"time_window\":null},\"required_tools\":[],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
		"User question: uhost-abc123 \u8fd9\u53f0\u542f\u52a8\u5931\u8d25\u4e86\u5e2e\u6211\u67e5",
		"{\"schema_version\":\"1.0\",\"intent\":\"diagnosis\",\"slots\":{\"target_refs\":[{\"type\":\"uhost_id_user_input\",\"value\":\"uhost-abc123\",\"source\":\"user_text\",\"source_span\":\"uhost-abc123\"}],\"metrics\":[],\"time_window\":null},\"required_tools\":[\"DescribeCompShareInstance\"],\"retrieval\":{\"enabled\":false},\"hard_block_hint\":false,\"confidence\":0.85}",
	}, "\n")
	// Capability Registry v1 (PR A, 2026-05-18): append directives + one-shot
	// examples that come from internal/intent/capabilities/*.md frontmatter
	// metadata. Engine.go has a single generic dispatch hook; planner-side
	// directives + examples are the only place that "knows about" new
	// capabilities, so adding a capability stays data-only.
	directives, examples := CapabilityPromptFragments()
	parts := make([]string, 0, 1+len(directives)+len(examples))
	parts = append(parts, base)
	parts = append(parts, directives...)
	parts = append(parts, examples...)
	return strings.Join(parts, "\n")
}

func buildUserPrompt(input PlannerInput, retryInstruction string) string {
	var b strings.Builder
	if retryInstruction != "" {
		b.WriteString(retryInstruction)
		b.WriteString("\n")
	}
	b.WriteString("User question: ")
	b.WriteString(input.UserText)
	if input.PriorText != "" {
		b.WriteString("\nPrior turns: ")
		b.WriteString(input.PriorText)
	}
	return b.String()
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
