package engine

import (
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
)

// selection source labels carried on a SemanticEntityHint. Only a genuine user
// selection ("user_selected") or the account's sole fresh instance is carried
// context the binder may complete a bare command from.
const (
	selectionSourceAccountSingle = "account_registry_single"
	selectionSourcePendingCard   = "pending_selection"
)

// selectionBinding is the deterministic resolution of which instance the user
// selected this turn. It never guesses: exactly one of a bound id, a conflict, or
// neither holds. A bound id is a server-derived SelectionProof; a conflict means
// the user's own references disagree, so the agent must ask rather than pick one.
type selectionBinding struct {
	id       string
	conflict bool
	// explicit distinguishes "the user named a target which could not be
	// resolved" from "the user did not name any target". The latter can safely
	// use a complete account-single proof obtained later in the same turn; the
	// former must never be silently replaced with that sole instance.
	explicit bool
}

func (b selectionBinding) bound() bool { return b.id != "" && !b.conflict }

// bindInstanceTarget collects every DETERMINISTICALLY verifiable selection signal
// this turn and returns the single instance id they agree on. It is not an intent
// parser and not a fuzzy matcher: it verifies references the server can prove — a
// typed id, an ordinal against a shown candidate list, a unique exact instance
// name, a prior EXPLICIT user pick, or the sole instance of a complete account.
// Semantic references ("把刚才最慢的那台停掉") are left to the Agent; if the binder
// cannot prove the reference it returns no binding, and the Agent's inferred target
// rides into the confirmation card where the user's confirm becomes the proof.
//
// Precedence: an explicit reference in THIS message (id / ordinal / name) wins over
// carried context (prior pick / account-single) — a user pointing now overrides a
// stale selection. Within either tier, two references to DIFFERENT instances are a
// conflict, never a pick-one.
func (e *Engine) bindInstanceTarget(view AgentContext) selectionBinding {
	if e == nil {
		return selectionBinding{}
	}
	text := strings.TrimSpace(view.CurrentQuestion)
	if text == "" {
		return selectionBinding{}
	}
	now := time.Now()
	snap := e.RegistrySnapshot()

	// Tier A — explicit references in the current message.
	var refs []string
	explicit := false
	conflict := false

	if pending, ok := e.pendingResourceSelectionFromSession(); ok {
		if m, _ := matchResourceSelectionReference(text, *pending); m.ok {
			explicit = true
			refs = appendDistinctID(refs, m.instance.UHostId)
		} else if m.ambiguous {
			explicit, conflict = true, true
		}
		// Independently collect every explicit reference to a SHOWN candidate — a
		// typed id or a boundary-safe exact name — against the candidates' own
		// (always-fresh) snapshot. A single matcher returns only ONE reference (an
		// ordinal short-circuits a co-present typed id), so without this a genuine
		// two-reference conflict ("停止 uhost-a 第2台", id A vs 第2台 B) silently binds
		// to one. It must fire regardless of the LIVE registry's freshness — a
		// rehydrated HTTP session or a post-mutation invalidated snapshot is cold, and
		// that is exactly when the conflict would otherwise slip through.
		hits, unresolved := pending.snapshot.ResolveInstanceRefsInText(text)
		for _, h := range hits {
			explicit = true
			refs = appendDistinctID(refs, h.UHostId)
		}
		if len(unresolved) > 0 {
			explicit = true
		}
		if id, ambiguous, present := uniqueRegistryNameInText(text, pending.snapshot); present {
			explicit = true
			if ambiguous {
				conflict = true
			} else {
				refs = appendDistinctID(refs, id)
			}
		}
	}
	// An ordinal phrase ("第2台") is an explicit reference even when it does not
	// resolve to a candidate (no list, or out of range): the user pointed at a
	// position, so account-single must not silently complete the command instead.
	if _, ok := extractResourceSelectionOrdinal(text); ok {
		explicit = true
	}

	if snap.FreshAndCompleteAt(now) {
		// EVERY literal id typed in the message, resolved against a trustworthy
		// registry — all of them, not the first. This used to call
		// findExplicitInstanceRef, which returns hits[0] and drops the rest, so
		// "停止 uhost-a 和 uhost-b" bound silently to uhost-a: a pick-one where the
		// contract above says conflict. The pending-candidate branch already
		// collects them all for exactly this reason, and its comment says so; the
		// live registry needed the same treatment. It matters more now that a
		// resolved Tier A reference is PERSISTED as the user's designation — an
		// ambiguity that used to last one turn would otherwise stick to the session.
		hits, unresolved := snap.ResolveInstanceRefsInText(text)
		for _, h := range hits {
			explicit = true
			refs = appendDistinctID(refs, h.UHostId)
		}
		if len(unresolved) > 0 {
			explicit = true // a typed id we can't resolve is still an explicit reference
		}
		// A unique exact instance name mentioned in the message.
		if id, ambiguous, present := uniqueRegistryNameInText(text, snap); present {
			explicit = true
			if ambiguous {
				conflict = true
			} else {
				refs = appendDistinctID(refs, id)
			}
		}
	} else if len(snap.InstanceIDTokensInText(text)) > 0 {
		// A cold/stale registry cannot resolve, but an id-shaped token is still an
		// explicit reference: suppress account-single and let existence adjudicate.
		explicit = true
	}

	if conflict || len(refs) > 1 {
		return selectionBinding{conflict: true, explicit: explicit}
	}
	if len(refs) == 1 {
		return selectionBinding{id: refs[0], explicit: explicit}
	}
	if explicit {
		// The user referenced something we could not resolve to one id (typo'd id,
		// out-of-range ordinal): do NOT fall back to carried context. Existence
		// verification rejects it, never account-single overriding an explicit miss.
		return selectionBinding{explicit: true}
	}

	// Tier B — carried context, only when the message names no target.
	var carried []string
	for _, ent := range view.SelectedEntities {
		if ent.Kind != "instance" || strings.TrimSpace(ent.ID) == "" || ent.Freshness == ContinuityFreshnessExpired {
			continue
		}
		// A prior GENUINE user pick, or the account's sole instance. An observed
		// referent (recorded from a read) is deliberately excluded — observed is
		// not chosen — and pending_selection candidates are handled by the explicit
		// ordinal/name matching above, never as bare context.
		if ent.Source == SelectedInstanceSourceUser || ent.Source == selectionSourceAccountSingle {
			carried = appendDistinctID(carried, ent.ID)
		}
	}
	if len(carried) > 1 {
		return selectionBinding{conflict: true}
	}
	if len(carried) == 1 {
		return selectionBinding{id: carried[0]}
	}
	return selectionBinding{}
}

// uniqueRegistryNameInText reports the single instance id whose exact name the text
// mentions (boundary-safe, so "机器" cannot name "机" and "pytest" cannot name
// "test"). present is false when no name matches; ambiguous is true when two
// different instances match (a duplicate name the user must disambiguate).
func uniqueRegistryNameInText(text string, snap entity.RegistrySnapshot) (id string, ambiguous, present bool) {
	seen := map[string]struct{}{}
	var ids []string
	for _, inst := range snap.Instances {
		if strings.TrimSpace(inst.Name) == "" {
			continue
		}
		if !entity.TextExplicitlyMentionsName(text, inst.Name) {
			continue
		}
		if _, done := seen[inst.UHostId]; done {
			continue
		}
		seen[inst.UHostId] = struct{}{}
		ids = append(ids, inst.UHostId)
	}
	switch len(ids) {
	case 0:
		return "", false, false
	case 1:
		return ids[0], false, true
	default:
		return "", true, true
	}
}

func appendDistinctID(ids []string, id string) []string {
	if strings.TrimSpace(id) == "" {
		return ids
	}
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}
