package engine

import (
	"strings"

	"github.com/compshare-agent/internal/intent"
	grounded "github.com/compshare-agent/internal/renderer"
)

// contextAwareDirectIntents is the production direct-dispatch surface whose
// answers must understand short follow-ups without turning user text into
// evidence. billing_account_unsupported is intentionally absent: it is a hard
// policy response, not a grounded data renderer.
var contextAwareDirectIntents = map[intent.Intent]struct{}{
	intent.IntentResourceInfo:          {},
	intent.IntentMonitorQuery:          {},
	intent.IntentGPUSpecsQuery:         {},
	intent.IntentStockAvailability:     {},
	intent.IntentPricingQuery:          {},
	intent.IntentRefundEstimate:        {},
	intent.IntentImageTagCatalog:       {},
	intent.IntentModelRepositoryBrowse: {},
	intent.IntentImageList:             {},
	intent.IntentNetAcceleratorStatus:  {},
}

func isContextAwareDirectIntent(value intent.Intent) bool {
	_, ok := contextAwareDirectIntents[value]
	return ok
}

// directRenderTaskSpec builds understanding-only input for the grounded
// renderer. The result deliberately excludes ContextFrame slots, tool facts,
// permissions, and trust sources. None of these fields authorize an operation
// or become factual evidence; renderer validates the answer against Envelope.
func (e *Engine) directRenderTaskSpec(plan intent.IntentRoute, userMsg string) grounded.TaskSpec {
	if !isContextAwareDirectIntent(plan.Intent) {
		return grounded.TaskSpec{}
	}
	spec := grounded.TaskSpec{
		CurrentQuestion: compactSemanticText(userMsg),
		Intent:          compactSemanticText(string(plan.Intent)),
	}
	if e == nil || !e.sessionStateHydrated {
		return spec
	}

	state := e.sessionState
	task := state.TaskSnapshot
	if task.Status != TaskSnapshotStatusResolved && !taskSnapshotEmpty(task) {
		spec.Goal = compactSemanticText(task.Goal)
		spec.Stage = compactSemanticText(task.Stage)
		spec.Freshness = compactSemanticText(task.Freshness)
		spec.Constraints = compactSemanticItems(task.Constraints)
		spec.Decisions = compactSemanticItems(task.Decisions)
		spec.MissingSlots = compactSemanticItems(task.MissingSlots)
		spec.EntityHints = appendTaskSpecEntityHints(spec.EntityHints, task.Entities)
	}

	digest := state.ConversationDigest
	spec.ContextSummary = compactSemanticNarrative(digest.Narrative)
	spec.UnresolvedTasks = compactSemanticItems(digest.UnresolvedTasks)
	spec.Constraints = mergeSemanticItems(spec.Constraints, digest.Constraints)
	spec.Decisions = mergeSemanticItems(spec.Decisions, digest.Decisions)
	spec.EntityHints = appendTaskSpecEntityHints(spec.EntityHints, digest.EntityHints)
	if id := strings.TrimSpace(state.SelectedInstanceID); id != "" {
		spec.EntityHints = appendTaskSpecEntityHints(spec.EntityHints, []SemanticEntityHint{{
			Kind:      "instance",
			ID:        id,
			Name:      state.SelectedInstanceName,
			Freshness: normalizedSelectedInstanceFreshness(state),
		}})
	}
	return spec
}

func appendTaskSpecEntityHints(existing []grounded.TaskSpecEntityHint, incoming []SemanticEntityHint) []grounded.TaskSpecEntityHint {
	positions := make(map[string]int, len(existing)+len(incoming))
	for i, hint := range existing {
		positions[strings.ToLower(hint.Kind+"\x00"+hint.ID+"\x00"+hint.Name)] = i
	}
	for _, hint := range incoming {
		converted := grounded.TaskSpecEntityHint{
			Kind:      compactSemanticText(hint.Kind),
			ID:        compactSemanticText(hint.ID),
			Name:      compactSemanticText(hint.Name),
			Freshness: compactSemanticText(hint.Freshness),
		}
		if converted.ID == "" && converted.Name == "" {
			continue
		}
		key := strings.ToLower(converted.Kind + "\x00" + converted.ID + "\x00" + converted.Name)
		if idx, ok := positions[key]; ok {
			existing[idx] = converted
			continue
		}
		positions[key] = len(existing)
		existing = append(existing, converted)
	}
	return existing
}
