package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

// Client wraps go-openai to talk to ModelVerse (OpenAI-compatible).
type Client struct {
	client *openai.Client
	model  string
}

func NewClient(cfg config.LLMConfig) *Client {
	ocfg := openai.DefaultConfig(cfg.APIKey)
	ocfg.BaseURL = cfg.BaseURL
	return &Client{
		client: openai.NewClientWithConfig(ocfg),
		model:  cfg.Model,
	}
}

// ChatRequest holds everything needed for one LLM call.
type ChatRequest struct {
	Messages []openai.ChatCompletionMessage
	Tools    []openai.Tool
}

// ChatResponse wraps the LLM output.
type ChatResponse struct {
	Content   string
	ToolCalls []openai.ToolCall
}

// Chat sends a streaming request and assembles the full response.
// Streaming is required because the proxy drops content in non-streaming mode.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	ccReq := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: req.Messages,
		Stream:   true,
	}
	if len(req.Tools) > 0 {
		ccReq.Tools = req.Tools
	}

	stream, err := c.client.CreateChatCompletionStream(ctx, ccReq)
	if err != nil {
		return nil, fmt.Errorf("llm stream: %w", err)
	}
	defer stream.Close()

	var contentBuf strings.Builder
	toolCallMap := make(map[int]*openai.ToolCall) // index → accumulated tool call

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("llm stream recv: %w", err)
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// Accumulate text content
		if delta.Content != "" {
			contentBuf.WriteString(delta.Content)
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
	}, nil
}
