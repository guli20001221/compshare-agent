package renderer

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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
	Fallback string
	Model    string
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

type GroundedRenderer struct {
	client LLMClient
}

func NewGroundedRenderer(client LLMClient) *GroundedRenderer {
	return &GroundedRenderer{client: client}
}

func (r *GroundedRenderer) Render(ctx context.Context, req RenderRequest) RenderResult {
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
	resp, err := r.client.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: groundedSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: string(payload)},
		},
	})
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
