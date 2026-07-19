package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/compshare-agent/internal/tools"
)

// Dual-proof write-target authority.
//
// A write TARGET (the instance a stop/start/reboot/rename/reset acts on) is
// authorized only when the server independently establishes BOTH:
//
//   SelectionProof  — why we believe the USER chose this target
//   ExistenceProof  — why we believe it currently exists in THIS account
//
// These are orthogonal. "The Agent queried an instance" proves it exists, not
// that the user chose it; "the user typed an id" proves selection, not that it
// still exists. The model-supplied source label is advisory (trace only) and
// never authorizes: deriveProposalProvenance recomputes provenance server-side,
// and the verifier requires both proofs. Network to establish existence lives in
// the engine (verifyTargetExistence), so the actionresolver stays a pure,
// replayable function of its inputs.

// selection sources carried on a SemanticEntityHint. Only a genuine user
// selection (typed id / card pick) or the account's sole fresh instance is a
// SelectionProof. An OBSERVED referent (recorded from a read) is not.
const (
	selectionSourceAccountSingle = "account_registry_single"
	selectionSourcePendingCard   = "pending_selection"
)

// isUserSelectionSource reports whether a SemanticEntityHint.Source represents a
// genuine user selection (or the unambiguous account-single instance), the only
// entity provenance that supplies a SelectionProof. SelectedInstanceSourceObserved
// deliberately returns false: observed != chosen.
func isUserSelectionSource(source string) bool {
	switch source {
	case SelectedInstanceSourceUser, selectionSourcePendingCard, selectionSourceAccountSingle:
		return true
	default:
		return false
	}
}

// existenceKind classifies why a write target is believed to exist in this
// account, verified THIS turn.
type existenceKind int

const (
	existenceNone existenceKind = iota
	// existenceFreshRegistry: the id is in a fresh, complete (absence-authoritative)
	// registry snapshot.
	existenceFreshRegistry
	// existenceCurrentTurnRead: a read capability's response this turn surfaced the
	// id as a subject.
	existenceCurrentTurnRead
	// existenceCurrentTurnDescribe: a point DescribeCompShareInstance(id) this turn
	// returned that same id.
	existenceCurrentTurnDescribe
)

// targetEvidence is the bound existence snapshot the engine produces for a write
// target BEFORE the pure resolver runs. It binds the account, the observation
// time and the exact id so a journal/trace entry can show which account and when
// established that the target really existed. The model can neither forge it nor
// promote it — it is server-owned.
type targetEvidence struct {
	AccountKey   string        `json:"account_key,omitempty"`
	UserEmail    string        `json:"user_email,omitempty"`
	ObservedUnix int64         `json:"observed_unix,omitempty"`
	ExactID      string        `json:"exact_id,omitempty"`
	Existence    existenceKind `json:"existence"`
}

func (e targetEvidence) confirmed() bool { return e.Existence != existenceNone }

// verifyTargetExistence establishes, account-scoped and this-turn, whether an
// exact instance id currently exists. The network call lives HERE, not in the
// resolver:
//
//	fresh+complete registry hit         -> existenceFreshRegistry (no upstream)
//	this-turn read already saw the id   -> existenceCurrentTurnRead (no upstream)
//	fresh+complete registry, id absent  -> existenceNone, NO upstream (authoritative)
//	cold / failed / truncated registry  -> DescribeCompShareInstance(id); the
//	                                       response must carry the SAME id
//
// The point-query runs under the ctx credentials, so a confirmed existence is
// inherently scoped to the caller's account.
func (e *Engine) verifyTargetExistence(ctx context.Context, id string) targetEvidence {
	ev := targetEvidence{ExactID: id, ObservedUnix: time.Now().Unix()}
	if u, ok := tools.UserFrom(ctx); ok {
		ev.AccountKey = fmt.Sprintf("%d/%d", u.TopOrganizationID, u.OrganizationID)
		ev.UserEmail = u.UserEmail
	}
	if id == "" {
		return ev
	}
	snap := e.RegistrySnapshot()
	if _, ok := snap.Instances[id]; ok {
		ev.Existence = existenceFreshRegistry
		return ev
	}
	if _, ok := e.readCapabilitySubjectsThisTurn[id]; ok {
		ev.Existence = existenceCurrentTurnRead
		return ev
	}
	if snap.CanAssertAbsence() {
		// A registry with standing to assert absence says this id is genuinely not
		// in the account — a real "not found", never a wasted point-query.
		return ev
	}
	raw := e.querySafeRead(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds": []string{id}})
	if describeResponseHasID(raw, id) {
		ev.Existence = existenceCurrentTurnDescribe
	}
	return ev
}

// describeResponseHasID reports whether a DescribeCompShareInstance response
// really carries the exact id. Existence is proven only by the upstream RESPONSE
// echoing the same UHostId — never by the request having asked for it (the API
// could ignore an unknown filter and return an empty or unrelated set).
func describeResponseHasID(raw map[string]any, id string) bool {
	if raw == nil || id == "" {
		return false
	}
	set, _ := raw["UHostSet"].([]any)
	for _, row := range set {
		m, _ := row.(map[string]any)
		if m == nil {
			continue
		}
		if got, _ := m["UHostId"].(string); got == id {
			return true
		}
	}
	return false
}
