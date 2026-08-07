package engine

type ResponseContract string

const (
	ResponseAgent          ResponseContract = "agent"
	ResponsePolicyTerminal ResponseContract = "policy_terminal"
)

const (
	memoryUpdateNone       = "none"
	memoryUpdateStructured = "structured_event"

	groundingSupported   = "supported"
	groundingRepaired    = "repaired"
	groundingUnsupported = "unsupported"
	groundingUnavailable = "unavailable"
)

func contextSourceIDs(view TurnContextView) []string {
	ids := make([]string, 0, 5)
	if len(view.RecentConversation) > 0 {
		ids = append(ids, "recent_pairs")
	}
	if len(view.SelectedEntities) > 0 {
		ids = append(ids, "selected_entities")
	}
	if len(view.ContinuityNotices) > 0 {
		ids = append(ids, "notices")
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

func (e *Engine) markMemoryUpdateSource(source string) {
	if e == nil || source != memoryUpdateStructured {
		return
	}
	e.memoryUpdateSourceThisTurn = memoryUpdateStructured
}

func normalizedMemoryUpdateSource(source string) string {
	switch source {
	case memoryUpdateStructured:
		return source
	default:
		return memoryUpdateNone
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
