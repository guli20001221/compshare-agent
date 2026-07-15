package engine

type ResponseContract string

const (
	ResponseAgent          ResponseContract = "agent"
	ResponsePolicyTerminal ResponseContract = "policy_terminal"
)

const (
	memoryUpdateNone       = "none"
	memoryUpdateStructured = "structured_event"
	memoryUpdateCompactor  = "compactor"
	memoryUpdateExcerpt    = "excerpt"

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
	digest := view.ConversationDigest
	if digest.Narrative != "" || len(digest.Excerpts) > 0 || len(digest.Goals) > 0 ||
		len(digest.Constraints) > 0 || len(digest.Decisions) > 0 || len(digest.UnresolvedTasks) > 0 {
		ids = append(ids, "digest")
	}
	if view.ActiveTask != nil {
		ids = append(ids, "active_task")
	}
	if len(view.SelectedEntities) > 0 {
		ids = append(ids, "selected_entities")
	}
	if len(view.RecentObservations) > 0 {
		ids = append(ids, "recent_observations")
	}
	if len(view.VerifiedKnowledge) > 0 {
		ids = append(ids, "verified_knowledge")
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
	priority := map[string]int{memoryUpdateNone: 0, memoryUpdateStructured: 1, memoryUpdateExcerpt: 2, memoryUpdateCompactor: 3}
	if priority[source] > priority[e.memoryUpdateSourceThisTurn] {
		e.memoryUpdateSourceThisTurn = source
	}
}

func normalizedMemoryUpdateSource(source string) string {
	switch source {
	case memoryUpdateStructured, memoryUpdateCompactor, memoryUpdateExcerpt:
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
