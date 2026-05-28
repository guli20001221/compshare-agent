package engine

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
)

// truncateDescribeResultForReAct caps the UHostSet length of a raw
// DescribeCompShareInstance result map to intent.DefaultMaxInstancesPerDisplay
// when the LLM did a full-account list (i.e. no specific UHostIds were
// pinned in the call args). Mutates result in place, adding "Shown" and
// "Truncated" fields so the LLM sees the pagination signal.
//
// This is the ReAct-path defense-in-depth: the handler cutover path
// (intent.HandleResourceInfo) already truncates earlier, but planner
// misclassification ("operation_lifecycle" → "unknown" jitter) can route
// a list query through the LLM-driven ReAct loop instead. Without this
// hook a 100-instance account would dump the full list into the token
// budget and the model would silently abbreviate.
//
// Returns (shown, total, truncated). When no truncation happens the
// function is a no-op except for the return values.
func truncateDescribeResultForReAct(args, result map[string]any) (shown, total int, truncated bool) {
	if result == nil {
		return 0, 0, false
	}
	if hasPinnedUHostIds(args) {
		return 0, 0, false
	}
	rawHosts, ok := result["UHostSet"].([]any)
	if !ok {
		return 0, 0, false
	}
	total = len(rawHosts)
	if total <= intent.DefaultMaxInstancesPerDisplay {
		return total, total, false
	}

	type rankedRow struct {
		row  any
		snap entity.InstanceSnapshot
	}
	ranked := make([]rankedRow, 0, total)
	for _, raw := range rawHosts {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ranked = append(ranked, rankedRow{row: raw, snap: entity.InstanceFromMap(row)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return intent.InstanceDisplayLess(ranked[i].snap, ranked[j].snap)
	})

	limit := intent.DefaultMaxInstancesPerDisplay
	kept := make([]any, 0, limit)
	for i := 0; i < len(ranked) && i < limit; i++ {
		kept = append(kept, ranked[i].row)
	}
	result["UHostSet"] = kept
	result["Shown"] = limit
	result["Truncated"] = true
	return limit, total, true
}

// filterDescribeResultByAction narrows UHostSet to rows whose State matches
// the requested lifecycle action so the candidate list shown to the user
// excludes operationally invalid options.
//
// Conservative default — only the actions with an unambiguous required state
// trigger filtering; unknown/empty actions and verbs that work on both states
// (create_disk) pass the list through untouched. This keeps the model in the
// loop when the user is exploring rather than committing to a verb.
//
// Like truncateDescribeResultForReAct, this skips when UHostIds were pinned
// — if the user already named a target there is no candidate set to filter.
//
// PR1 hotfix Bug 4 (2026-05-28): replaces the LLM-side "guess which subset
// to show" heuristic with a deterministic State filter. See memory:
// llm-filter-nondeterministic.
func filterDescribeResultByAction(args, result map[string]any, action intent.LifecycleAction) (kept, removed int, filtered bool) {
	if result == nil || action == "" {
		return 0, 0, false
	}
	if hasPinnedUHostIds(args) {
		return 0, 0, false
	}
	wantState, ok := requiredStateForAction(action)
	if !ok {
		return 0, 0, false
	}
	rawHosts, ok := result["UHostSet"].([]any)
	if !ok {
		return 0, 0, false
	}
	keptRows := make([]any, 0, len(rawHosts))
	for _, raw := range rawHosts {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		state, _ := row["State"].(string)
		if state == wantState {
			keptRows = append(keptRows, raw)
		}
	}
	removed = len(rawHosts) - len(keptRows)
	if removed == 0 {
		return len(keptRows), 0, false
	}
	result["UHostSet"] = keptRows
	result["ActionFilter"] = map[string]any{
		"action":     string(action),
		"want_state": wantState,
		"removed":    removed,
		"kept":       len(keptRows),
		"total_seen": len(rawHosts),
	}
	return len(keptRows), removed, true
}

// requiredStateForAction returns the State value that an instance MUST have
// for the given lifecycle action to be applicable. Conservative: only the
// stop/start/reboot verbs trigger filtering — those are unambiguously
// blocked when the State doesn't match. Verbs that can be valid on either
// state (create_disk, rename) or that need clarification (reinstall/resize
// — UI typically warns either way) are intentionally not filtered.
func requiredStateForAction(action intent.LifecycleAction) (string, bool) {
	switch action {
	case intent.LifecycleActionStop, intent.LifecycleActionReboot:
		return "Running", true
	case intent.LifecycleActionStart:
		return "Stopped", true
	default:
		return "", false
	}
}

// hasPinnedUHostIds returns true when the caller passed at least one
// UHostId in args — in that case the LLM has a specific target list and
// truncation must not drop anything.
func hasPinnedUHostIds(args map[string]any) bool {
	if args == nil {
		return false
	}
	switch v := args["UHostIds"].(type) {
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	case string:
		return v != ""
	}
	return false
}
