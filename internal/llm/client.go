package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

// Client wraps go-openai to talk to ModelVerse (OpenAI-compatible).
type Client struct {
	client *openai.Client
	model  string
}

const maxChatAttempts = 2

func NewClient(cfg config.LLMConfig) *Client {
	ocfg := openai.DefaultConfig(cfg.APIKey)
	ocfg.BaseURL = cfg.BaseURL

	// Bypass HTTP proxy for localhost connections (local LLM proxy).
	if strings.Contains(cfg.BaseURL, "127.0.0.1") || strings.Contains(cfg.BaseURL, "localhost") {
		ocfg.HTTPClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy: nil, // no proxy for localhost
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		}
	}

	return &Client{
		client: openai.NewClientWithConfig(ocfg),
		model:  cfg.Model,
	}
}

// ChatRequest holds everything needed for one LLM call.
type ChatRequest struct {
	Messages []openai.ChatCompletionMessage
	Tools    []openai.Tool
	// ToolChoice forces tool selection when non-nil. Accepts either a
	// string ("auto"/"required"/"none") or an openai.ToolChoice struct
	// naming a specific function. Leave nil for default auto behavior.
	ToolChoice any
	// OnTextDelta, when non-nil, is called for each non-empty text content
	// delta as it arrives from the stream. It is NOT called for empty deltas
	// or for tool-call-only chunks.
	OnTextDelta func(string)
}

// ChatResponse wraps the LLM output.
type ChatResponse struct {
	Content   string
	ToolCalls []openai.ToolCall
	Usage     TokenUsage
}

type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Chat sends a streaming request and assembles the full response.
// Streaming is required because the proxy drops content in non-streaming mode.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	resp, err := c.chat(ctx, req, true)
	if err != nil && isUsageUnsupportedChatError(err) {
		return c.chat(ctx, req, false)
	}
	return resp, err
}

func (c *Client) chat(ctx context.Context, req ChatRequest, includeUsage bool) (*ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt < maxChatAttempts; attempt++ {
		resp, err := c.chatOnce(ctx, req, includeUsage)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isTransientChatError(ctx, err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) chatOnce(ctx context.Context, req ChatRequest, includeUsage bool) (*ChatResponse, error) {
	ccReq := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: req.Messages,
		Stream:   true,
	}
	if includeUsage {
		ccReq.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	if len(req.Tools) > 0 {
		ccReq.Tools = req.Tools
	}
	if req.ToolChoice != nil {
		ccReq.ToolChoice = req.ToolChoice
	}

	stream, err := c.client.CreateChatCompletionStream(ctx, ccReq)
	if err != nil {
		return nil, fmt.Errorf("llm stream: %w", err)
	}
	defer stream.Close()

	var contentBuf strings.Builder
	var usage TokenUsage
	toolCallMap := make(map[int]*openai.ToolCall) // index → accumulated tool call

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("llm stream recv: %w", err)
		}

		if chunk.Usage != nil {
			usage = TokenUsage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// Accumulate text content
		if delta.Content != "" {
			contentBuf.WriteString(delta.Content)
			if req.OnTextDelta != nil {
				req.OnTextDelta(delta.Content)
			}
		}

		// Accumulate tool calls
		for _, tc := range delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			existing, ok := toolCallMap[idx]
			if !ok {
				existing = &openai.ToolCall{
					Index: tc.Index,
					Type:  tc.Type,
				}
				toolCallMap[idx] = existing
			}
			if tc.ID != "" {
				existing.ID = tc.ID
			}
			if tc.Function.Name != "" {
				existing.Function.Name = tc.Function.Name
			}
			existing.Function.Arguments += tc.Function.Arguments
		}
	}

	// Convert map to sorted slice (handles sparse indices like [0, 2])
	var toolCalls []openai.ToolCall
	if len(toolCallMap) > 0 {
		keys := make([]int, 0, len(toolCallMap))
		for k := range toolCallMap {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			toolCalls = append(toolCalls, *toolCallMap[k])
		}
	}

	return &ChatResponse{
		Content:   contentBuf.String(),
		ToolCalls: toolCalls,
		Usage:     usage,
	}, nil
}

func isUsageUnsupportedChatError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "stream_options") || strings.Contains(msg, "include_usage") {
		return strings.Contains(msg, "not support") ||
			strings.Contains(msg, "unsupported") ||
			strings.Contains(msg, "does not support") ||
			strings.Contains(msg, "not allowed") ||
			strings.Contains(msg, "not permitted") ||
			strings.Contains(msg, "unrecognized") ||
			strings.Contains(msg, "not recognized") ||
			strings.Contains(msg, "unknown parameter")
	}
	return false
}

func isTransientChatError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "tls handshake timeout") ||
		strings.Contains(msg, "timeout")
}
