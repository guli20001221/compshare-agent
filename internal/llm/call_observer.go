package llm

import (
	"context"
	"strings"
)

// ProviderOpenAICompatible identifies the protocol adapter, not a hostname or
// a commercial vendor. The client intentionally records this stable family
// rather than guessing an upstream provider from deployment configuration.
const ProviderOpenAICompatible = "openai_compatible"

// OutboundCall describes one request handed to the OpenAI-compatible client.
// It deliberately carries no prompt, tool arguments, or response content.
type OutboundCall struct {
	// Provider is a closed, non-secret endpoint family (for example
	// "modelverse" or "openai_compatible"). It deliberately is not the raw
	// base URL: trace records need to group provider behavior without retaining
	// deployment topology or credentials.
	Provider string
	Model    string
}

// OutboundCallObserver is invoked once for every actual upstream request,
// including retries and requests that fail before a stream is established.
type OutboundCallObserver func(OutboundCall)

const (
	OutboundAttemptSuccess = "success"
	OutboundAttemptError   = "error"

	OutboundErrorCancelled   = "cancelled"
	OutboundErrorDeadline    = "deadline_exceeded"
	OutboundErrorRateLimited = "rate_limited"
	OutboundErrorUpstream4xx = "upstream_4xx"
	OutboundErrorUpstream5xx = "upstream_5xx"
	OutboundErrorNetwork     = "network"
	OutboundErrorStream      = "stream"
	OutboundErrorOther       = "other"
)

// OutboundCallResult describes the terminal observation for one actual
// upstream request, successful or failed. It carries no prompt, response, URL,
// or provider diagnostic text.
type OutboundCallResult struct {
	Call OutboundCall
	// AttemptInCall is 1-based within one Client.Chat invocation. A turn may
	// contain several calls, so this is deliberately not named as a turn-global
	// sequence number.
	AttemptInCall        int
	LatencyMS            int64
	Outcome              string
	ErrorClass           string
	Retried              bool
	StopReason           string
	ProviderFirstChunkMS *int64
	// PromptTokens is nil when this attempt returned no usage block. A pointer
	// to zero is a real provider-reported value, distinct from missing usage.
	PromptTokens *int
	// CachedPromptTokens is nil when prompt-token details were absent. A pointer
	// to zero means a details object was present and decoded to zero cached tokens.
	CachedPromptTokens *int
	// ToolCount and ToolWindowRunes describe the final tool array handed to the
	// SDK for this exact attempt. Both remain explicit zeroes for a tool-free
	// request so new traces distinguish that shape from legacy missing fields.
	ToolCount       int
	ToolWindowRunes int
	// ToolWindowHash is an order-sensitive SHA-256 of the serialized tool array.
	// It is empty for a tool-free request or if the array could not be serialized.
	ToolWindowHash string
}

// OutboundCallResultObserver receives one terminal record for every actual
// attempt. A failed attempt has no StopReason; an attempt that yielded no
// provider chunk has a nil ProviderFirstChunkMS.
type OutboundCallResultObserver func(OutboundCallResult)

// TraceFinishReason reduces a provider's free-form finish_reason to the
// protocol values this client recognizes. Trace is an aggregation boundary,
// not a place to preserve a provider's arbitrary diagnostic string; new
// non-empty spellings remain visible as "other" and still fail closed in
// ChatResponse.OutputIncomplete. The "unspecified" mapping remains available
// for legacy records and non-stream response producers; the streaming client
// rejects a missing native reason before recording success. A failed attempt
// carries no StopReason.
func TraceFinishReason(reason string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(reason)); normalized {
	// A standard JSON null decodes to the zero value of the SDK's string alias,
	// so an empty value means no terminal reason was supplied. A literal
	// "null" is instead a non-standard, non-empty provider
	// spelling and intentionally falls through to "other" below.
	case "":
		return "unspecified"
	case "stop", "tool_calls", "function_call", "length", "max_tokens", "content_filter":
		return normalized
	default:
		return "other"
	}
}

type outboundCallObserverKey struct{}
type outboundCallResultObserverKey struct{}

// WithOutboundCallObserver adds an observer without discarding one already
// attached by an outer layer. The callback runs synchronously at the outbound
// request boundary so the count is complete before the enclosing turn exits.
func WithOutboundCallObserver(ctx context.Context, observer OutboundCallObserver) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	if previous, ok := ctx.Value(outboundCallObserverKey{}).(OutboundCallObserver); ok && previous != nil {
		current := observer
		observer = func(call OutboundCall) {
			previous(call)
			current(call)
		}
	}
	return context.WithValue(ctx, outboundCallObserverKey{}, observer)
}

func observeOutboundCall(ctx context.Context, call OutboundCall) {
	if ctx == nil {
		return
	}
	if observer, ok := ctx.Value(outboundCallObserverKey{}).(OutboundCallObserver); ok && observer != nil {
		observer(call)
	}
}

// WithOutboundCallResultObserver adds a terminal-attempt observer without
// discarding one already attached by an outer layer.
func WithOutboundCallResultObserver(ctx context.Context, observer OutboundCallResultObserver) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	if previous, ok := ctx.Value(outboundCallResultObserverKey{}).(OutboundCallResultObserver); ok && previous != nil {
		current := observer
		observer = func(result OutboundCallResult) {
			previous(result)
			current(result)
		}
	}
	return context.WithValue(ctx, outboundCallResultObserverKey{}, observer)
}

func observeOutboundCallResult(ctx context.Context, result OutboundCallResult) {
	if ctx == nil {
		return
	}
	if observer, ok := ctx.Value(outboundCallResultObserverKey{}).(OutboundCallResultObserver); ok && observer != nil {
		observer(result)
	}
}
