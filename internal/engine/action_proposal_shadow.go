package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
)

var (
	actionCatalogOnce sync.Once
	actionCatalog     *actionresolver.Catalog
	actionCatalogErr  error
)

func defaultActionCatalog() (*actionresolver.Catalog, error) {
	actionCatalogOnce.Do(func() { actionCatalog, actionCatalogErr = actionresolver.BuildCatalog() })
	return actionCatalog, actionCatalogErr
}

type agentContextEvidenceVerifier struct {
	context AgentContext
	engine  *Engine
	spec    actionresolver.OperationSpec
}

func (v agentContextEvidenceVerifier) VerifyCandidate(candidate actionresolver.SlotCandidate) bool {
	if candidate.Evidence == nil {
		return false
	}
	switch candidate.Source {
	case actionresolver.SourceUserExplicit:
		field, known := v.spec.Fields[candidate.Name]
		if !known || !verifyCurrentQuestionEvidence(v.context, candidate, field.Codec) {
			return false
		}
		if !known || !field.Target {
			return known
		}
		value, _ := candidate.Value.(string)
		if v.engine != nil {
			_, ok := v.engine.RegistrySnapshot().Instances[value]
			return ok
		}
		return false
	case actionresolver.SourceVerifiedContext:
		if candidate.Evidence.ContextField != "selected_entities" {
			return false
		}
		value, _ := candidate.Value.(string)
		for _, entity := range v.context.SelectedEntities {
			if entity.ID == value && entity.Freshness != ContinuityFreshnessExpired {
				return true
			}
		}
	case actionresolver.SourceToolObservation:
		value, _ := candidate.Value.(string)
		if candidate.Evidence.ContextField == "current_turn_read" && v.engine != nil {
			_, ok := v.engine.readCapabilitySubjectsThisTurn[value]
			return ok
		}
		if candidate.Evidence.ContextField != "recent_observations" {
			return false
		}
		for _, observation := range v.context.RecentObservations {
			if observation.SubjectID == value && !observation.RefreshRequired {
				return true
			}
		}
	}
	return false
}

func verifyCurrentQuestionEvidence(context AgentContext, candidate actionresolver.SlotCandidate, codec actionresolver.SlotCodecKind) bool {
	evidence := candidate.Evidence
	if evidence.MessageID == "" || evidence.MessageID != context.TurnID || evidence.Start < 0 || evidence.End <= evidence.Start {
		return false
	}
	question := []rune(context.CurrentQuestion)
	if evidence.End > len(question) || string(question[evidence.Start:evidence.End]) != evidence.Quote {
		return false
	}
	value, ok := candidate.Value.(string)
	if !ok || value != evidence.Quote {
		return false
	}
	return evidenceSpanForCodec(question, evidence.Start, evidence.End, codec)
}

func evidenceSpanForCodec(text []rune, start, end int, codec actionresolver.SlotCodecKind) bool {
	if codec != actionresolver.CodecCapacity {
		return standaloneSpan(text, start, end)
	}
	// Capacities are normally embedded in Chinese prose ("加200G数据盘").
	// Only ASCII token neighbours can make the quote a substring of another
	// number/unit; surrounding CJK characters are grammatical separators here.
	isASCIIValuePart := func(r rune) bool {
		return r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.')
	}
	if start > 0 && isASCIIValuePart(text[start-1]) {
		return false
	}
	return end >= len(text) || !isASCIIValuePart(text[end])
}

func standaloneSpan(text []rune, start, end int) bool {
	isPart := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
	if start > 0 && isPart(text[start-1]) {
		return false
	}
	return end >= len(text) || !isPart(text[end])
}

func decodeActionProposal(args map[string]any) (actionresolver.ActionProposal, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return actionresolver.ActionProposal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var proposal actionresolver.ActionProposal
	if err := decoder.Decode(&proposal); err != nil {
		return actionresolver.ActionProposal{}, err
	}
	if strings.TrimSpace(proposal.Operation) == "" {
		return actionresolver.ActionProposal{}, fmt.Errorf("operation is required")
	}
	return proposal, nil
}

func (e *Engine) resolveActionProposalShadow(ctx context.Context, args map[string]any) (actionresolver.ResolvedAction, error) {
	proposal, err := decodeActionProposal(args)
	if err != nil {
		return actionresolver.ResolvedAction{}, err
	}
	catalog, err := defaultActionCatalog()
	if err != nil {
		return actionresolver.ResolvedAction{}, err
	}
	view := e.turnContextViewThisTurn
	if !e.turnContextViewReady {
		view = (ContextCompiler{}).CompileForTurn(e, e.lastUserMsg, proposal.TurnID, time.Now())
	}
	if proposal.TurnID == "" {
		proposal.TurnID = view.TurnID
	}
	if view.TurnID != "" && proposal.TurnID != view.TurnID {
		return actionresolver.ResolvedAction{}, fmt.Errorf("proposal turn_id does not match the active turn")
	}
	spec, ok := catalog.Lookup(proposal.Operation)
	if ok {
		proposal = completeCurrentTurnEvidence(proposal, view, spec)
		proposal = addSealedSecretCandidates(proposal, spec, e.secretInputsThisTurn)
	}
	if !ok {
		return actionresolver.New(catalog, agentContextEvidenceVerifier{context: view, engine: e}, actionresolver.MachineTypeCatalog{}).Resolve(proposal), nil
	}
	machineTypes := e.machineTypeCatalogSnapshot(ctx, spec)
	return actionresolver.New(catalog, agentContextEvidenceVerifier{context: view, engine: e, spec: spec}, machineTypes).Resolve(proposal), nil
}

// machineTypeCatalogSnapshot fetches the live machine-type names and hands them
// to the resolver as data. This function is the boundary the design turns on:
// the network call, its failure mode and any future caching live HERE, in the
// engine, so actionresolver stays a pure function of its inputs and a Resolve
// can be replayed from a trace.
//
// A failed or empty query yields Available=false — NOT a fallback to a built-in
// table. Canonicalizing a machine type against a stale local copy is exactly the
// bug this vertical removed: the platform's catalog is the only thing that knows
// which cards exist, so when we cannot reach it we say so and refuse rather than
// name a card from memory.
//
// Status is deliberately NOT filtered: a sold-out card is still a real machine
// type, and resolving the name then failing at the capacity precheck tells the
// user the truth ("4090 售罄") instead of a lie ("没有 4090 这种机型").
func (e *Engine) machineTypeCatalogSnapshot(ctx context.Context, spec actionresolver.OperationSpec) actionresolver.MachineTypeCatalog {
	if !actionresolver.SpecNeedsMachineTypeCatalog(spec) {
		return actionresolver.MachineTypeCatalog{}
	}
	result := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
	if result == nil {
		return actionresolver.MachineTypeCatalog{Available: false}
	}
	items, _ := result["AvailableInstanceTypes"].([]any)
	names := platform.CollectAPINamesFromInstanceTypes(items)
	if len(names) == 0 {
		return actionresolver.MachineTypeCatalog{Available: false}
	}
	return actionresolver.MachineTypeCatalog{Names: names, Available: true}
}

// completeCurrentTurnEvidence fills protocol metadata that the runtime already
// owns. The model identifies the quoted value; it does not need to copy an
// opaque turn id or count Unicode offsets. Ambiguous or non-standalone quotes
// remain unresolved and therefore cannot become trusted write targets.
func completeCurrentTurnEvidence(proposal actionresolver.ActionProposal, view AgentContext, spec actionresolver.OperationSpec) actionresolver.ActionProposal {
	question := []rune(view.CurrentQuestion)
	for index := range proposal.Slots {
		candidate := &proposal.Slots[index]
		if candidate.Source != actionresolver.SourceUserExplicit {
			continue
		}
		value, ok := candidate.Value.(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if candidate.Evidence == nil {
			candidate.Evidence = &actionresolver.SourceEvidence{Quote: value}
		}
		if candidate.Evidence.Quote == "" {
			candidate.Evidence.Quote = value
		}
		if candidate.Evidence.Quote != value {
			continue
		}
		field, known := spec.Fields[candidate.Name]
		if !known {
			continue
		}
		start, end, ok := uniqueQuoteForCodec(question, []rune(candidate.Evidence.Quote), field.Codec)
		if !ok {
			continue
		}
		candidate.Evidence.MessageID = view.TurnID
		candidate.Evidence.Start = start
		candidate.Evidence.End = end
	}
	return proposal
}

func uniqueQuoteForCodec(text, quote []rune, codec actionresolver.SlotCodecKind) (int, int, bool) {
	if codec != actionresolver.CodecCapacity {
		return uniqueStandaloneQuote(text, quote)
	}
	if len(quote) == 0 || len(quote) > len(text) {
		return 0, 0, false
	}
	start := -1
	for offset := 0; offset+len(quote) <= len(text); offset++ {
		if string(text[offset:offset+len(quote)]) != string(quote) || !evidenceSpanForCodec(text, offset, offset+len(quote), codec) {
			continue
		}
		if start >= 0 {
			return 0, 0, false
		}
		start = offset
	}
	if start < 0 {
		return 0, 0, false
	}
	return start, start + len(quote), true
}

func uniqueStandaloneQuote(text, quote []rune) (int, int, bool) {
	if len(quote) == 0 || len(quote) > len(text) {
		return 0, 0, false
	}
	start := -1
	for offset := 0; offset+len(quote) <= len(text); offset++ {
		if string(text[offset:offset+len(quote)]) != string(quote) || !standaloneSpan(text, offset, offset+len(quote)) {
			continue
		}
		if start >= 0 {
			return 0, 0, false
		}
		start = offset
	}
	if start < 0 {
		return 0, 0, false
	}
	return start, start + len(quote), true
}

func addSealedSecretCandidates(proposal actionresolver.ActionProposal, spec actionresolver.OperationSpec, secrets map[string]string) actionresolver.ActionProposal {
	present := make(map[string]struct{}, len(proposal.Slots))
	for _, candidate := range proposal.Slots {
		present[candidate.Name] = struct{}{}
	}
	for name, field := range spec.Fields {
		if field.Codec != actionresolver.CodecSensitiveText {
			continue
		}
		if _, exists := present[name]; exists {
			continue
		}
		if value := strings.TrimSpace(secrets[name]); value != "" {
			proposal.Slots = append(proposal.Slots, actionresolver.SlotCandidate{
				Name: name, Value: value, Source: actionresolver.SourceUserConfirmation,
			})
		}
	}
	return proposal
}

func (e *Engine) executeActionProposalShadow(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	resolved, err := e.resolveActionProposalShadow(ctx, args)
	if err != nil {
		onStep(StepEvent{Type: StepError, Action: tools.ProposeActionName, Source: observability.ToolSourceShadowOnly, Message: err.Error()})
		payload, _ := json.Marshal(map[string]any{"error": err.Error(), "ready_for_confirmation": false})
		return string(payload)
	}
	raw, _ := json.Marshal(resolved)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	payload, _ := json.Marshal(security.RedactForLLM(wire))
	var trace map[string]any
	tracePayload, _ := json.Marshal(security.RedactForTrace(wire))
	_ = json.Unmarshal(tracePayload, &trace)
	onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceShadowOnly, Message: "影子解析完成，未执行操作", TraceResult: trace})
	return string(payload)
}

func (e *Engine) executeActionProposal(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	e.actionProposalRanThisTurn = true
	resolved, err := e.resolveActionProposalShadow(ctx, args)
	if err != nil {
		onStep(StepEvent{Type: StepError, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: err.Error()})
		payload, _ := json.Marshal(map[string]any{"error": err.Error(), "ready_for_confirmation": false})
		return string(payload)
	}
	if !resolved.ReadyForConfirmation {
		e.rememberPendingResolvedAction(resolved)
		return resolvedActionForModel(resolved)
	}
	onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: "提案已验证，进入统一确认与执行门"})
	return e.executeWorkflow(ctx, resolved.Operation, resolved.Arguments, onStep)
}

func (e *Engine) rememberPendingResolvedAction(resolved actionresolver.ResolvedAction) {
	if len(resolved.Missing) == 0 || len(resolved.Conflicts) != 0 || len(resolved.Rejected) != 0 {
		return
	}
	now := time.Now()
	frame := ContextFrame{
		Version:         1,
		Kind:            ContextFrameKindWorkflowTask,
		Status:          ContextFrameStatusPending,
		Intent:          "write_action",
		Workflow:        resolved.Operation,
		OriginalUserMsg: e.lastUserMsg,
		Slots:           map[string]string{},
		SlotSources:     map[string]string{},
		MissingSlots:    append([]string(nil), resolved.Missing...),
		Stage:           "collecting_parameters",
		ProducedAtUnix:  now.Unix(),
		TTLSeconds:      ContextFrameTTLSeconds,
	}
	for name, slot := range resolved.Provenance {
		if slot.Codec == actionresolver.CodecSensitiveText {
			continue
		}
		frame.Slots[name] = fmt.Sprint(slot.Value)
		frame.SlotSources[name] = string(slot.Source)
	}
	e.setContextFrame(frame)
}

func resolvedActionForModel(resolved actionresolver.ResolvedAction) string {
	raw, _ := json.Marshal(resolved)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	payload, _ := json.Marshal(security.RedactForLLM(wire))
	return string(payload)
}
