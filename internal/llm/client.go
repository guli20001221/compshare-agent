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
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

// Client wraps go-openai to talk to ModelVerse (OpenAI-compatible).
type Client struct {
	client   *openai.Client
	model    string
	provider string
}

const maxChatAttempts = 2

func NewClient(cfg config.LLMConfig) *Client {
	ocfg := openai.DefaultConfig(cfg.APIKey)
	ocfg.BaseURL = cfg.BaseURL

	// A streaming response may legitimately run for minutes. http.Client.Timeout
	// covers reading the entire response body, so putting a fixed timeout here
	// turns a healthy long answer into a synthetic stream failure. The request
	// context supplied by the HTTP/WS owner remains the authoritative lifecycle
	// bound. The only special transport behavior here is the local proxy bypass.
	ocfg.HTTPClient = chatHTTPClient(cfg.BaseURL)

	return &Client{
		client:   openai.NewClientWithConfig(ocfg),
		model:    cfg.Model,
		provider: ProviderOpenAICompatible,
	}
}

func chatHTTPClient(baseURL string) *http.Client {
	// Leave Timeout at zero deliberately. Unlike a response-header or idle
	// timeout, Client.Timeout is a total deadline for opening *and reading* the
	// stream, which is the wrong policy for an agent that may reason or emit a
	// long answer. A future idle watchdog must be implemented as an explicit
	// stream-progress policy, not by reusing this total-duration field.
	client := &http.Client{}
	// Bypass HTTP proxy only for an actual loopback endpoint (the local LLM
	// proxy), never for a remote URL which merely happens to contain that text.
	if isLoopbackLLMEndpoint(baseURL) {
		client.Transport = &http.Transport{
			Proxy: nil, // no proxy for localhost
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
	}
	return client
}

func isLoopbackLLMEndpoint(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
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
	// StopReason is the provider's terminal finish_reason for the selected
	// choice. It is intentionally carried to the engine instead of being
	// inferred from an empty final chunk: a length-stopped response is not a
	// complete answer and must not be committed or allowed to execute a tool.
	StopReason string
	// ForcedToolChoiceDegraded is true when the request carried a forced
	// tool_choice ("required" or an object) that the provider rejected in
	// thinking mode, so Chat silently retried with auto (see Chat). The tool
	// calls (if any) then come from an UNFORCED call — a caller that depends on
	// the forcing being honored must treat this response as non-authoritative and
	// degrade, never score it as a structural guarantee. Callers that only used
	// forcing as an advisory optimization (SearchKnowledge / monitor) can ignore it.
	ForcedToolChoiceDegraded bool
}

// OutputIncomplete reports whether the provider says this choice failed to
// finish normally. Unknown non-empty reasons are deliberately fail-closed: an
// incomplete answer is unsafe to persist or execute as a tool plan, whereas a
// new provider spelling can always be added after observing it.
func (r ChatResponse) OutputIncomplete() bool {
	switch strings.ToLower(strings.TrimSpace(r.StopReason)) {
	case "", "stop", "tool_calls", "function_call":
		return false
	default:
		return true
	}
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
	// Some thinking-mode providers reject forced tool_choice. Retry only that
	// specific rejection with auto; absent-tool and other 4xx errors still fail.
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
		// A retry is one logical model response. Do not let a failed stream leak
		// a partial prefix to the caller and then append a successful retry behind
		// it: the engine persists only the retry's response, while the browser
		// would have displayed two incompatible answers. Buffer deltas per attempt
		// and publish them only after that attempt reaches EOF successfully.
		attemptReq := req
		var attemptDeltas []string
		if req.OnTextDelta != nil {
			attemptReq.OnTextDelta = func(delta string) {
				attemptDeltas = append(attemptDeltas, delta)
			}
		}
		resp, err := c.chatOnce(ctx, attemptReq, includeUsage)
		if err == nil {
			if req.OnTextDelta != nil {
				for _, delta := range attemptDeltas {
					req.OnTextDelta(delta)
				}
			}
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
	call := OutboundCall{Provider: c.provider, Model: c.model}
	observeOutboundCall(ctx, call)
	stream, err := c.client.CreateChatCompletionStream(ctx, ccReq)
	if err != nil {
		return nil, fmt.Errorf("llm stream: %w", err)
	}
	defer stream.Close()

	var contentBuf strings.Builder
	var usage TokenUsage
	var stopReason string
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
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			stopReason = string(choice.FinishReason)
		}
		delta := choice.Delta

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

	response := &ChatResponse{
		Content:    contentBuf.String(),
		ToolCalls:  toolCalls,
		Usage:      usage,
		StopReason: stopReason,
	}
	observeOutboundCallResult(ctx, OutboundCallResult{Call: call, StopReason: TraceFinishReason(response.StopReason)})
	return response, nil
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

// providerOverloadStatus recognizes retryable 429/5xx responses. Deterministic
// 4xx rejections are never retried.
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
