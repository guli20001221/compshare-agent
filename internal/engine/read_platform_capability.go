package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
)

// ReadCapabilityObservation is the only result shape exposed by the read
// capability adapter. Control-flow states stay distinct and factual answers
// must cross an evidence envelope before the Agent can consume them.
type ReadCapabilityObservation struct {
	Capability       string                      `json:"capability"`
	Status           platform.ReadStatus         `json:"status"`
	FailureClass     platform.ReadFailureClass   `json:"failure_class,omitempty"`
	FallbackReason   platform.ReadFallbackReason `json:"fallback_reason,omitempty"`
	RouteStatus      platform.RouteStatus        `json:"route_status,omitempty"`
	ToolAction       string                      `json:"tool_action,omitempty"`
	Envelope         *envelope.Envelope          `json:"evidence,omitempty"`
	Guidance         string                      `json:"guidance,omitempty"`
	CanAssertAbsence bool                        `json:"can_assert_absence"`
	MissingFields    []capability.MissingField   `json:"missing_fields,omitempty"`
}

// platformReadEvidence is server-side proof of a read tool's factual result.
// It is intentionally separate from the text shown to the user: every ordinary
// read is already given to the Agent as observation evidence, which owns the
// final Markdown response.
type platformReadEvidence struct {
	Capability string
	Reply      string
	Envelope   envelope.Envelope
}

func (e *Engine) executeConcreteReadCapability(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	readIntent, request, err := capability.DecodeReadRequest(action, args)
	if err != nil {
		// The user did not supply an invalid value here: this is the model's
		// just-emitted tool JSON failing the typed schema. Preserve the closed
		// AgentTool contract and tell the model to repair its own call instead of
		// converting a local serialization mistake into a user clarification.
		agentResult := modelOwnedReadArgumentError(
			action,
			"read_argument_validation",
			"工具参数未通过该能力的参数契约。请依据工具 schema 和用户已明确表达的条件修正参数后，重发同一次调用；不要向用户重复提问。",
		)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: err.Error(), ErrorCode: agentResult.Error.Code})
		return tools.MarshalAgentToolResult(agentResult)
	}
	if err := e.validateCurrentTurnReadGrounding(request); err != nil {
		// Grounding rejects an invented optional filter before any read runs. It
		// is also wholly model-owned: the user already gave the question, so the
		// next step is to remove the invention or use an expressed condition.
		agentResult := modelOwnedReadArgumentError(
			action,
			"read_argument_grounding",
			"工具参数包含用户本轮或近期对话未明确表达的筛选条件或资源 ID。请按用户原文修正后重发同一次调用；不要向用户重复提问。",
		)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: err.Error(), ErrorCode: agentResult.Error.Code})
		return tools.MarshalAgentToolResult(agentResult)
	}
	if missing := request.MissingFields(); len(missing) > 0 {
		observation := ReadCapabilityObservation{Capability: string(readIntent), Status: platform.ReadStatusNeedsInput, MissingFields: missing}
		payload, _ := json.Marshal(observation)
		onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "查询参数不完整", TraceResult: map[string]any{"status": string(platform.ReadStatusNeedsInput), "missing_fields": missing}})
		return string(payload)
	}
	reg, ok := capability.RegisteredReadForTool(action)
	if !ok {
		agentResult := readCapabilityInternalError(action, "unregistered read capability")
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: "unregistered read capability", ErrorCode: agentResult.Error.Code})
		return tools.MarshalAgentToolResult(agentResult)
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

// executeTypedReadCapability dispatches a typed read vertical.
func (e *Engine) executeTypedReadCapability(ctx context.Context, action, capabilityLabel string, reg capability.RegisteredRead, request platform.ReadRequest, args map[string]any, onStep func(StepEvent)) string {
	onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: args})

	now := time.Now()
	snapshot := e.RegistrySnapshot()
	rt := capability.ReadRuntime{
		Executor:     capabilityHandlerExecutor{engine: e, onStep: onStep},
		Resolver:     snapshot,
		Now:          now,
		SyncRegistry: e.syncRegistryFromDescribe,
	}
	if reg.RequiresZoneCatalog(request) {
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
	if readResult.Status == platform.ReadStatusNeedsInput &&
		readResult.FallbackReason == platform.ReadFallbackValidation &&
		readResult.Envelope != nil {
		return e.buildModelOwnedReadCorrection(action, capabilityLabel, readResult, onStep)
	}
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
// off the read-observation evidence path: nothing was retrieved, so the Agent
// receives only the deterministic reason and the alternatives it can offer.
func (e *Engine) buildUnavailableObservation(action, capabilityLabel string, r capability.ReadResult, onStep func(StepEvent)) string {
	observation := UnavailableCapabilityObservation{
		Capability:   capabilityLabel,
		Status:       string(platform.ReadStatusUnavailable),
		Reason:       r.Reply,
		Alternatives: r.Alternatives,
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return tools.MarshalAgentToolResult(readCapabilityInternalError(action, err.Error()))
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
	if result.Status == platform.ReadStatusHandled && !result.NeedsClarification && result.Envelope != nil {
		e.recordPlatformReadEvidence(capabilityLabel, result)
	}
	if result.Status == platform.ReadStatusHandled && !result.NeedsClarification &&
		strings.TrimSpace(result.SensitiveReply) != "" {
		e.sensitiveRepliesThisTurn = append(e.sensitiveRepliesThisTurn, strings.TrimSpace(result.SensitiveReply))
		observation.Guidance = "已取得敏感访问凭据，系统会安全展示给用户；不要推测、复述或要求用户再次提供该凭据。"
	}
	if result.Status != platform.ReadStatusHandled || result.NeedsClarification {
		observation.Guidance = result.Reply
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return tools.MarshalAgentToolResult(readCapabilityInternalError(action, err.Error()))
	}
	var traceResult map[string]any
	_ = json.Unmarshal(payload, &traceResult)
	onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "查询完成", TraceResult: traceResult})
	return string(payload)
}

// buildModelOwnedReadCorrection uses the existing correct_tool_call control
// plane when a typed read has enough live evidence for the model to repair a
// dynamic argument itself. The user has already supplied the intent, so this
// must not degrade into ask_user. The evidence is also recorded in the existing
// turn-local ledger so the corrected call can cite only an exact value returned
// by the live catalog.
func (e *Engine) buildModelOwnedReadCorrection(action, capabilityLabel string, result capability.ReadResult, onStep func(StepEvent)) string {
	e.recordPlatformReadEvidence(capabilityLabel, result)
	data := map[string]any{
		"capability":         capabilityLabel,
		"fallback_reason":    result.FallbackReason,
		"tool_action":        result.ToolAction,
		"evidence":           result.Envelope,
		"guidance":           strings.TrimSpace(result.Reply),
		"can_assert_absence": false,
	}
	agentResult := tools.AgentToolInvalidToolCall(
		action,
		tools.AgentToolCodeInvalidArguments,
		"工具参数未匹配本轮返回的实时目录。请仅在语义唯一时使用 data.evidence 中的完整名称或 ID 修正参数并重发同一次调用；存在多个合理候选时再请用户选择。",
		tools.AgentToolMeta{SourceStatus: "read_argument_validation"},
	)
	agentResult.Data = data
	onStep(StepEvent{
		Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct,
		Message: "已读取实时目录，正在修正查询条件",
		TraceResult: map[string]any{
			"status":          string(platform.ReadStatusNeedsInput),
			"fallback_reason": string(result.FallbackReason),
			"tool_action":     result.ToolAction,
		},
		ErrorCode: agentResult.Error.Code,
	})
	return tools.MarshalAgentToolResult(agentResult)
}

func (e *Engine) recordPlatformReadEvidence(capabilityLabel string, result capability.ReadResult) {
	if e == nil || result.Envelope == nil {
		return
	}
	e.platformReadEvidenceThisTurn = append(e.platformReadEvidenceThisTurn, platformReadEvidence{
		Capability: capabilityLabel,
		Reply:      strings.TrimSpace(result.Reply),
		Envelope:   *result.Envelope,
	})
}

func (e *Engine) validateCurrentTurnReadGrounding(request platform.ReadRequest) error {
	if stock, ok := request.(capability.StockAvailabilityRequest); ok &&
		e.stockZonesGroundedByCurrentTurnCatalog(stock.ZoneMentions) {
		// Only the zone field gains proof from the live catalog. Clear that field
		// and leave every other present or future grounding rule intact.
		stock.ZoneMentions = nil
		request = stock
	}
	return capability.ValidateCurrentTurnGrounding(request, e.lastUserMsg, e.recentPriorUserTexts(4)...)
}

// stockZonesGroundedByCurrentTurnCatalog is the narrow bridge between a live
// catalog observation and the corrected stock call that follows it. It accepts
// only exact zone subjects from evidence produced this turn; it does not infer
// aliases, persist a mapping, or relax grounding for any other argument.
func (e *Engine) stockZonesGroundedByCurrentTurnCatalog(mentions []string) bool {
	if len(mentions) == 0 {
		return false
	}
	allowed := map[string]struct{}{}
	for _, evidence := range e.platformReadEvidenceThisTurn {
		if evidence.Envelope.Kind != envelope.KindZoneCatalog {
			continue
		}
		for _, subject := range evidence.Envelope.Subjects {
			if subject.Type != envelope.SubjectZone {
				continue
			}
			if value := platform.FoldLiteralSpan(subject.ID); value != "" {
				allowed[value] = struct{}{}
			}
			if value := platform.FoldLiteralSpan(subject.Name); value != "" {
				allowed[value] = struct{}{}
			}
		}
	}
	if len(allowed) == 0 {
		return false
	}
	for _, mention := range mentions {
		if _, ok := allowed[platform.FoldLiteralSpan(mention)]; !ok {
			return false
		}
	}
	return true
}

// applyReadEffects applies the typed context side-effects a read capability
// declared. The engine consumes only the effect types it recognizes; an
// unrecognized effect is ignored rather than silently reinterpreted.
func (e *Engine) applyReadEffects(effects []capability.ReadEffect) {
	for _, effect := range effects {
		switch eff := effect.(type) {
		case capability.RememberVerifiedInstances:
			// Same-ID existence evidence for the write path:
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
		case capability.RememberDisplayedInstances:
			if len(eff.Instances) > 1 {
				candidates := append([]entity.InstanceSnapshot(nil), eff.Instances...)
				e.displayedResourceSelectionThisTurn = &pendingResourceSelection{
					snapshot: snapshotFromPendingSelectionCandidates(candidates), candidates: candidates,
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

// modelOwnedReadArgumentError keeps invalid typed read calls on the
// already-established correct_tool_call branch. It must return the outer Agent
// result directly: agentToolObservation recognizes that envelope and therefore
// cannot re-wrap it as ask_user.
func modelOwnedReadArgumentError(action, sourceStatus, message string) tools.AgentToolResult {
	return tools.AgentToolInvalidToolCall(
		action,
		tools.AgentToolCodeInvalidArguments,
		message,
		tools.AgentToolMeta{SourceStatus: sourceStatus},
	)
}

// readCapabilityInternalError is for wiring and result-encoding faults after the
// model's arguments were accepted. These are server faults, not a request for
// the user to restate a valid question. The protected step event/log retains the
// diagnostic; the model gets only a stable, non-retryable failure disposition.
func readCapabilityInternalError(action, diagnostic string) tools.AgentToolResult {
	_ = diagnostic
	return tools.AgentToolFailure(
		action,
		nil,
		"READ_CAPABILITY_INTERNAL_ERROR",
		"读取能力当前无法完成该请求，请如实说明暂时无法查询，不要要求用户重复提供相同信息。",
		tools.AgentToolMeta{SourceStatus: "read_internal_error"},
	)
}
