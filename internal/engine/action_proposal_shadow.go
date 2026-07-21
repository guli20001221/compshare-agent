package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/entity"
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
	// binding is the deterministic instance selection for the turn, computed ONCE
	// by the SelectionBinder. A bound id is a server-derived SelectionProof; a
	// conflict means the user's references disagree and every instance target must
	// be asked, never picked.
	binding selectionBinding
	// targetEvidence is the engine-produced existence verdict for each proposed
	// write target, keyed by (field, kind, id) — never a bare id, so an instance's
	// proof cannot authorize a same-id disk/CFS. It is built BEFORE Resolve (the
	// network point-query lives in the engine), so target adjudication stays a pure
	// read of server-owned evidence.
	targetEvidence map[targetEvidenceKey]targetEvidence
}

// VerifyCandidate is the trust boundary for NON-target fields (a current-turn
// literal span is their only verifiable provenance). Write TARGETS are decided by
// AdjudicateTarget instead — the resolver calls it directly — so a target's
// existence outage or a reference conflict lands in the right refusal channel.
func (v agentContextEvidenceVerifier) VerifyCandidate(candidate actionresolver.SlotCandidate) bool {
	field, known := v.spec.Fields[candidate.Name]
	if !known {
		return false
	}
	if field.Target {
		return v.AdjudicateTarget(candidate) == actionresolver.TargetAccept
	}
	if candidate.Evidence == nil {
		return false
	}
	if candidate.Source == actionresolver.SourceUserExplicit {
		return verifyCurrentQuestionEvidence(v.context, candidate, field.Codec)
	}
	return false
}

// AdjudicateTarget decides a write target's disposition. A conflict among the
// user's own references is a Conflict (ask); existence drives the rest — Verified
// exists (Accept, the confirmation card is the remaining gate), NotFound / an
// unverified inferred id is a Reject, and an existence-check outage is a
// DependencyFailure (our failure, never the user's target being invalid).
func (v agentContextEvidenceVerifier) AdjudicateTarget(candidate actionresolver.SlotCandidate) actionresolver.TargetVerdict {
	value, ok := candidate.Value.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return actionresolver.TargetReject
	}
	field, known := v.spec.Fields[candidate.Name]
	if !known || field.TargetKind == "" {
		// Not a known target field (or a field with no resource kind): refuse rather
		// than fall through to a stale/foreign evidence entry.
		return actionresolver.TargetReject
	}
	if field.TargetKind == "instance" && v.binding.conflict {
		return actionresolver.TargetConflict
	}
	// Look up evidence by the SAME (field, kind, id) key it was built under, so an
	// instance proof can never be reused for a same-id disk/CFS target.
	ev, ok := v.targetEvidence[targetEvidenceKey{field: candidate.Name, kind: field.TargetKind, id: strings.TrimSpace(value)}]
	if !ok {
		return actionresolver.TargetReject
	}
	switch ev.Verdict {
	case entity.ExistenceVerified:
		return actionresolver.TargetAccept
	case entity.ExistenceUnavailable:
		return actionresolver.TargetDependencyFailure
	default:
		return actionresolver.TargetReject
	}
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
	// targetEvidence is the per-(field,kind,id) existence proof the engine
	// established for this proposal's write targets before Resolve. The verifier
	// consumes only its Verdict; it is carried here so executeActionProposal can
	// emit the dual-proof audit (emitWriteAuthorizationTraces) instead of discarding
	// it. Empty for the no-spec path and for non-target proposals.
	targetEvidence map[targetEvidenceKey]targetEvidence
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
	// Deterministic instance selection for the turn, computed ONCE: it binds the
	// user's verifiable reference (typed id / ordinal / unique name / prior explicit
	// pick / sole account instance) and is threaded into provenance, the point-query
	// gate and the target adjudicator so all three agree on which id the user chose.
	binding := e.bindInstanceTarget(view)
	proposal = e.deriveProposalProvenance(proposal, view, spec, binding)
	proposal = completeCurrentTurnEvidence(proposal, view, spec)
	proposal = addSealedSecretCandidates(proposal, spec, e.secretInputsThisTurn)
	targetEvidence := e.targetEvidenceForProposal(ctx, proposal, spec)
	machineTypes := e.machineTypeCatalogSnapshot(ctx, spec)
	zoneCatalog := e.zoneCatalogSnapshotForSpec(ctx, spec)
	imageCatalog := e.imageCatalogSnapshotForSpec(ctx, spec,
		proposalSlotString(proposal, "ImageSource"), proposalSlotString(proposal, "CompShareImageId"))
	resolved := actionresolver.New(catalog, agentContextEvidenceVerifier{context: view, engine: e, spec: spec, binding: binding, targetEvidence: targetEvidence}, machineTypes).
		WithZoneCatalog(zoneCatalog).
		WithImageCatalog(imageCatalog).
		Resolve(proposal)
	return resolvedProposal{action: resolved, referenceData: workflow.ReferenceData{ZoneCatalog: zoneCatalog, ImageCatalog: imageCatalog}, targetEvidence: targetEvidence}, nil
}

// targetEvidenceForProposal builds an existence verdict for every distinct
// write-target value in the proposal, before the (pure) resolver runs. The engine
// owns any point-query so a Resolve stays replayable from a trace. Existence is
// established UNIFORMLY for every concrete target — a deterministic binding, a
// carried referent and a fresh inference all get the same server-side point-query;
// the confirmation card + the user's confirm is the SelectionProof, so there is no
// source-based gate before verification. The verifier is chosen by resource kind;
// a disk is scoped to the proposal's instance target (its parent) because a disk
// exists only inside an instance's DiskSet.
func (e *Engine) targetEvidenceForProposal(ctx context.Context, proposal actionresolver.ActionProposal, spec actionresolver.OperationSpec) map[targetEvidenceKey]targetEvidence {
	instanceID := proposalInstanceTargetValue(proposal, spec)
	var out map[targetEvidenceKey]targetEvidence
	for _, candidate := range proposal.Slots {
		field, ok := spec.Fields[candidate.Name]
		if !ok || !field.Target {
			continue
		}
		value, ok := candidate.Value.(string)
		if !ok {
			continue
		}
		id := strings.TrimSpace(value)
		if id == "" {
			continue
		}
		key := targetEvidenceKey{field: candidate.Name, kind: field.TargetKind, id: id}
		if _, done := out[key]; done {
			continue
		}
		if out == nil {
			out = map[targetEvidenceKey]targetEvidence{}
		}
		out[key] = e.verifyTargetExistence(ctx, field.TargetKind, id, instanceID)
	}
	return out
}

// proposalInstanceTargetValue returns the value of the proposal's instance target
// field (UHostId), used to scope a disk target to its parent instance. Empty when
// the proposal names no instance target.
func proposalInstanceTargetValue(proposal actionresolver.ActionProposal, spec actionresolver.OperationSpec) string {
	for _, candidate := range proposal.Slots {
		field, ok := spec.Fields[candidate.Name]
		if !ok || !field.Target || field.TargetKind != "instance" {
			continue
		}
		if value, ok := candidate.Value.(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// recordUserSelectedTargets persists a CONFIRMED write target as a genuine user
// selection so a later turn's "关掉它" resolves to it (and re-verifies its
// existence). The caller fires it only after the confirmation gate succeeded, so
// the user's confirm event is what promotes the target to a persisted selection —
// an unconfirmed inference is never recorded.
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
		// Only an INSTANCE target may become the session's SelectedInstanceID. A CFS or
		// disk id must never be written there — a ResizeCFS/ResizeDisk success would
		// otherwise poison the next turn's "关掉它" with a CfsId/DiskId (worse for a disk
		// resize, whose UHostId+DiskId targets race on Go's nondeterministic map order).
		// CFS/disk cross-turn memory, if ever needed, gets its own typed selection state.
		if !field.Target || field.TargetKind != "instance" {
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
func (e *Engine) deriveProposalProvenance(proposal actionresolver.ActionProposal, view AgentContext, spec actionresolver.OperationSpec, binding selectionBinding) actionresolver.ActionProposal {
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
		// A deterministic binding OWNS the instance target: the server bound the
		// user's verifiable reference to this exact id, overriding whatever id the
		// model proposed (a "第2台" that the model answered with the 第1台's id is
		// re-pointed at the 第2台's). This is what makes a mis-selected but existing
		// id fail authorization rather than sail through.
		if field.Target && isString && binding.bound() && field.TargetKind == "instance" {
			candidate.Value = binding.id
			candidate.Source = actionresolver.SourceVerifiedContext
			candidate.Evidence = &actionresolver.SourceEvidence{ContextField: "selection_binding"}
			continue
		}
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
		// A concrete target the user did not reference deterministically this turn is
		// left as the Agent's honest inference. The server does NOT canonicalize it
		// against SelectedEntities or override it with the sole selected entity: that
		// would make candidate-set membership a trust signal again (the P0 bug class).
		// Existence is proven uniformly by ExactTargetVerifier and the confirmation
		// card is the SelectionProof — neither reads this source label.
	}
	// A missing instance target is completed ONLY from a deterministic binding (the
	// sole account instance, or a prior explicit pick). A bare command with no
	// verifiable target is left Missing so the agent asks — never silently completed
	// from a mere observation.
	for name, field := range spec.Fields {
		if !field.Target || field.TargetKind != "instance" {
			continue
		}
		if _, exists := present[name]; exists {
			continue
		}
		if binding.bound() {
			proposal.Slots = append(proposal.Slots, actionresolver.SlotCandidate{
				Name: name, Value: binding.id, Source: actionresolver.SourceVerifiedContext,
				Evidence: &actionresolver.SourceEvidence{ContextField: "selection_binding"},
			})
		}
	}
	return proposal
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
		e.actionProposalDispositionThisTurn = "resolve_error"
		onStep(StepEvent{Type: StepError, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: err.Error()})
		payload, _ := json.Marshal(map[string]any{"error": err.Error(), "ready_for_confirmation": false})
		return string(payload)
	}
	// Classify what the resolver did with the proposal (value-free) for the
	// acceptance measurement / trace: did it reach a card, and if not, why.
	e.actionProposalDispositionThisTurn = resolvedProposalDisposition(resolved.action, e.guidedCreate && e.confirmEditsFn != nil)
	if !resolved.action.ReadyForConfirmation {
		// An incomplete-but-collectable proposal opens the guided intake form
		// instead of a prose back-and-forth — but only when a guided form is
		// actually available this turn (the client opted in). The guided workflow
		// collects the missing fields through its own confirm gates and confirms
		// before it creates; it never executes straight from intake.
		if resolved.action.ReadyForIntake && e.guidedCreate && e.confirmEditsFn != nil {
			onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: "提案进入引导式表单收集"})
			// ReadyForIntake established just above, so the constructor accepts it.
			ca, _ := newConfirmableAction(resolved)
			return e.executeResolvedWorkflow(ctx, ca, onStep)
		}
		e.rememberPendingResolvedAction(resolved.action)
		return resolvedActionForModel(resolved.action)
	}
	onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceMainReAct, Message: "提案已验证，进入统一确认与执行门"})
	// Thread the SAME zone snapshot the resolver canonicalized Zone against into the
	// workflow, so the create runs against exactly one catalog for the turn rather
	// than building a second one that could disagree (gate 1). ReadyForConfirmation
	// was established at the top of this branch, so the constructor accepts it.
	ca, _ := newConfirmableAction(resolved)
	reply := e.executeResolvedWorkflow(ctx, ca, onStep)
	// Persist the dual-proof audit for this write's verified targets: the
	// ExistenceProof the resolver established (which oracle / when / account /
	// verdict) plus whether the confirmation authorized execution. Fires for both
	// authorized and declined writes — executionAuthorized carries the distinction.
	e.emitWriteAuthorizationTraces(resolved, e.lastConfirmationAcceptedThisCall)
	// Record the target as a genuine user selection ONLY when the confirmation gate
	// was ACCEPTED — the confirmation IS the SelectionProof for an Agent-inferred
	// target. Gating on acceptance (not full workflow success) is deliberate: a
	// confirmed target whose upstream write then fails is still a target the user
	// chose, so it is remembered and a later "关掉它" resolves to it (its existence
	// is re-verified next turn). A cancel / timeout / decline / pre-confirm stop
	// persists nothing — an unconfirmed guess must never resolve a later reference.
	if e.lastConfirmationAcceptedThisCall {
		e.recordUserSelectedTargets(resolved.action)
	}
	return reply
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

// resolvedProposalDisposition is a compact, value-free classification of what the
// resolver did with a write proposal, for the acceptance measurement and the
// outcome trace: it answers "did the proposal reach a card, and if not, why". It
// records only field names + typed rejection kinds — never slot VALUES — so it is
// safe to persist. The order of the checks matches executeActionProposal's own
// branch precedence (a card path wins; a server outage is reported before a
// user-facing rejection). guidedFormAvailable is (guidedCreate && confirmEditsFn),
// so an intake-eligible proposal that could not open a form (client did not opt
// in) is distinguished from one that carded.
func resolvedProposalDisposition(a actionresolver.ResolvedAction, guidedFormAvailable bool) string {
	switch {
	case a.ReadyForConfirmation:
		return "confirmation"
	case a.ReadyForIntake && guidedFormAvailable:
		return "intake_form"
	case a.ReadyForIntake:
		return "intake_form_unavailable"
	case len(a.DependencyFailures) > 0:
		return "dependency_failure"
	case len(a.RejectedProblems) > 0:
		return "rejected:" + rejectionKindSummary(a.RejectedProblems)
	case len(a.Rejected) > 0:
		return "rejected"
	case len(a.Conflicts) > 0:
		return "conflict:" + conflictSlotSummary(a.Conflicts)
	case len(a.Missing) > 0:
		return "missing:" + strings.Join(a.Missing, ",")
	default:
		return "unresolved"
	}
}

// rejectionKindSummary renders "<slot>=<kind>" pairs, deduped and sorted for a
// stable trace. An operation-level rejection (empty slot) is rendered as "_op".
func rejectionKindSummary(problems []actionresolver.RejectedProblem) string {
	seen := map[string]bool{}
	var parts []string
	for _, p := range problems {
		slot := p.Slot
		if slot == "" {
			slot = "_op"
		}
		item := slot + "=" + p.Kind.String()
		if seen[item] {
			continue
		}
		seen[item] = true
		parts = append(parts, item)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// conflictSlotSummary renders the conflicting slot names, deduped and sorted.
func conflictSlotSummary(conflicts []actionresolver.Conflict) string {
	seen := map[string]bool{}
	var parts []string
	for _, c := range conflicts {
		if seen[c.Slot] {
			continue
		}
		seen[c.Slot] = true
		parts = append(parts, c.Slot)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
