package intent

import "context"

// ReadHandlerContext contains runtime-owned dependencies that are not model
// parameters. Capability handlers receive their own request type plus this
// context; they never receive the user's raw sentence.
type ReadHandlerContext struct {
	Resolver           EntityResolver
	FallbackInstanceID string
	FallbackGPUModel   string
}

// HandleReadRequest is the legacy typed read-tool lifecycle entry. Every
// model-visible read capability now owns a typed vertical (capability.MigratedRead),
// so the engine dispatches through the kernel and this method is unreachable on
// the live path. It is retained as a compiling stub only until P3.5 deletes the
// legacy read dispatch (executeReadCapability / invokeReadHandler) wholesale.
func (h *DemoHandler) HandleReadRequest(ctx context.Context, request ReadRequest, meta ReadHandlerContext) HandlerResult {
	result := FallbackBeforeTool(FallbackValidation)
	result.Reply = "unsupported typed read request"
	return result
}
