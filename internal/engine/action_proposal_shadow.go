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
	"github.com/compshare-agent/internal/workflow"
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

// operationSupportsGuidedIntake reports whether the catalog declares guided
// intake for an operation. It is the declarative replacement for the engine
// hardcoding "CreateInstanceWorkflow" as the guided-card trigger: both the
// complete-proposal guided swap and the incomplete-proposal intake route ask
// this instead of naming the workflow.
func operationSupportsGuidedIntake(action string) bool {
	catalog, err := defaultActionCatalog()
	if err != nil {
		return false
	}
	spec, ok := catalog.Lookup(action)
	return ok && spec.Intake.Mode == actionresolver.IntakeGuided
}

type agentContextEvidenceVerifier struct {
	context AgentContext
	engine  *Engine
	spec    actionresolver.OperationSpec
	// targetEvidence is the engine-produced existence snapshot for each proposed
	// write-target value, keyed by value. It is built BEFORE Resolve (the network
	// point-query lives in the engine), so VerifyCandidate stays a pure read of
	// server-owned evidence.
	targetEvidence map[string]targetEvidence
}

// VerifyCandidate is the single authority over a candidate's provenance. For a
// write TARGET it requires BOTH proofs — the user chose it AND it exists in this
// account this turn — so neither the model's source label, nor mere existence,
// nor a mere prior observation can authorize a write. Non-target fields carry
// only a current-turn literal span as verifiable provenance.
func (v agentContextEvidenceVerifier) VerifyCandidate(candidate actionresolver.SlotCandidate) bool {
	if candidate.Evidence == nil {
		return false
	}
	field, known := v.spec.Fields[candidate.Name]
	if !known {
		return false
	}
	if field.Target {
		value, ok := candidate.Value.(string)
		if !ok || value == "" {
			return false
		}
		return v.targetHasSelectionProof(candidate, value) && v.targetHasExistenceProof(value)
	}
	if candidate.Source == actionresolver.SourceUserExplicit {
		return verifyCurrentQuestionEvidence(v.context, candidate, field.Codec)
	}
	return false
}

// targetHasSelectionProof: why the server believes the USER chose this target.
// The model source label is advisory; selection is re-derived from context.
func (v agentContextEvidenceVerifier) targetHasSelectionProof(candidate actionresolver.SlotCandidate, value string) bool {
	return targetSelectionProof(candidate, value, v.context, v.spec)
}

// targetSelectionProof is the pure SelectionProof: it reads only server-owned
// context, never the network, so both the verifier and the engine's evidence
// builder use it. Existence is only worth establishing for a SELECTED target, so
// the engine gates its point-query on this.
func targetSelectionProof(candidate actionresolver.SlotCandidate, value string, view AgentContext, spec actionresolver.OperationSpec) bool {
	// (1) current-turn literal span: the user typed the id verbatim this turn.
	if candidate.Source == actionresolver.SourceUserExplicit && candidate.Evidence != nil {
		if verifyCurrentQuestionEvidence(view, candidate, spec.Fields[candidate.Name].Codec) {
			return true
		}
	}
	// (2) a fresh entity the user genuinely selected (typed id / card pick) or the
	// account's sole instance. An OBSERVED referent (recorded from a read) is NOT
	// a selection — observed != chosen.
	for _, entity := range view.SelectedEntities {
		if entity.ID == value && entity.Freshness != ContinuityFreshnessExpired && isUserSelectionSource(entity.Source) {
			return true
		}
	}
	return false
}

// targetHasExistenceProof: why the server believes it exists in this account,
// verified THIS turn — a fresh registry hit, a this-turn read, or a this-turn
// point Describe (pre-computed engine-side).
func (v agentContextEvidenceVerifier) targetHasExistenceProof(value string) bool {
	if v.engine == nil {
		return false
	}
	if _, ok := v.engine.RegistrySnapshot().Instances[value]; ok {
		return true
	}
	if _, ok := v.engine.readCapabilitySubjectsThisTurn[value]; ok {
		return true
	}
	if ev, ok := v.targetEvidence[value]; ok && ev.confirmed() {
		return true
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

// resolvedProposal carries a resolved action together with the SAME zone catalog
// snapshot the resolver canonicalized its Zone against, so executeActionProposal
// can thread that one snapshot into the workflow rather than have the workflow
// build a second one. The snapshot is never stashed on the Engine — it lives only
// for this turn's resolve→execute, exactly one per turn (ReferenceData.ZoneCatalog
// is nil for operations that carry no zone field).
type resolvedProposal struct {
	action        actionresolver.ResolvedAction
	referenceData workflow.ReferenceData
}

func (e *Engine) resolveActionProposalShadow(ctx context.Context, args map[string]any) (resolvedProposal, error) {
	proposal, err := decodeActionProposal(args)
	if err != nil {
		return resolvedProposal{}, err
	}
	catalog, err := defaultActionCatalog()
	if err != nil {
		return resolvedProposal{}, err
	}
	view := e.turnContextViewThisTurn
	if !e.turnContextViewReady {
		view = (ContextCompiler{}).CompileForTurn(e, e.lastUserMsg, proposal.TurnID, time.Now())
	}
	if proposal.TurnID == "" {
		proposal.TurnID = view.TurnID
	}
	if view.TurnID != "" && proposal.TurnID != view.TurnID {
		return resolvedProposal{}, fmt.Errorf("proposal turn_id does not match the active turn")
	}
	spec, ok := catalog.Lookup(proposal.Operation)
	if !ok {
		resolved := actionresolver.New(catalog, agentContextEvidenceVerifier{context: view, engine: e}, actionresolver.MachineTypeCatalog{}).Resolve(proposal)
		return resolvedProposal{action: resolved}, nil
	}
	proposal = e.deriveProposalProvenance(proposal, view, spec)
	proposal = completeCurrentTurnEvidence(proposal, view, spec)
	proposal = addSealedSecretCandidates(proposal, spec, e.secretInputsThisTurn)
	targetEvidence := e.targetEvidenceForProposal(ctx, proposal, spec, view)
	machineTypes := e.machineTypeCatalogSnapshot(ctx, spec)
	zoneCatalog := e.zoneCatalogSnapshotForSpec(ctx, spec)
	imageCatalog := e.imageCatalogSnapshotForSpec(ctx, spec, proposalSlotString(proposal, "ImageSource"))
	resolved := actionresolver.New(catalog, agentContextEvidenceVerifier{context: view, engine: e, spec: spec, targetEvidence: targetEvidence}, machineTypes).
		WithZoneCatalog(zoneCatalog).
		WithImageCatalog(imageCatalog).
		Resolve(proposal)
	return resolvedProposal{action: resolved, referenceData: workflow.ReferenceData{ZoneCatalog: zoneCatalog, ImageCatalog: imageCatalog}}, nil
}

// targetEvidenceForProposal builds the existence snapshot for each distinct
// write-target value in the proposal, before the (pure) resolver runs. The
// engine owns the point-query so a Resolve stays replayable from a trace.
func (e *Engine) targetEvidenceForProposal(ctx context.Context, proposal actionresolver.ActionProposal, spec actionresolver.OperationSpec, view AgentContext) map[string]targetEvidence {
	var out map[string]targetEvidence
	for _, candidate := range proposal.Slots {
		field, ok := spec.Fields[candidate.Name]
		if !ok || !field.Target {
			continue
		}
		value, ok := candidate.Value.(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if _, done := out[value]; done {
			continue
		}
		// Only establish existence for a target the user actually selected. An id
		// the model merely inferred has no selection proof and must be refused
		// without spending an upstream point-query on it.
		if !targetSelectionProof(candidate, value, view, spec) {
			continue
		}
		if out == nil {
			out = map[string]targetEvidence{}
		}
		out[value] = e.verifyTargetExistence(ctx, value)
	}
	return out
}

// recordUserSelectedTargets persists an authorized write target as a genuine
// user selection so a later turn's "关掉它" resolves to it (and re-verifies its
// existence). It fires only for a target that already passed the dual-proof, so
// it never promotes an observed-only referent into a selection.
func (e *Engine) recordUserSelectedTargets(resolved actionresolver.ResolvedAction) {
	catalog, err := defaultActionCatalog()
	if err != nil {
		return
	}
	spec, ok := catalog.Lookup(resolved.Operation)
	if !ok {
		return
	}
	for name, field := range spec.Fields {
		if !field.Target {
			continue
		}
		if value, ok := resolved.Arguments[name].(string); ok && strings.TrimSpace(value) != "" {
			e.recordSelectedInstanceIDWithSource(value, "", SelectedInstanceSourceUser)
		}
	}
}

// deriveProposalProvenance keeps write authority out of model-authored
// arguments. The Agent proposes semantic field values; the server decides
// whether each value is present in the current user text, a verified entity, or
// a current/recent read observation. A model cannot promote its own inference
// into a trusted write target by choosing a source label.
func (e *Engine) deriveProposalProvenance(proposal actionresolver.ActionProposal, view AgentContext, spec actionresolver.OperationSpec) actionresolver.ActionProposal {
	present := make(map[string]struct{}, len(proposal.Slots))
	for index := range proposal.Slots {
		candidate := &proposal.Slots[index]
		present[candidate.Name] = struct{}{}
		candidate.Source = actionresolver.SourceAgentInference
		candidate.Evidence = nil

		field, known := spec.Fields[candidate.Name]
		if !known {
			continue
		}
		value, isString := candidate.Value.(string)
		if isString && strings.TrimSpace(value) != "" {
			if start, end, ok := uniqueQuoteForCodec([]rune(view.CurrentQuestion), []rune(value), field.Codec); ok {
				candidate.Source = actionresolver.SourceUserExplicit
				candidate.Evidence = &actionresolver.SourceEvidence{
					MessageID: view.TurnID,
					Start:     start,
					End:       end,
					Quote:     value,
				}
				continue
			}
		}
		if !field.Target || !isString {
			continue
		}
		if _, ok := e.readCapabilitySubjectsThisTurn[value]; ok {
			candidate.Source = actionresolver.SourceToolObservation
			candidate.Evidence = &actionresolver.SourceEvidence{ContextField: "current_turn_read"}
			continue
		}
		for _, entity := range view.SelectedEntities {
			if entity.ID == value && entity.Freshness != ContinuityFreshnessExpired {
				candidate.Source = actionresolver.SourceVerifiedContext
				candidate.Evidence = &actionresolver.SourceEvidence{ContextField: "selected_entities"}
				break
			}
		}
		if candidate.Source != actionresolver.SourceAgentInference {
			continue
		}
		for _, observation := range view.RecentObservations {
			if observation.SubjectID == value && !observation.RefreshRequired {
				candidate.Source = actionresolver.SourceToolObservation
				candidate.Evidence = &actionresolver.SourceEvidence{ContextField: "recent_observations"}
				break
			}
		}
		if candidate.Source == actionresolver.SourceAgentInference {
			if selected, ok := uniqueSelectedEntity(view, field.TargetKind); ok {
				candidate.Value = selected.ID
				candidate.Source = actionresolver.SourceVerifiedContext
				candidate.Evidence = &actionresolver.SourceEvidence{ContextField: "selected_entities"}
			}
		}
	}
	for name, field := range spec.Fields {
		if !field.Target {
			continue
		}
		if _, exists := present[name]; exists {
			continue
		}
		if selected, ok := uniqueSelectedEntity(view, field.TargetKind); ok {
			proposal.Slots = append(proposal.Slots, actionresolver.SlotCandidate{
				Name: name, Value: selected.ID, Source: actionresolver.SourceVerifiedContext,
				Evidence: &actionresolver.SourceEvidence{ContextField: "selected_entities"},
			})
		}
	}
	return proposal
}

func uniqueSelectedEntity(view AgentContext, kind string) (SemanticEntityHint, bool) {
	var selected SemanticEntityHint
	for _, entity := range view.SelectedEntities {
		if entity.Kind != kind || strings.TrimSpace(entity.ID) == "" || entity.Freshness == ContinuityFreshnessExpired {
			continue
		}
		if selected.ID != "" && selected.ID != entity.ID {
			return SemanticEntityHint{}, false
		}
		selected = entity
	}
	return selected, selected.ID != ""
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
	raw, _ := json.Marshal(resolved.action)
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
	if !resolved.action.ReadyForConfirmation {
		// An incomplete-but-collectable proposal opens the guided intake form
		// instead of a prose back-and-forth — but only when a guided form is
		// actually available this turn (the client opted in). The guided workflow
		// collects the missing fields through its own confirm gates and confirms
		// before it creates; it never executes straight from intake.
		if resolved.action.ReadyForIntake && e.guidedCreate && e.confirmEditsFn != nil {
			onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: "提案进入引导式表单收集"})
			return e.executeResolvedWorkflow(ctx, resolved.action.Operation, resolved.action.Arguments, onStep, resolved.referenceData)
		}
		e.rememberPendingResolvedAction(resolved.action)
		return resolvedActionForModel(resolved.action)
	}
	// The target passed the dual proof — record it as a genuine user selection so
	// a later "关掉它" resolves to it (and re-verifies its existence next turn).
	e.recordUserSelectedTargets(resolved.action)
	onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: "提案已验证，进入统一确认与执行门"})
	// Thread the SAME zone snapshot the resolver canonicalized Zone against into the
	// workflow, so the create runs against exactly one catalog for the turn rather
	// than building a second one that could disagree (gate 1).
	return e.executeResolvedWorkflow(ctx, resolved.action.Operation, resolved.action.Arguments, onStep, resolved.referenceData)
}

// rememberPendingResolvedAction parks a half-finished write as a task frame so a
// later turn can complete it. It fires ONLY for a clean "the user has not told us
// X yet" — never for a DependencyFailure, which means OUR catalog query failed. A
// frame in that case would persist "waiting for the user to supply GpuType" into
// the session, blaming the user for our outage and poisoning every later turn's
// context with a task they cannot resolve.
func (e *Engine) rememberPendingResolvedAction(resolved actionresolver.ResolvedAction) {
	if len(resolved.Missing) == 0 || len(resolved.Conflicts) != 0 ||
		len(resolved.Rejected) != 0 || len(resolved.DependencyFailures) != 0 {
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
