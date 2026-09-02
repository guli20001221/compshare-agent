package engine

type ResponseContract string

const (
	ResponseAgent          ResponseContract = "agent"
	ResponsePolicyTerminal ResponseContract = "policy_terminal"
)

const (
	evidenceUpdateNone     = "none"
	evidenceUpdateRecorded = "structured_event"

	groundingSupported   = "supported"
	groundingRepaired    = "repaired"
	groundingUnsupported = "unsupported"
	groundingUnavailable = "unavailable"

	groundingCitationScopeCurrentOnly = "current_only"
	groundingCitationScopePriorOnly   = "prior_only"
	groundingCitationScopeMixed       = "mixed"
)

func contextSourceIDs(view AgentContext) []string {
	ids := make([]string, 0, 5)
	if len(view.RecentConversation) > 0 {
		ids = append(ids, "recent_pairs")
	}
	if len(view.SelectedEntities) > 0 {
		ids = append(ids, "selected_entities")
	}
	return ids
}

func (e *Engine) effectiveResponseContract() ResponseContract {
	if e == nil {
		return ResponseAgent
	}
	if e.hardBlockStandingThisTurn {
		return ResponsePolicyTerminal
	}
	return ResponseAgent
}

func (e *Engine) markVerifiedEvidenceUpdated() {
	if e == nil {
		return
	}
	e.verifiedEvidenceUpdateThisTurn = evidenceUpdateRecorded
}

func normalizedEvidenceUpdateSource(source string) string {
	switch source {
	case evidenceUpdateRecorded:
		return source
	default:
		return evidenceUpdateNone
	}
}

func normalizedGroundingOutcome(outcome string) string {
	switch outcome {
	case groundingSupported, groundingRepaired, groundingUnsupported:
		return outcome
	default:
		return groundingUnavailable
	}
}

func normalizedGroundingCitationScope(scope string) string {
	switch scope {
	case groundingCitationScopeCurrentOnly, groundingCitationScopePriorOnly, groundingCitationScopeMixed:
		return scope
	default:
		return ""
	}
}
