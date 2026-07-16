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

// HandleReadRequest is the temporary common lifecycle entry. The type switch
// is structural dispatch only: every branch immediately enters a method whose
// parameter is the concrete capability request type.
func (h *DemoHandler) HandleReadRequest(ctx context.Context, request ReadRequest, meta ReadHandlerContext) HandlerResult {
	switch typed := request.(type) {
	case MonitorHistoryRequest:
		return h.HandleMonitorHistoryRequest(ctx, typed, meta)
	case GPUSpecsRequest:
		return h.HandleGPUSpecsRequest(ctx, typed, meta)
	case StockAvailabilityRequest:
		return h.HandleStockAvailabilityRequest(ctx, typed, meta)
	case ImageListRequest:
		return h.HandleImageListRequest(ctx, typed, meta)
	case ImageTagCatalogRequest:
		return h.HandleImageTagCatalogRequest(ctx, typed, meta)
	case ModelRepositoryRequest:
		return h.HandleModelRepositoryRequest(ctx, typed, meta)
	case CFSListRequest:
		return h.HandleCFSListRequest(ctx, typed, meta)
	case CFSCreatePriceRequest:
		return h.HandleCFSCreatePriceRequest(ctx, typed, meta)
	case CFSUpgradePriceRequest:
		return h.HandleCFSUpgradePriceRequest(ctx, typed, meta)
	case CFSRefundEstimateRequest:
		return h.HandleCFSRefundEstimateRequest(ctx, typed, meta)
	default:
		result := FallbackBeforeTool(FallbackValidation)
		result.Reply = "unsupported typed read request"
		return result
	}
}

func typedHandlerRequest(readIntent Intent, slots Slots, meta ReadHandlerContext) HandlerRequest {
	return HandlerRequest{
		Plan:     IntentRoute{SchemaVersion: SchemaVersion, Intent: readIntent, Slots: slots, Confidence: 1},
		Resolver: meta.Resolver, FallbackInstanceID: meta.FallbackInstanceID, FallbackGpuModel: meta.FallbackGPUModel,
	}
}

func (h *DemoHandler) HandleMonitorHistoryRequest(ctx context.Context, request MonitorHistoryRequest, meta ReadHandlerContext) HandlerResult {
	return h.HandleMonitorQuery(ctx, typedHandlerRequest(IntentMonitorHistory, Slots{TargetRefs: request.Targets, Metrics: request.Metrics, TimeWindow: request.TimeWindow}, meta))
}
func (h *DemoHandler) HandleGPUSpecsRequest(ctx context.Context, request GPUSpecsRequest, meta ReadHandlerContext) HandlerResult {
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentGPUSpecsQuery, Slots{SearchQuery: request.GPUType, DetailLevel: request.DetailLevel}, meta))
}
func (h *DemoHandler) HandleStockAvailabilityRequest(ctx context.Context, request StockAvailabilityRequest, meta ReadHandlerContext) HandlerResult {
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentStockAvailability, Slots{SearchQuery: request.GPUType, Zone: request.Zone}, meta))
}
func (h *DemoHandler) HandleImageListRequest(ctx context.Context, request ImageListRequest, meta ReadHandlerContext) HandlerResult {
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentImageList, Slots{ImageSource: request.Source, SearchQuery: request.Query, ListMode: request.Mode}, meta))
}
func (h *DemoHandler) HandleImageTagCatalogRequest(ctx context.Context, request ImageTagCatalogRequest, meta ReadHandlerContext) HandlerResult {
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentImageTagCatalog, Slots{}, meta))
}
func (h *DemoHandler) HandleModelRepositoryRequest(ctx context.Context, request ModelRepositoryRequest, meta ReadHandlerContext) HandlerResult {
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentModelRepositoryBrowse, Slots{SearchQuery: request.Query, ListMode: request.Mode}, meta))
}
func (h *DemoHandler) HandleCFSListRequest(ctx context.Context, request CFSListRequest, meta ReadHandlerContext) HandlerResult {
	refs := []TargetRef(nil)
	if request.CFS != nil {
		refs = []TargetRef{{Type: TargetRefName, Value: request.CFS.ID, Source: SourceUserText}}
	}
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentCFSInfo, Slots{TargetRefs: refs, CFSKind: CFSKindList}, meta))
}
func (h *DemoHandler) HandleCFSCreatePriceRequest(ctx context.Context, request CFSCreatePriceRequest, meta ReadHandlerContext) HandlerResult {
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentCFSInfo, Slots{CFSKind: CFSKindCreatePrice, SizeGB: request.TargetSizeGB, Zone: request.Zone, ChargeType: request.ChargeType}, meta))
}
func (h *DemoHandler) HandleCFSUpgradePriceRequest(ctx context.Context, request CFSUpgradePriceRequest, meta ReadHandlerContext) HandlerResult {
	refs := []TargetRef{{Type: TargetRefName, Value: request.CFS.ID, Source: SourceUserText}}
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentCFSInfo, Slots{TargetRefs: refs, CFSKind: CFSKindUpgradePrice, SizeGB: request.TargetSizeGB}, meta))
}
func (h *DemoHandler) HandleCFSRefundEstimateRequest(ctx context.Context, request CFSRefundEstimateRequest, meta ReadHandlerContext) HandlerResult {
	refs := []TargetRef{{Type: TargetRefName, Value: request.CFS.ID, Source: SourceUserText}}
	return h.DispatchRoute(ctx, typedHandlerRequest(IntentCFSInfo, Slots{TargetRefs: refs, CFSKind: CFSKindRefund}, meta))
}
