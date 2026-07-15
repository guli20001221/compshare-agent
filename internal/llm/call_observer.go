package llm

import "context"

// OutboundCall describes one request handed to the OpenAI-compatible client.
// It deliberately carries no prompt, tool arguments, or response content.
type OutboundCall struct {
	Model string
}

// OutboundCallObserver is invoked once for every actual upstream request,
// including retries and requests that fail before a stream is established.
type OutboundCallObserver func(OutboundCall)

type outboundCallObserverKey struct{}

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
