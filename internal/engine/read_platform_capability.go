package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/platform"
)

// ReadCapabilityObservation is the only result shape exposed by the read
// capability adapter. Control-flow states stay distinct and factual answers
// must cross an evidence envelope before the Agent can consume them.
type ReadCapabilityObservation struct {
	Capability       string                     `json:"capability"`
	Status           intent.HandlerStatus       `json:"status"`
	FailureClass     intent.HandlerFailureClass `json:"failure_class,omitempty"`
	FallbackReason   intent.FallbackReason      `json:"fallback_reason,omitempty"`
	RouteStatus      intent.RouteStatus         `json:"route_status,omitempty"`
	ToolAction       string                     `json:"tool_action,omitempty"`
	Envelope         *envelope.Envelope         `json:"evidence,omitempty"`
	Guidance         string                     `json:"guidance,omitempty"`
	RenderRef        string                     `json:"render_ref,omitempty"`
	RenderContract   string                     `json:"render_contract,omitempty"`
	CanAssertAbsence bool                       `json:"can_assert_absence"`
	MissingFields    []capability.MissingField  `json:"missing_fields,omitempty"`
}

func (e *Engine) executeConcreteReadCapability(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	readIntent, request, err := capability.DecodeReadRequest(action, args)
	if err != nil {
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: err.Error()})
		return marshalReadCapabilityError(fmt.Errorf("invalid capability request: %w", err))
	}
	if missing := request.MissingFields(); len(missing) > 0 {
		observation := ReadCapabilityObservation{Capability: string(readIntent), Status: intent.HandlerStatusNeedsInput, MissingFields: missing}
		payload, _ := json.Marshal(observation)
		onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "查询参数不完整", TraceResult: map[string]any{"status": string(intent.HandlerStatusNeedsInput), "missing_fields": missing}})
		return string(payload)
	}
	reg, ok := capability.MigratedRead(action)
	if !ok {
		// Every model-visible read owns a typed vertical (P3.3/P3.4). A decoded read
		// tool with no migrated capability is a wiring bug, not a runtime state — the
		// legacy intent.Slots read dispatch (invokeReadHandler → HandleReadRequest →
		// DispatchRoute) has been cut from the live path. The intent-side dead code is
		// deleted in P6.
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: "unmigrated read capability"})
		return marshalReadCapabilityError(fmt.Errorf("read capability %q has no typed vertical", action))
	}
	return e.executeTypedReadCapability(ctx, action, string(readIntent), reg, request, args, onStep)
}

// executeTypedReadCapability dispatches a migrated read through its typed
// vertical. The capability produces a capability.ReadResult (never intent.Slots
// / HandlerResult / DispatchRoute); the engine bridges it to a HandlerResult
// only to serialise the observation identically to the legacy path.
func (e *Engine) executeTypedReadCapability(ctx context.Context, action, capabilityLabel string, reg capability.RegisteredRead, request intent.ReadRequest, args map[string]any, onStep func(StepEvent)) string {
	onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: args})

	snapshot := e.RegistrySnapshot()
	rt := capability.ReadRuntime{
		Executor:         plannerHandlerExecutor{engine: e, onStep: onStep},
		Resolver:         snapshot,
		FallbackGPUModel: e.fallbackStockGpuModel(time.Now()),
	}
	if e.sessionStateHydrated && e.sessionState.SelectedInstanceID != "" {
		rt.FallbackInstanceID = e.sessionState.SelectedInstanceID
	}
	readResult := reg.Run(ctx, request, rt)
	e.applyReadEffects(readResult.Effects)
	if readResult.Status == platform.ReadStatusUnavailable {
		return e.buildUnavailableObservation(action, capabilityLabel, readResult, onStep)
	}
	return e.buildReadObservation(action, capabilityLabel, handlerResultFromReadResult(readResult), snapshot.CanAssertAbsence(), onStep)
}

// UnavailableCapabilityObservation is the structured result of an Unavailable
// capability: a deliberately-unsupported real-time capability answered with a
// deterministic explanation + supported alternatives, never a fabricated number.
type UnavailableCapabilityObservation struct {
	Capability   string   `json:"capability"`
	Status       string   `json:"status"`
	Reason       string   `json:"reason"`
	Alternatives []string `json:"alternatives,omitempty"`
}

// buildUnavailableObservation serialises an Unavailable read outcome. It stays
// off the read-observation evidence path: there is no envelope and no render_ref
// (nothing was retrieved), just the deterministic reason the model relays and the
// alternatives it can offer.
func (e *Engine) buildUnavailableObservation(action, capabilityLabel string, r capability.ReadResult, onStep func(StepEvent)) string {
	observation := UnavailableCapabilityObservation{
		Capability:   capabilityLabel,
		Status:       string(platform.ReadStatusUnavailable),
		Reason:       r.Reply,
		Alternatives: r.Alternatives,
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return marshalReadCapabilityError(err)
	}
	var traceResult map[string]any
	_ = json.Unmarshal(payload, &traceResult)
	onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "能力当前不可用", TraceResult: traceResult})
	return string(payload)
}

// buildReadObservation serialises a read outcome into the single read-tool
// observation shape shared by the legacy and typed dispatch paths.
func (e *Engine) buildReadObservation(action, capabilityLabel string, result intent.HandlerResult, canAssertAbsence bool, onStep func(StepEvent)) string {
	result = e.contextEnvelopeForPlainDirectReply(result)
	if result.Envelope != nil {
		if e.readCapabilitySubjectsThisTurn == nil {
			e.readCapabilitySubjectsThisTurn = map[string]struct{}{}
		}
		for _, subject := range result.Envelope.Subjects {
			if subject.ID != "" {
				e.readCapabilitySubjectsThisTurn[subject.ID] = struct{}{}
			}
		}
	}
	observation := ReadCapabilityObservation{
		Capability:       capabilityLabel,
		Status:           result.Status,
		FailureClass:     result.FailureClass,
		FallbackReason:   result.FallbackReason,
		RouteStatus:      result.RouteStatus,
		ToolAction:       result.ToolAction,
		Envelope:         result.Envelope,
		CanAssertAbsence: canAssertAbsence,
	}
	if result.Status == intent.HandlerStatusHandled && !result.NeedsClarification && result.Envelope != nil && strings.TrimSpace(result.Reply) != "" {
		placeholder := fmt.Sprintf("{{READ_OBSERVATION_%d}}", len(e.readResponseEvidenceThisTurn)+1)
		e.readResponseEvidenceThisTurn = append(e.readResponseEvidenceThisTurn, readResponseEvidence{
			Capability:  capabilityLabel,
			Reply:       strings.TrimSpace(result.Reply),
			Envelope:    *result.Envelope,
			Placeholder: placeholder,
		})
		observation.RenderRef = placeholder
		observation.RenderContract = "需要展示这份观察中的精确标识、数量、价格、库存、规格或状态时，在最终自然回答中原样插入 render_ref；服务端会替换为确定性结果。其他文字可自然解释，不要自行誊写精确字段。"
	}
	if result.Status != intent.HandlerStatusHandled || result.NeedsClarification {
		observation.Guidance = result.Reply
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return marshalReadCapabilityError(err)
	}
	var traceResult map[string]any
	_ = json.Unmarshal(payload, &traceResult)
	onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "查询完成", TraceResult: traceResult})
	return string(payload)
}

// handlerResultFromReadResult bridges a typed capability.ReadResult into the
// legacy HandlerResult shape used only for observation serialisation. RouteStatus
// is derived from the read status/fallback the same way the legacy result
// constructors did, so the emitted observation is byte-identical.
func handlerResultFromReadResult(r capability.ReadResult) intent.HandlerResult {
	return intent.HandlerResult{
		Status:             intent.HandlerStatus(r.Status),
		Reply:              r.Reply,
		NeedsClarification: r.NeedsClarification,
		FailureClass:       intent.HandlerFailureClass(r.FailureClass),
		FallbackReason:     intent.FallbackReason(r.FallbackReason),
		RouteStatus:        routeStatusForReadResult(r),
		ToolAction:         r.ToolAction,
		Envelope:           r.Envelope,
	}
}

// applyReadEffects applies the typed context side-effects a read capability
// declared. The engine consumes only the effect types it recognizes; an
// unrecognized effect is ignored rather than silently reinterpreted.
func (e *Engine) applyReadEffects(effects []capability.ReadEffect) {
	for _, effect := range effects {
		switch eff := effect.(type) {
		case capability.RememberStockReferent:
			// RC017: feed the resolved stock referent to both the structured-fact
			// path (session-fact context on) and the legacy scalar (off); each is
			// mode-gated internally, so a subject-eliding stock follow-up resolves.
			e.recordResolvedStockGpuFact(eff.GPUModel)
			e.recordLastStockGpuModel(eff.GPUModel)
		}
	}
}

func routeStatusForReadResult(r capability.ReadResult) intent.RouteStatus {
	switch r.Status {
	case platform.ReadStatusHandled:
		return intent.RouteStatusDispatched
	case platform.ReadStatusFailureAfterTool:
		return intent.RouteStatusFailureAfterTool
	case platform.ReadStatusFallbackBeforeTool:
		switch r.FallbackReason {
		case platform.ReadFallbackMissingTarget, platform.ReadFallbackUnresolvedTarget, platform.ReadFallbackAmbiguousTarget:
			return intent.RouteStatusFallbackUnresolvedTarget
		case platform.ReadFallbackTimeWindow:
			return intent.RouteStatusFallbackTimeWindow
		case platform.ReadFallbackActionNotAllowed:
			return intent.RouteStatusFallbackIneligible
		default:
			return intent.RouteStatusFallbackInvalid
		}
	default:
		return intent.RouteStatusNone
	}
}

func marshalReadCapabilityError(err error) string {
	payload, _ := json.Marshal(map[string]any{"status": intent.HandlerStatusFallbackBeforeTool, "fallback_reason": intent.FallbackValidation, "error": err.Error()})
	return string(payload)
}
