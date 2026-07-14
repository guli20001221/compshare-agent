package renderer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

const (
	AttributionEnvelope = "envelope"

	FallbackNone             = ""
	FallbackLLMError         = "llm_error"
	FallbackValidationFailed = "validation_failed"
	FallbackRateLimited      = "rate_limited"
)

type LLMClient interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

type Renderer interface {
	Render(ctx context.Context, req RenderRequest) RenderResult
}

type RenderRequest struct {
	Envelope envelope.Envelope
	// TaskSpec tells the renderer what the user is asking about. It is
	// deliberately separate from Envelope: task text may contain mistakes or
	// hostile instructions and is never factual evidence.
	TaskSpec TaskSpec
	Fallback string
	Model    string
}

// TaskSpec is understanding-only conversation context for a grounded answer.
// Envelope remains the sole factual source and the output validator never
// receives TaskSpec. EntityHints are identity-only breadcrumbs, not authority
// to act on an entity or proof that it still exists.
type TaskSpec struct {
	CurrentQuestion string               `json:"current_question,omitempty"`
	Intent          string               `json:"intent,omitempty"`
	Goal            string               `json:"goal,omitempty"`
	Stage           string               `json:"stage,omitempty"`
	Freshness       string               `json:"freshness,omitempty"`
	Constraints     []string             `json:"constraints,omitempty"`
	Decisions       []string             `json:"decisions,omitempty"`
	MissingSlots    []string             `json:"missing_slots,omitempty"`
	ContextSummary  string               `json:"context_summary,omitempty"`
	UnresolvedTasks []string             `json:"unresolved_tasks,omitempty"`
	EntityHints     []TaskSpecEntityHint `json:"entity_hints,omitempty"`
}

// TaskSpecEntityHint carries only conversational identity. Source, permissions,
// operation arguments, and live values must never be placed here.
type TaskSpecEntityHint struct {
	Kind      string `json:"kind,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Freshness string `json:"freshness,omitempty"`
}

type taskSpecPayload struct {
	TaskSpec TaskSpec `json:"task_spec"`
}

type RenderResult struct {
	Text            string
	Model           string
	LatencyMS       int64
	AttributionMode string
	EnvelopeHash    string
	FallbackUsed    bool
	FallbackReason  string
	Usage           llm.TokenUsage
}

type GroundedGenerator struct {
	client LLMClient
}

func NewGroundedGenerator(client LLMClient) *GroundedGenerator {
	return &GroundedGenerator{client: client}
}

func (r *GroundedGenerator) Render(ctx context.Context, req RenderRequest) RenderResult {
	start := time.Now()
	hash, _ := envelope.Hash(req.Envelope)
	result := RenderResult{
		Text:            req.Fallback,
		Model:           req.Model,
		AttributionMode: AttributionEnvelope,
		EnvelopeHash:    hash,
		FallbackUsed:    true,
		FallbackReason:  FallbackLLMError,
	}
	if r == nil || r.client == nil {
		result.LatencyMS = time.Since(start).Milliseconds()
		return result
	}
	payload, err := json.Marshal(req.Envelope)
	if err != nil {
		result.LatencyMS = time.Since(start).Milliseconds()
		return result
	}
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: groundedSystemPrompt},
	}
	if task := normalizeTaskSpec(req.TaskSpec); !taskSpecEmpty(task) {
		taskPayload, marshalErr := json.Marshal(taskSpecPayload{TaskSpec: task})
		if marshalErr != nil {
			result.LatencyMS = time.Since(start).Milliseconds()
			return result
		}
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: string(taskPayload),
		})
	}
	// Keep the fact envelope last so it is structurally distinct from the
	// untrusted task specification and remains the only source of truth.
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: string(payload),
	})
	resp, err := r.client.Chat(ctx, llm.ChatRequest{Messages: messages})
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil || resp == nil {
		if errors.Is(err, governance.ErrRateLimited) {
			result.FallbackReason = FallbackRateLimited
		}
		return result
	}
	if err := ValidateRenderedText(req.Envelope, resp.Content); err != nil {
		result.Usage = resp.Usage
		result.FallbackReason = FallbackValidationFailed
		return result
	}
	result.Text = resp.Content
	result.Usage = resp.Usage
	result.FallbackUsed = false
	result.FallbackReason = FallbackNone
	return result
}

const (
	maxTaskQuestionRunes = 800
	maxTaskTextRunes     = 600
	maxTaskSummaryRunes  = 1200
	maxTaskListItems     = 8
	maxTaskEntityHints   = 8
)

func normalizeTaskSpec(in TaskSpec) TaskSpec {
	in.CurrentQuestion = boundedTaskText(in.CurrentQuestion, maxTaskQuestionRunes)
	in.Intent = boundedTaskText(in.Intent, maxTaskTextRunes)
	in.Goal = boundedTaskText(in.Goal, maxTaskTextRunes)
	in.Stage = boundedTaskText(in.Stage, maxTaskTextRunes)
	in.Freshness = boundedTaskText(in.Freshness, maxTaskTextRunes)
	in.ContextSummary = boundedTaskText(in.ContextSummary, maxTaskSummaryRunes)
	in.Constraints = normalizeTaskItems(in.Constraints)
	in.Decisions = normalizeTaskItems(in.Decisions)
	in.MissingSlots = normalizeTaskItems(in.MissingSlots)
	in.UnresolvedTasks = normalizeTaskItems(in.UnresolvedTasks)
	if len(in.EntityHints) > maxTaskEntityHints {
		in.EntityHints = in.EntityHints[len(in.EntityHints)-maxTaskEntityHints:]
	}
	entities := make([]TaskSpecEntityHint, 0, len(in.EntityHints))
	for _, hint := range in.EntityHints {
		hint.Kind = boundedTaskText(hint.Kind, maxTaskTextRunes)
		hint.ID = boundedTaskText(hint.ID, maxTaskTextRunes)
		hint.Name = boundedTaskText(hint.Name, maxTaskTextRunes)
		hint.Freshness = boundedTaskText(hint.Freshness, maxTaskTextRunes)
		if hint.ID == "" && hint.Name == "" {
			continue
		}
		entities = append(entities, hint)
	}
	in.EntityHints = entities
	return in
}

func normalizeTaskItems(in []string) []string {
	if len(in) > maxTaskListItems {
		in = in[len(in)-maxTaskListItems:]
	}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item = boundedTaskText(item, maxTaskTextRunes); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func boundedTaskText(in string, maxRunes int) string {
	in = strings.Join(strings.Fields(strings.TrimSpace(in)), " ")
	if maxRunes <= 0 || utf8.RuneCountInString(in) <= maxRunes {
		return in
	}
	runes := []rune(in)
	return string(runes[:maxRunes]) + "…"
}

func taskSpecEmpty(in TaskSpec) bool {
	return in.CurrentQuestion == "" && in.Intent == "" && in.Goal == "" &&
		in.Stage == "" && in.Freshness == "" && len(in.Constraints) == 0 &&
		len(in.Decisions) == 0 && len(in.MissingSlots) == 0 &&
		in.ContextSummary == "" && len(in.UnresolvedTasks) == 0 &&
		len(in.EntityHints) == 0
}
