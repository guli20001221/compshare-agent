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

// OutboundCallResult describes a successfully completed upstream model
// response. Failed transport attempts have no provider finish_reason and are
// represented by OutboundCall alone; this keeps call counts honest without
// inventing a terminal reason for a request that never completed.
type OutboundCallResult struct {
	Call       OutboundCall
	StopReason string
}

// OutboundCallResultObserver receives the provider's terminal finish_reason
// for every successful upstream response. It deliberately carries neither
// prompts nor response content.
type OutboundCallResultObserver func(OutboundCallResult)

// TraceFinishReason reduces a provider's free-form finish_reason to the
// protocol values this client recognizes. Trace is an aggregation boundary,
// not a place to preserve a provider's arbitrary diagnostic string; new
// non-empty spellings remain visible as "other" and still fail closed in
// ChatResponse.OutputIncomplete. A successful response that omitted a native
// finish_reason is recorded as "unspecified"; it is deliberately distinct
// from a transport failure, which produces no OutboundCallResult at all.
func TraceFinishReason(reason string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(reason)); normalized {
	// A standard JSON null decodes to the zero value of the SDK's string alias,
	// so an empty value means this successful response supplied no terminal
	// reason. A literal "null" is instead a non-standard, non-empty provider
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

// WithOutboundCallResultObserver adds a successful-response observer without
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
