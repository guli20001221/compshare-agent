package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"sync/atomic"
	"time"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

// Client wraps go-openai to talk to ModelVerse (OpenAI-compatible).
type Client struct {
	client        *openai.Client
	model         string
	fallbackModel string
	provider      string
	// primaryUnhealthyUntil is a UnixNano instant. While it lies in the future a
	// call starts on the fallback model: the gateway fails slowly and per model
	// pool, so a call that already paid that wait must not make the next call
	// pay it again just to rediscover the same outage.
	primaryUnhealthyUntil atomic.Int64
}

// maxChatAttempts bounds the actual requests one Chat call makes for one
// response, not counting the re-sends that only narrow the request shape.
const maxChatAttempts = 2

// primaryCooldown is how long a call skips the primary model after a request
// to it failed upstream. Long enough that a turn with several model calls
// pays the gateway's wait once, short enough that a healthy primary is back
// in use within minutes without an operator action.
const primaryCooldown = 5 * time.Minute

func NewClient(cfg config.LLMConfig) *Client {
	ocfg := openai.DefaultConfig(cfg.APIKey)
	ocfg.BaseURL = cfg.BaseURL

	// A streaming response may legitimately run for minutes. http.Client.Timeout
	// covers reading the entire response body, so putting a fixed timeout here
	// turns a healthy long answer into a synthetic stream failure. The request
	// context supplied by the HTTP/WS owner remains the authoritative lifecycle
	// bound. The only special transport behavior here is the local proxy bypass.
	ocfg.HTTPClient = chatHTTPClient(cfg.BaseURL)

	client := &Client{
		client:   openai.NewClientWithConfig(ocfg),
		model:    cfg.Model,
		provider: ProviderOpenAICompatible,
	}
	if fallback := strings.TrimSpace(cfg.FallbackModel); fallback != "" && fallback != cfg.Model {
		client.fallbackModel = fallback
	}
	return client
}

// modelOrder is the preference order for one call: the primary first unless a
// recent request to it failed upstream, in which case the fallback goes first
// and the primary remains the last resort.
func (c *Client) modelOrder(now time.Time) []string {
	if c.fallbackModel == "" {
		return []string{c.model}
	}
	if now.UnixNano() < c.primaryUnhealthyUntil.Load() {
		return []string{c.fallbackModel, c.model}
	}
	return []string{c.model, c.fallbackModel}
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
//
// One call makes at most maxChatAttempts actual requests. With a fallback
// model configured the second request goes to the other model rather than
// back to the pool that just failed; a primary that failed upstream is then
// skipped by later calls for primaryCooldown. Every actual request is
// observed with the model it went to, so the trace shows the switch.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	order := c.modelOrder(time.Now())
	shape := requestShape{includeUsage: true}
	attempt := 0
	var lastErr error
	for slot := 0; slot < maxChatAttempts; slot++ {
		model := order[slot%len(order)]
		resp, failed := c.chatWithModel(ctx, req, model, &shape, &attempt)
		if failed == nil {
			return resp, nil
		}
		lastErr = failed.err
		if !isTransientChatError(ctx, failed.err) {
			observeOutboundCallResult(ctx, failed.record(false))
			return nil, failed.err
		}
		if model == c.model && c.fallbackModel != "" {
			c.primaryUnhealthyUntil.Store(time.Now().Add(primaryCooldown).UnixNano())
		}
		// Only pause when another attempt actually follows — sleeping before
		// returning the final error just delays the user's error by a second.
		if slot+1 >= maxChatAttempts {
			observeOutboundCallResult(ctx, failed.record(false))
			break
		}
		next := order[(slot+1)%len(order)]
		if next != model {
			log.Printf("runtime: %s failed upstream (%s); routing this call to %s", model, failed.errorClass, next)
		} else if _, overloaded := providerOverloadStatus(failed.err); overloaded {
			// Re-sending to the same pool: an immediate retry mostly re-hits the
			// same exhausted pool, a short wait is what makes it worth making.
			select {
			case <-ctx.Done():
				observeOutboundCallResult(ctx, failed.record(false))
				return nil, failed.err
			case <-time.After(providerOverloadBackoff):
			}
		}
		// The next request begins immediately after this point. In the overload
		// case the observation is deliberately delayed until after the
		// cancellable backoff, so Retried never claims a request that did not run.
		observeOutboundCallResult(ctx, failed.record(true))
	}
	return nil, lastErr
}

// requestShape is what the endpoint has been found to accept. Both narrowings
// are deterministic properties of the endpoint, not of one request, so a
// shape learned on one attempt is kept for the requests that follow it.
type requestShape struct {
	// includeUsage asks for stream_options.include_usage.
	includeUsage bool
	// forcingDegraded sends tool_choice as auto although the caller forced a
	// tool, after the provider rejected forcing in thinking mode.
	forcingDegraded bool
}

// chatWithModel makes the requests for one attempt slot on one model: the
// request as currently shaped, re-sent with a narrower shape when the provider
// rejects stream_options or a forced tool_choice. The terminal failure is
// returned unobserved because the caller decides whether another request
// follows before recording it.
func (c *Client) chatWithModel(ctx context.Context, req ChatRequest, model string, shape *requestShape, attempt *int) (*ChatResponse, *failedAttempt) {
	for {
		resp, failed := c.attemptOnce(ctx, req, model, *shape, attempt)
		if failed == nil {
			// Signal the silent degrade so a caller that relied on the forcing
			// being honored can fall back instead of trusting an unforced response.
			resp.ForcedToolChoiceDegraded = shape.forcingDegraded
			return resp, nil
		}
		switch {
		case shape.includeUsage && isUsageUnsupportedChatError(failed.err):
			shape.includeUsage = false
		// Some thinking-mode providers reject forced tool_choice. Retry only that
		// specific rejection with auto; absent-tool and other 4xx errors still fail.
		case !shape.forcingDegraded && isForcedToolChoice(req.ToolChoice) && isForcedToolChoiceUnsupportedError(failed.err):
			log.Printf("runtime: upstream rejected forced tool_choice in thinking mode; retrying with auto (configure a forced-tool-capable LLM key for deterministic forcing)")
			shape.forcingDegraded = true
		default:
			return nil, failed
		}
		observeOutboundCallResult(ctx, failed.record(true))
	}
}

// failedAttempt is one actual request that ended in an error, held until the
// caller knows whether a further request follows it.
type failedAttempt struct {
	err        error
	errorClass string
	result     OutboundCallResult
}

func (f *failedAttempt) record(retried bool) OutboundCallResult {
	result := f.result
	result.Retried = retried
	return result
}

// attemptOnce makes one actual request. Deltas are buffered per attempt and
// published only after a terminal choice reason and a clean EOF: the engine
// persists only the response a later request produces, so a failed stream
// must never leak a partial prefix ahead of it.
func (c *Client) attemptOnce(ctx context.Context, req ChatRequest, model string, shape requestShape, attempt *int) (*ChatResponse, *failedAttempt) {
	attemptReq := req
	if shape.forcingDegraded {
		attemptReq.ToolChoice = nil
	}
	var attemptDeltas []string
	if req.OnTextDelta != nil {
		attemptReq.OnTextDelta = func(delta string) {
			attemptDeltas = append(attemptDeltas, delta)
		}
	}
	*attempt++
	resp, timing, err := c.chatOnce(ctx, attemptReq, model, shape.includeUsage)
	result := OutboundCallResult{
		Call: OutboundCall{Provider: c.provider, Model: model}, AttemptInCall: *attempt,
		LatencyMS: timing.latencyMS, ProviderFirstChunkMS: timing.firstChunkMS,
		PromptTokens: timing.promptTokens, CachedPromptTokens: timing.cachedPromptTokens,
		ToolCount: timing.toolCount, ToolWindowRunes: timing.toolWindowRunes, ToolWindowHash: timing.toolWindowHash,
	}
	if err != nil {
		result.Outcome = OutboundAttemptError
		result.ErrorClass = traceOutboundErrorClass(ctx, err)
		return nil, &failedAttempt{err: err, errorClass: result.ErrorClass, result: result}
	}
	result.Outcome = OutboundAttemptSuccess
	result.StopReason = TraceFinishReason(resp.StopReason)
	observeOutboundCallResult(ctx, result)
	for _, delta := range attemptDeltas {
		req.OnTextDelta(delta)
	}
	return resp, nil
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

type outboundAttemptTiming struct {
	latencyMS          int64
	firstChunkMS       *int64
	promptTokens       *int
	cachedPromptTokens *int
	toolCount          int
	toolWindowRunes    int
	toolWindowHash     string
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

func (c *Client) chatOnce(ctx context.Context, req ChatRequest, model string, includeUsage bool) (response *ChatResponse, timing outboundAttemptTiming, err error) {
	ccReq := openai.ChatCompletionRequest{
		Model:    model,
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

	// Measure the final tool array at the last boundary before the SDK serializes
	// the request. Hashing the ordered JSON is deliberate: prompt caches match an
	// exact prefix, so reordering otherwise-identical tools is a different window.
	timing.toolCount, timing.toolWindowRunes, timing.toolWindowHash = observeToolWindow(ccReq.Tools)

	// Count at the last boundary before the SDK attempts the upstream request.
	// Putting this in Chat would miss internal retries or count logical calls
	// that never became requests.
	call := OutboundCall{Provider: c.provider, Model: model}
	started := time.Now()
	defer func() { timing.latencyMS = time.Since(started).Milliseconds() }()
	observeOutboundCall(ctx, call)
	stream, err := c.client.CreateChatCompletionStream(ctx, ccReq)
	if err != nil {
		return nil, timing, fmt.Errorf("llm stream: %w", err)
	}
	defer stream.Close()

	var contentBuf strings.Builder
	var usage TokenUsage
	var stopReason string
	toolCallMap := make(map[int]*openai.ToolCall) // index → accumulated tool call

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// The SDK maps both a bare HTTP EOF and [DONE] to io.EOF. Only a
			// terminal choice reason proves that the accumulated answer or tool
			// plan finished; an interrupted prefix must never become a response.
			if strings.TrimSpace(stopReason) == "" {
				return nil, timing, fmt.Errorf("llm stream ended without finish_reason: %w", io.ErrUnexpectedEOF)
			}
			break
		}
		if err != nil {
			return nil, timing, fmt.Errorf("llm stream recv: %w", err)
		}
		if timing.firstChunkMS == nil {
			firstChunkMS := time.Since(started).Milliseconds()
			timing.firstChunkMS = &firstChunkMS
		}

		if chunk.Usage != nil {
			usage = TokenUsage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
			// A later usage block is authoritative, including the absence of
			// data or details. Do not retain data from an earlier chunk. An empty
			// JSON usage object decodes to the SDK's all-zero struct; it is absence,
			// not a provider-reported zero prompt. A real zero prompt remains
			// observable when another usage field or the details block is present.
			timing.promptTokens = nil
			timing.cachedPromptTokens = nil
			if usageBlockObserved(chunk.Usage) {
				promptTokens := chunk.Usage.PromptTokens
				timing.promptTokens = &promptTokens
				if chunk.Usage.PromptTokensDetails != nil {
					cachedPromptTokens := chunk.Usage.PromptTokensDetails.CachedTokens
					timing.cachedPromptTokens = &cachedPromptTokens
				}
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

	response = &ChatResponse{
		Content:    contentBuf.String(),
		ToolCalls:  toolCalls,
		Usage:      usage,
		StopReason: stopReason,
	}
	return response, timing, nil
}

// observeToolWindow returns content-free measurements of the exact ordered
// tool array supplied to the SDK. It never changes request behavior: if a tool
// schema is not JSON-serializable, the SDK remains responsible for returning
// the request error and the unavailable size/hash stay empty.
func observeToolWindow(tools []openai.Tool) (count, runes int, hash string) {
	count = len(tools)
	if count == 0 {
		return count, 0, ""
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return count, 0, ""
	}
	sum := sha256.Sum256(raw)
	return count, len([]rune(string(raw))), "sha256:" + hex.EncodeToString(sum[:])
}

func usageBlockObserved(usage *openai.Usage) bool {
	return usage != nil && (usage.PromptTokens != 0 || usage.CompletionTokens != 0 || usage.TotalTokens != 0 ||
		usage.PromptTokensDetails != nil || usage.CompletionTokensDetails != nil)
}

// traceOutboundErrorClass reduces typed transport/provider failures to a closed
// set. It deliberately never inspects err.Error().
func traceOutboundErrorClass(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return OutboundErrorCancelled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return OutboundErrorDeadline
	}
	status, _ := providerStatus(err)
	switch {
	case status == http.StatusTooManyRequests:
		return OutboundErrorRateLimited
	case status >= 500:
		return OutboundErrorUpstream5xx
	case status >= 400:
		return OutboundErrorUpstream4xx
	}
	var netErr net.Error
	switch {
	case errors.As(err, &netErr):
		return OutboundErrorNetwork
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return OutboundErrorStream
	default:
		return OutboundErrorOther
	}
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

// providerStatus is the HTTP status of a typed provider error. An error event
// the provider writes into an already-open stream carries no status — go-openai
// surfaces it as an *APIError with HTTPStatusCode 0 — so its OpenAI error type
// is read as the status the provider sends for the same condition before a
// stream opens. Every classifier here then reasons in one vocabulary; none of
// them inspects the message text.
func providerStatus(err error) (int, bool) {
	var apiErr *openai.APIError
	var reqErr *openai.RequestError
	switch {
	case errors.As(err, &apiErr):
		if apiErr.HTTPStatusCode == 0 {
			return streamErrorEventStatus(apiErr.Type), true
		}
		return apiErr.HTTPStatusCode, true
	case errors.As(err, &reqErr):
		return reqErr.HTTPStatusCode, reqErr.HTTPStatusCode != 0
	}
	return 0, false
}

// streamErrorEventStatus maps the OpenAI error type of an in-stream error
// event to its pre-stream status. The types that describe the request or the
// caller are the closed set below; anything else — rate limits aside — is the
// provider failing to produce the response, which is what a 503 says.
func streamErrorEventStatus(errorType string) int {
	switch strings.ToLower(strings.TrimSpace(errorType)) {
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "insufficient_quota":
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

// providerOverloadStatus recognizes retryable 429/5xx responses, whether they
// arrive as the HTTP status or as an error event inside a 200 stream.
// Deterministic 4xx rejections are never retried.
func providerOverloadStatus(err error) (int, bool) {
	status, ok := providerStatus(err)
	if !ok {
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
// well under the human confirmation window so a retried turn still lands
// inside the card the user is looking at.
const providerOverloadBackoff = 900 * time.Millisecond

// isTransientChatError reports whether the failure belongs to the upstream
// rather than to the request or the caller, so that another request — to the
// same model after a pause, or to the fallback model — is worth making.
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
