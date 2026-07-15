package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/observability"
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

type agentContextEvidenceVerifier struct{ context AgentContext }

func (v agentContextEvidenceVerifier) VerifyCandidate(candidate actionresolver.SlotCandidate) bool {
	if candidate.Evidence == nil {
		return false
	}
	switch candidate.Source {
	case actionresolver.SourceUserExplicit:
		if !verifyCurrentQuestionEvidence(v.context, candidate) {
			return false
		}
		value, _ := candidate.Value.(string)
		for _, entity := range v.context.SelectedEntities {
			if entity.ID == value && entity.Freshness != ContinuityFreshnessExpired {
				return true
			}
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
		if candidate.Evidence.ContextField != "recent_observations" {
			return false
		}
		value, _ := candidate.Value.(string)
		for _, observation := range v.context.RecentObservations {
			if observation.SubjectID == value && !observation.RefreshRequired {
				return true
			}
		}
	}
	return false
}

func verifyCurrentQuestionEvidence(context AgentContext, candidate actionresolver.SlotCandidate) bool {
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
	return standaloneSpan(question, evidence.Start, evidence.End)
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
	if strings.TrimSpace(proposal.TurnID) == "" || strings.TrimSpace(proposal.Operation) == "" {
		return actionresolver.ActionProposal{}, fmt.Errorf("turn_id and operation are required")
	}
	return proposal, nil
}

func (e *Engine) resolveActionProposalShadow(args map[string]any) (actionresolver.ResolvedAction, error) {
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
	return actionresolver.New(catalog, agentContextEvidenceVerifier{context: view}).Resolve(proposal), nil
}

func (e *Engine) executeActionProposalShadow(args map[string]any, onStep func(StepEvent)) string {
	resolved, err := e.resolveActionProposalShadow(args)
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

// observeLegacyWorkflowArguments compares the old workflow argument surface to
// the new resolver without granting the resolver an executor or authority.
func (e *Engine) observeLegacyWorkflowArguments(action string, args map[string]any, onStep func(StepEvent)) {
	catalog, err := defaultActionCatalog()
	if err != nil {
		return
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	proposal := actionresolver.ActionProposal{Operation: action}
	for _, name := range names {
		proposal.Slots = append(proposal.Slots, actionresolver.SlotCandidate{Name: name, Value: args[name], Source: actionresolver.SourceLegacyArguments})
	}
	resolved := actionresolver.New(catalog, nil).Resolve(proposal)
	onStep(StepEvent{Type: StepToolResult, Action: tools.ProposeActionName, Source: observability.ToolSourceShadowOnly, Message: "旧写路径影子比对完成，未改变执行", TraceResult: map[string]any{
		"operation": action, "legacy_slot_count": len(args), "resolved_slot_count": len(resolved.Arguments),
		"missing_count": len(resolved.Missing), "conflict_count": len(resolved.Conflicts), "rejected_count": len(resolved.Rejected),
	}})
}
