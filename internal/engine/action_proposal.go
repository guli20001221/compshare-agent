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

// operationSupportsGuidedIntake reads the operation's declared guided intake.
func operationSupportsGuidedIntake(action string) bool {
	catalog, err := defaultActionCatalog()
	if err != nil {
		return false
	}
	spec, ok := catalog.Lookup(action)
	return ok && spec.Intake.Mode == actionresolver.IntakeGuided
}

type proposalTargetVerifier struct {
	spec actionresolver.OperationSpec
	// targetEvidence is the engine-produced existence verdict for each proposed
	// write target, keyed by (field, kind, id) — never a bare id, so an instance's
	// proof cannot authorize a same-id disk/CFS. It is built BEFORE Resolve (the
	// network point-query lives in the engine), so target adjudication stays a pure
	// read of server-owned evidence.
	targetEvidence map[targetEvidenceKey]targetEvidence
}

// AdjudicateTarget checks the exact target proposed for this operation. The
// Agent resolves conversational references; account existence and the operation's
// confirmation card remain server-owned.
func (v proposalTargetVerifier) AdjudicateTarget(candidate actionresolver.SlotCandidate) actionresolver.TargetVerdict {
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
	proposal.Slots = pruneBlankSlots(proposal.Slots)
	return proposal, nil
}

// pruneBlankSlots treats null and blank strings as omitted arguments, allowing
// the guided form to collect missing values. Zero and false remain real values.
func pruneBlankSlots(slots []actionresolver.SlotCandidate) []actionresolver.SlotCandidate {
	kept := slots[:0:0]
	for _, candidate := range slots {
		if blankProposalValue(candidate.Value) {
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}

// blankProposalValue is deliberately narrow: only JSON null and a whitespace-only
// string. Zero, false and empty collections are real values for their codecs and
// must keep reaching adjudication.
func blankProposalValue(value any) bool {
	if value == nil {
		return true
	}
	text, isString := value.(string)
	return isString && strings.TrimSpace(text) == ""
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

func (e *Engine) resolveActionProposal(ctx context.Context, args map[string]any) (resolvedProposal, error) {
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
		resolved := actionresolver.New(catalog, nil, actionresolver.MachineTypeCatalog{}).Resolve(proposal)
		return resolvedProposal{action: resolved}, nil
	}
	targetEvidence := e.targetEvidenceForProposal(ctx, proposal, spec)
	machineTypes := e.machineTypeCatalogSnapshot(ctx, spec)
	zoneCatalog := e.zoneCatalogSnapshotForSpec(ctx, spec)
	imageSource := proposalImageCatalogSource(proposal, spec)
	// An explicit source constrains the exact lookup; when omitted, the catalogs
	// establish which source owns the ID.
	imageCatalog, detectedImageSource := e.resolveImageCatalogSnapshotForSpec(
		ctx, spec, imageSource, proposalSlotString(proposal, "CompShareImageId"), imageSource != "",
	)
	if imageSource == "" && detectedImageSource != "" {
		proposal.Slots = append(proposal.Slots, actionresolver.SlotCandidate{Name: "ImageSource", Value: detectedImageSource})
	}
	resolved := actionresolver.New(catalog, proposalTargetVerifier{spec: spec, targetEvidence: targetEvidence}, machineTypes).
		WithZoneCatalog(zoneCatalog).
		WithImageCatalog(imageCatalog).
		Resolve(proposal)
	return resolvedProposal{action: resolved, referenceData: workflow.ReferenceData{
		ZoneCatalog:  zoneCatalog,
		ImageCatalog: imageCatalog,
	}, targetEvidence: targetEvidence}, nil
}

func proposalImageCatalogSource(proposal actionresolver.ActionProposal, spec actionresolver.OperationSpec) string {
	if source := proposalSlotString(proposal, "ImageSource"); source != "" {
		return source
	}
	return spec.ImageCatalogSource
}

// targetEvidenceForProposal builds an existence verdict for every distinct
// write-target value in the proposal, before the (pure) resolver runs. The engine
// owns any point-query so a Resolve stays replayable from a trace. Existence is
// established UNIFORMLY for every concrete target — a literal user reference, a
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

// proposalInstanceTargetValue returns the proposal's instance target, if any.
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

func (e *Engine) executeActionProposal(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	resolved, err := e.resolveActionProposal(ctx, args)
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

func resolvedActionForModel(resolved actionresolver.ResolvedAction) string {
	raw, _ := json.Marshal(resolved)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if len(resolved.RejectedProblems) > 0 {
		details := make([]any, 0, len(resolved.RejectedProblems))
		for _, problem := range resolved.RejectedProblems {
			details = append(details, map[string]any{
				"slot":  problem.Slot,
				"kind":  problem.Kind.String(),
				"actor": string(problem.Actor),
			})
		}
		wire["rejection_details"] = details
	}
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
