package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
)

// ReadCapabilityObservation is the only result shape exposed by the read
// capability adapter. Control-flow states stay distinct and factual answers
// must cross an evidence envelope before the Agent can consume them.
type ReadCapabilityObservation struct {
	Capability           string                     `json:"capability"`
	Status               intent.HandlerStatus       `json:"status"`
	FailureClass         intent.HandlerFailureClass `json:"failure_class,omitempty"`
	FallbackReason       intent.FallbackReason      `json:"fallback_reason,omitempty"`
	RouteStatus          intent.RouteStatus         `json:"route_status,omitempty"`
	ToolAction           string                     `json:"tool_action,omitempty"`
	Envelope             *envelope.Envelope         `json:"evidence,omitempty"`
	Guidance             string                     `json:"guidance,omitempty"`
	DirectSubmitEligible bool                       `json:"direct_submit_eligible"`
	CanAssertAbsence     bool                       `json:"can_assert_absence"`
}

func isReadHandlerIntent(value intent.Intent) bool {
	return value == intent.IntentResourceInfo || value == intent.IntentMonitorQuery ||
		value == intent.IntentMonitorHistory || intent.IsRoutingIntent(value)
}

func (e *Engine) invokeReadHandler(ctx context.Context, plan intent.IntentRoute, userMsg string, snapshot entity.RegistrySnapshot, onStep func(StepEvent)) intent.HandlerResult {
	handler := intent.NewDemoHandler(plannerHandlerExecutor{engine: e, onStep: onStep})
	req := intent.HandlerRequest{Plan: plan, Resolver: snapshot, UserText: userMsg}
	if e.sessionStateHydrated && e.sessionState.SelectedInstanceID != "" {
		req.FallbackInstanceID = e.sessionState.SelectedInstanceID
	}
	req.FallbackGpuModel = e.fallbackStockGpuModel(time.Now())
	switch plan.Intent {
	case intent.IntentResourceInfo:
		return handler.HandleResourceInfo(ctx, req)
	case intent.IntentMonitorQuery, intent.IntentMonitorHistory:
		return handler.HandleMonitorQuery(ctx, req)
	default:
		if intent.IsRoutingIntent(plan.Intent) {
			return handler.DispatchRoute(ctx, req)
		}
		result := intent.FallbackBeforeTool(intent.FallbackValidation)
		result.Reply = "unsupported read capability"
		return result
	}
}

func decodeReadCapabilityArgs(args map[string]any) (intent.Intent, intent.Slots, error) {
	name, ok := args["capability"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", intent.Slots{}, fmt.Errorf("capability is required")
	}
	capability := intent.Intent(strings.TrimSpace(name))
	if !isReadHandlerIntent(capability) {
		return "", intent.Slots{}, fmt.Errorf("capability %q is not a registered read capability", capability)
	}
	var slots intent.Slots
	raw, exists := args["slots"]
	if !exists || raw == nil {
		return capability, slots, nil
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return "", intent.Slots{}, fmt.Errorf("encode slots: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&slots); err != nil {
		return "", intent.Slots{}, fmt.Errorf("invalid slots: %w", err)
	}
	return capability, slots, nil
}

func (e *Engine) executeReadPlatformCapability(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	capability, slots, err := decodeReadCapabilityArgs(args)
	if err != nil {
		onStep(StepEvent{Type: StepError, Action: tools.ReadPlatformCapabilityName, Source: observability.ToolSourceMainReAct, Message: err.Error()})
		return marshalReadCapabilityError(err)
	}
	filtered := map[string]any{"capability": string(capability), "slots": args["slots"]}
	onStep(StepEvent{Type: StepToolCall, Action: tools.ReadPlatformCapabilityName, Source: observability.ToolSourceMainReAct, Args: filtered})

	plan := intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: capability, Slots: slots, Confidence: 1}
	userMsg := e.lastUserMsg
	plan = planWithUserTextMonitorMetrics(plan, userMsg)
	snapshot := e.RegistrySnapshot()
	plan = augmentPlanTargetRefsFromUserText(plan, userMsg, snapshot)
	result := e.invokeReadHandler(ctx, plan, userMsg, snapshot, onStep)
	e.annotateHandlerResultForUserQuestion(&result, plan, userMsg)
	result = e.contextEnvelopeForPlainDirectReply(result, plan, userMsg)

	observation := ReadCapabilityObservation{
		Capability:           string(capability),
		Status:               result.Status,
		FailureClass:         result.FailureClass,
		FallbackReason:       result.FallbackReason,
		RouteStatus:          result.RouteStatus,
		ToolAction:           result.ToolAction,
		Envelope:             result.Envelope,
		DirectSubmitEligible: result.Status == intent.HandlerStatusHandled && !result.NeedsClarification && result.Envelope != nil,
		CanAssertAbsence:     snapshot.CanAssertAbsence(),
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
	onStep(StepEvent{Type: StepToolResult, Action: tools.ReadPlatformCapabilityName, Source: observability.ToolSourceMainReAct, Message: "查询完成", TraceResult: traceResult})
	return string(payload)
}

func marshalReadCapabilityError(err error) string {
	payload, _ := json.Marshal(map[string]any{"status": intent.HandlerStatusFallbackBeforeTool, "fallback_reason": intent.FallbackValidation, "error": err.Error()})
	return string(payload)
}
