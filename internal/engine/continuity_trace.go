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
	if view.ActiveTask != nil {
		ids = append(ids, "active_task")
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
	if e == nil {
		return
	}
	// Two tiers, not four: the "excerpt" and "compactor" tiers were produced only
	// by the ConversationDigest writers, which are gone. The highest-wins shape is
	// kept because that is the contract — a turn reports its strongest memory
	// write, not its last one.
	priority := map[string]int{memoryUpdateNone: 0, memoryUpdateStructured: 1}
	if priority[source] > priority[e.memoryUpdateSourceThisTurn] {
		e.memoryUpdateSourceThisTurn = source
	}
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
