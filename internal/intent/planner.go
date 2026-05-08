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
		raw, err := p.llm.CompleteIntentPlan(ctx, PlannerLLMRequest{
			Mode:         mode,
			SystemPrompt: systemPrompt,
			UserPrompt:   userPrompt,
		})
		if err != nil {
			return result, err
		}

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
	return strings.Join([]string{
		"你是 CompShare 控制台 agent 的 IntentPlan planner。",
		"只输出 JSON，不输出 Markdown 或解释。",
		"schema_version 必须是 1.0。",
		"可用 intent enum: monitor_query, monitor_history, resource_info, billing_instance, billing_account_unsupported, expiry_renewal, diagnosis, vague_failure, operation_lifecycle, recommendation, knowledge_qa, mixed_diagnosis_kb, mixed_billing_kb, unknown。",
		"Phase 0 只重点分类 monitor_query, billing_instance, billing_account_unsupported；其它不确定问题输出 unknown。",
		"不得生成用户原文或引用历史中没有逐字出现的 uhost ID。",
	}, "\n")
}

func buildUserPrompt(input PlannerInput, retryInstruction string) string {
	var b strings.Builder
	if retryInstruction != "" {
		b.WriteString(retryInstruction)
		b.WriteString("\n")
	}
	b.WriteString("用户问题：")
	b.WriteString(input.UserText)
	if input.PriorText != "" {
		b.WriteString("\n引用历史：")
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
