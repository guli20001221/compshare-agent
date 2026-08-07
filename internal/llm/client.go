package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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
	Messages       []openai.ChatCompletionMessage
	Tools          []openai.Tool
	ResponseFormat *openai.ChatCompletionResponseFormat
	// ToolChoice forces tool selection when non-nil. Accepts either a
	// string ("auto"/"required"/"none") or an openai.ToolChoice struct
	// naming a specific function. Leave nil for default auto behavior.
	ToolChoice any
	// Temperature pins the sampling temperature for this call. nil leaves the
	// field off the wire entirely, so every existing caller keeps the provider
	// default and this addition changes no current behaviour.
	//
	// Set it only for a caller whose contract explicitly benefits from a fixed
	// sampling value. It is not a general reproducibility guarantee.
	Temperature *float32
	// OnTextDelta, if non-nil, is invoked synchronously for each non-empty
	// text delta chunk received from the upstream stream.
	OnTextDelta func(string)
}

// ChatResponse wraps the LLM output.
type ChatResponse struct {
	Content   string
	ToolCalls []openai.ToolCall
	Usage     TokenUsage
	// ForcedToolChoiceDegraded is true when the request carried a forced
	// tool_choice ("required" or an object) that the provider rejected in
	// thinking mode, so Chat silently retried with auto (see Chat). The tool
	// calls (if any) then come from an UNFORCED call — a caller that depends on
	// the forcing being honored must treat this response as non-authoritative and
	// degrade, never score it as a structural guarantee. Callers that only used
	// forcing as an advisory optimization (SearchKnowledge / monitor) can ignore it.
	ForcedToolChoiceDegraded bool
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
		resp, err = c.chat(ctx, req, false)
	}
	// Graceful degradation for forced tool_choice. On Modelverse the
	// deepseek-v4-flash deployment's support for object/"required" tool_choice in
	// THINKING mode is per-KEY: some keys honor a forced choice, others return
	// HTTP 400 ("tool_choice ... does not support being set to required or object
	// in thinking mode") — verified by direct probe 2026-06-10 (same model + request,
	// only the API key differs). Forcing a tool is only ever an advisory optimization
	// here: engine callers that force SearchKnowledge / a monitor tool also inject a
	// system note naming it, so retry once with auto instead of failing the turn. The
	// note keeps the intended tool highly likely. Scoped to the thinking-mode message
	// so an absent-tool 400 ("no function named X in tools") does NOT trigger it.
	if err != nil && isForcedToolChoice(req.ToolChoice) && isForcedToolChoiceUnsupportedError(err) {
		log.Printf("runtime: upstream rejected forced tool_choice in thinking mode; retrying with auto (configure a forced-tool-capable LLM key for deterministic forcing)")
		auto := req
		auto.ToolChoice = nil
		resp, err = c.chat(ctx, auto, true)
		if err != nil && isUsageUnsupportedChatError(err) {
			resp, err = c.chat(ctx, auto, false)
		}
		// Signal the silent degrade so a caller that relied on the forcing being
		// honored can fall back instead of trusting an unforced response.
		if err == nil && resp != nil {
			resp.ForcedToolChoiceDegraded = true
		}
	}
	return resp, err
}

// isForcedToolChoice reports whether tc forces a specific tool — "required" or an
// object {type:function,function:{name}}. nil / "auto" / "none" are not forced.
func isForcedToolChoice(tc any) bool {
	switch v := tc.(type) {
	case nil:
		return false
	case string:
		return v == "required"
	default:
		return true // openai.ToolChoice struct (object) names a function
	}
}

// isForcedToolChoiceUnsupportedError matches the Modelverse rejection of forced
// tool_choice in thinking mode. Scoped narrowly to that message so a generic
// tool_choice error (e.g. "no function named X in tools") does NOT trigger the
// auto fallback (mirrors TestClientChatDoesNotRetryProviderStatusError).
func isForcedToolChoiceUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "tool_choice") && strings.Contains(msg, "thinking mode")
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
		// Only pause when another attempt actually follows — sleeping before
		// returning the final error just delays the user's error by a second.
		if attempt+1 >= maxChatAttempts {
			break
		}
		if _, overloaded := providerOverloadStatus(err); overloaded {
			select {
			case <-ctx.Done():
				return nil, err
			case <-time.After(providerOverloadBackoff):
			}
		}
	}
	return nil, lastErr
}

// wireTemperature makes a requested temperature survive serialization.
//
// go-openai tags ChatCompletionRequest.Temperature as `json:"temperature,omitempty"`,
// so a literal 0 is dropped from the request body and the provider applies its
// own default — silently giving the caller the OPPOSITE of the pinned sampling
// they asked for. The smallest positive float32 serializes, and is
// indistinguishable from 0 as a sampling temperature.
func wireTemperature(requested float32) float32 {
	if requested == 0 {
		return math.SmallestNonzeroFloat32
	}
	return requested
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
	if req.ResponseFormat != nil {
		ccReq.ResponseFormat = req.ResponseFormat
	}
	if req.ToolChoice != nil {
		ccReq.ToolChoice = req.ToolChoice
	}
	if req.Temperature != nil {
		ccReq.Temperature = wireTemperature(*req.Temperature)
	}

	// Count at the last boundary before the SDK attempts the upstream request.
	// Putting this in Chat or chat would miss internal retries or count logical
	// calls that never became requests.
	observeOutboundCall(ctx, OutboundCall{Model: c.model})
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

// providerOverloadStatus reports the upstream HTTP status when the provider
// answered "not now" rather than "your request is wrong": 429 (rate/quota) and
// the 5xx family the ModelVerse relay returns when its account pool is
// momentarily empty. Measured 2026-07-29: a real turn died on
// `503 ... No available accounts`, and a direct probe with the same key and the
// same model answered 200 minutes later — the request was never the problem.
//
// A 4xx deliberately does NOT match. Retrying a deterministic rejection just
// pays for it twice, which is what TestClientChatDoesNotRetryProviderStatusError
// pins (400). Both go-openai error shapes carry the status: APIError for a
// decoded provider error body, RequestError for the streaming path, which is
// where the 503 above surfaced (client.go wraps it with %w, so errors.As sees
// through).
func providerOverloadStatus(err error) (int, bool) {
	status := 0
	var apiErr *openai.APIError
	var reqErr *openai.RequestError
	switch {
	case errors.As(err, &apiErr):
		status = apiErr.HTTPStatusCode
	case errors.As(err, &reqErr):
		status = reqErr.HTTPStatusCode
	default:
		return 0, false
	}
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return status, true
	}
	return status, false
}

// providerOverloadBackoff is the pause before re-sending a request the provider
// refused for capacity. An immediate retry mostly re-hits the same exhausted
// pool; a short wait is what makes the second attempt worth making at all. Kept
// well under the confirmation timeout (60s) so a retried turn still lands
// inside the card the user is looking at.
const providerOverloadBackoff = 900 * time.Millisecond

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
	// Status classification precedes the message match on purpose: a 4xx whose
	// prose happens to contain "timeout" ("request timeout is not a valid
	// parameter") is a rejection, not a transient, and must not be retried.
	if status, overloaded := providerOverloadStatus(err); status != 0 {
		return overloaded
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "tls handshake timeout") ||
		strings.Contains(msg, "timeout")
}
