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
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
)

// ReadCapabilityObservation is the only result shape exposed by the read
// capability adapter. Control-flow states stay distinct and factual answers
// must cross an evidence envelope before the Agent can consume them. Its status /
// failure / fallback / route types are the platform read vocabulary — the read
// adapter builds the observation directly off capability.ReadResult and no
// longer bridges through the legacy intent.HandlerResult carrier.
type ReadCapabilityObservation struct {
	Capability       string                      `json:"capability"`
	Status           platform.ReadStatus         `json:"status"`
	FailureClass     platform.ReadFailureClass   `json:"failure_class,omitempty"`
	FallbackReason   platform.ReadFallbackReason `json:"fallback_reason,omitempty"`
	RouteStatus      platform.RouteStatus        `json:"route_status,omitempty"`
	ToolAction       string                      `json:"tool_action,omitempty"`
	Envelope         *envelope.Envelope          `json:"evidence,omitempty"`
	Guidance         string                      `json:"guidance,omitempty"`
	RenderRef        string                      `json:"render_ref,omitempty"`
	RenderContract   string                      `json:"render_contract,omitempty"`
	CanAssertAbsence bool                        `json:"can_assert_absence"`
	MissingFields    []capability.MissingField   `json:"missing_fields,omitempty"`
}

func (e *Engine) executeConcreteReadCapability(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	readIntent, request, err := capability.DecodeReadRequest(action, args)
	if err != nil {
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: err.Error()})
		return marshalReadCapabilityError(fmt.Errorf("invalid capability request: %w", err))
	}
	if err := capability.ValidateCurrentTurnGrounding(request, e.lastUserMsg, e.recentPriorUserTexts(4)...); err != nil {
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: err.Error()})
		return marshalReadCapabilityError(fmt.Errorf("ungrounded capability request: %w", err))
	}
	if missing := request.MissingFields(); len(missing) > 0 {
		observation := ReadCapabilityObservation{Capability: string(readIntent), Status: platform.ReadStatusNeedsInput, MissingFields: missing}
		payload, _ := json.Marshal(observation)
		onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "查询参数不完整", TraceResult: map[string]any{"status": string(platform.ReadStatusNeedsInput), "missing_fields": missing}})
		return string(payload)
	}
	reg, ok := capability.MigratedRead(action)
	if !ok {
		// Every model-visible read owns a typed vertical (P3.3/P3.4). A decoded read
		// tool with no migrated capability is a wiring bug, not a runtime state; the
		// legacy intent.Slots read dispatch was deleted in P6.
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: "unmigrated read capability"})
		return marshalReadCapabilityError(fmt.Errorf("read capability %q has no typed vertical", action))
	}
	return e.executeTypedReadCapability(ctx, action, string(readIntent), reg, request, args, onStep)
}

func (e *Engine) recentPriorUserTexts(limit int) []string {
	if e == nil || limit <= 0 {
		return nil
	}
	out := make([]string, 0, limit)
	skippedCurrent := false
	current := strings.TrimSpace(e.lastUserMsg)
	for index := len(e.messages) - 1; index >= 0 && len(out) < limit; index-- {
		message := e.messages[index]
		if message.Role != openai.ChatMessageRoleUser {
			continue
		}
		text := userAuthoredText(message.Content)
		if !skippedCurrent && current != "" && text == current {
			skippedCurrent = true
			continue
		}
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

// executeTypedReadCapability dispatches a migrated read through its typed
// vertical. The capability produces a capability.ReadResult directly, without
// the legacy route-dispatch machinery — which no longer exists to bridge to.
func (e *Engine) executeTypedReadCapability(ctx context.Context, action, capabilityLabel string, reg capability.RegisteredRead, request platform.ReadRequest, args map[string]any, onStep func(StepEvent)) string {
	onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: args})

	now := time.Now()
	snapshot := e.RegistrySnapshot()
	rt := capability.ReadRuntime{
		Executor:         capabilityHandlerExecutor{engine: e, onStep: onStep},
		Resolver:         snapshot,
		Now:              now,
		FallbackGPUModel: e.fallbackStockGpuModel(now),
	}
	if capabilityLabel == string(intent.IntentZoneCatalog) {
		rt.ZoneCatalog = e.zoneCatalogSnapshot(ctx)
	}
	if user, ok := tools.UserFrom(ctx); ok {
		rt.TopOrganizationID = user.TopOrganizationID
		rt.OrganizationID = user.OrganizationID
	}
	if e.sessionStateHydrated && e.sessionState.SelectedInstanceID != "" {
		rt.FallbackInstanceID = e.sessionState.SelectedInstanceID
	}
	readResult := reg.Run(ctx, request, rt)
	e.applyReadEffects(readResult.Effects)
	if readResult.Status == platform.ReadStatusUnavailable {
		return e.buildUnavailableObservation(action, capabilityLabel, readResult, onStep)
	}
	// Absence is asserted only against a registry still fresh as of `now`: a
	// stale-but-complete snapshot must not tell the model "you have none".
	observation := e.buildReadObservation(action, capabilityLabel, readResult, snapshot.CanAssertAbsenceAt(now), onStep)
	return observation
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
// observation shape. It builds directly off the typed capability.ReadResult and
// the platform status vocabulary; RouteStatus is the one derived field.
func (e *Engine) buildReadObservation(action, capabilityLabel string, result capability.ReadResult, canAssertAbsence bool, onStep func(StepEvent)) string {
	result = e.contextEnvelopeForPlainDirectReply(result)
	observation := ReadCapabilityObservation{
		Capability:       capabilityLabel,
		Status:           result.Status,
		FailureClass:     result.FailureClass,
		FallbackReason:   result.FallbackReason,
		RouteStatus:      routeStatusForReadResult(result),
		ToolAction:       result.ToolAction,
		Envelope:         result.Envelope,
		CanAssertAbsence: canAssertAbsence,
	}
	// A browse listing is a menu, not a measurement: it gets no render_ref, so the
	// server never staples the raw catalog in front of the model's curated answer.
	// The envelope below still carries every exact id/count the model needs.
	if result.Status == platform.ReadStatusHandled && !result.NeedsClarification && !result.ReplyIsBrowseListing &&
		result.Envelope != nil && strings.TrimSpace(result.Reply) != "" {
		placeholder := fmt.Sprintf("{{READ_OBSERVATION_%d}}", len(e.readResponseEvidenceThisTurn)+1)
		e.readResponseEvidenceThisTurn = append(e.readResponseEvidenceThisTurn, readResponseEvidence{
			Capability:  capabilityLabel,
			Reply:       strings.TrimSpace(result.Reply),
			Envelope:    *result.Envelope,
			Placeholder: placeholder,
			Required:    result.RenderRequired,
		})
		observation.RenderRef = placeholder
		observation.RenderContract = readRenderContract(result.RenderExclusive)
	}
	if result.Status != platform.ReadStatusHandled || result.NeedsClarification {
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

func readRenderContract(exclusive bool) string {
	contract := "在最终回答中原样插入 render_ref；服务端会替换为真实查询结果。可以继续查询资料并解释原因或处理方法，但不得改写、否定或用推测替代这份观察中的事实。"
	if exclusive {
		return "在最终回答中原样插入 render_ref；服务端会替换为真实查询结果。只输出 render_ref，不要在前后复述；只有用户还要求解释或建议时才补充。可以继续查询资料并解释原因或处理方法，但不得改写、否定或用推测替代这份观察中的事实。"
	}
	return contract
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
		case capability.RememberVerifiedInstances:
			// Same-id-verified existence evidence for the write path (concern #6):
			// only a resource_info response echoing the exact id lands here, so a
			// later write on that id has a real ExistenceProof without re-querying.
			if e.verifiedInstanceEvidenceThisTurn == nil {
				e.verifiedInstanceEvidenceThisTurn = map[string]struct{}{}
			}
			for _, id := range eff.IDs {
				if id != "" {
					e.verifiedInstanceEvidenceThisTurn[id] = struct{}{}
				}
			}
		}
	}
}

func routeStatusForReadResult(r capability.ReadResult) platform.RouteStatus {
	switch r.Status {
	case platform.ReadStatusHandled, platform.ReadStatusEmpty:
		// Empty is a successful dispatch that found no data — it ran, so it shares
		// the dispatched route status; the distinct read status carries the emptiness.
		return platform.RouteStatusDispatched
	case platform.ReadStatusConflict:
		// Ambiguous request resolving to multiple candidates — mirror the legacy
		// ambiguous-target fallback route status.
		return platform.RouteStatusFallbackUnresolvedTarget
	case platform.ReadStatusFailureAfterTool:
		return platform.RouteStatusFailureAfterTool
	case platform.ReadStatusFallbackBeforeTool:
		switch r.FallbackReason {
		case platform.ReadFallbackMissingTarget, platform.ReadFallbackUnresolvedTarget, platform.ReadFallbackAmbiguousTarget:
			return platform.RouteStatusFallbackUnresolvedTarget
		case platform.ReadFallbackTimeWindow:
			return platform.RouteStatusFallbackTimeWindow
		case platform.ReadFallbackActionNotAllowed:
			return platform.RouteStatusFallbackIneligible
		default:
			return platform.RouteStatusFallbackInvalid
		}
	default:
		return platform.RouteStatusNone
	}
}

func marshalReadCapabilityError(err error) string {
	payload, _ := json.Marshal(map[string]any{"status": platform.ReadStatusFallbackBeforeTool, "fallback_reason": platform.ReadFallbackValidation, "error": err.Error()})
	return string(payload)
}
