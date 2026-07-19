package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/tools"
)

// Dual-proof write-target authority.
//
// A write TARGET (the instance a stop/start/reboot/rename/reset acts on) reaches
// the server confirmation card only when the server independently establishes that
// it EXISTS in this account this turn (ExistenceProof), and it is authorized to
// EXECUTE only after the user confirms the card. Two ways a target earns the card:
//
//   - the SelectionBinder deterministically bound the user's reference (a typed id,
//     an ordinal against a shown list, a unique instance name, a prior explicit
//     pick) — a server-derived SelectionProof; or
//   - the Agent inferred a concrete existing target from context — no deterministic
//     SelectionProof, so the user-confirm event on the card IS the SelectionProof.
//
// Either way, existence is required and network to establish it lives HERE, in the
// engine (verifyTargetExistence), so the actionresolver stays a pure, replayable
// function of its inputs. The model-supplied source label is advisory (trace only)
// and never authorizes: deriveProposalProvenance recomputes provenance server-side.

// targetEvidence is the bound existence snapshot the engine produces for a write
// target BEFORE the pure resolver runs. It binds the account, the observation time
// and the exact id so a journal/trace entry can show which account and when
// established the target's existence verdict. The model can neither forge it nor
// promote it — it is server-owned.
type targetEvidence struct {
	AccountKey   string                  `json:"account_key,omitempty"`
	UserEmail    string                  `json:"user_email,omitempty"`
	ObservedUnix int64                   `json:"observed_unix,omitempty"`
	ExactID      string                  `json:"exact_id,omitempty"`
	Verdict      entity.ExistenceVerdict `json:"verdict"`
}

func (e targetEvidence) confirmed() bool { return e.Verdict == entity.ExistenceVerified }

// verifyTargetExistence establishes, account-scoped and this-turn, whether an exact
// instance id currently exists, returning a three-state verdict. It draws a hard
// line between a SELECTED target and an Agent-INFERRED one:
//
//	same-id-verified read this turn      -> Verified   (ANY target: the Agent
//	                                        actively confirmed existence this turn)
//	inferred target, nothing verified    -> NotFound   (a bare background-registry
//	                                        hit does NOT authorize an unselected id —
//	                                        that is how a model self-elects a target;
//	                                        it must be actively verified or bound)
//	selected + fresh+complete hit        -> Verified   (no network)
//	selected + fresh+complete, absent    -> NotFound   (authoritative, no network)
//	selected + cold/stale                -> point Describe(id); response must echo
//	                                        the SAME id (Verified) else NotFound; a
//	                                        failed query is Unavailable.
//
// allowPointQuery marks the SelectionProof: an id the user genuinely referenced may
// use the fresh registry / a point-query for existence. An inferred id may not —
// its ONLY existence path is a same-id-verified read this turn ("已核实"), so a
// model naming an arbitrary existing id neither point-queries nor rides the
// background registry into the card. A failed point-query yields Unavailable so the
// caller reports a dependency failure, never a false "the instance does not exist".
func (e *Engine) verifyTargetExistence(ctx context.Context, id string, allowPointQuery bool) targetEvidence {
	ev := targetEvidence{ExactID: id, ObservedUnix: time.Now().Unix(), Verdict: entity.ExistenceNotFound}
	if u, ok := tools.UserFrom(ctx); ok {
		ev.AccountKey = fmt.Sprintf("%d/%d", u.TopOrganizationID, u.OrganizationID)
		ev.UserEmail = u.UserEmail
	}
	if id == "" {
		return ev
	}
	// A same-id-verified read this turn is existence for ANY target — the only way an
	// Agent-inferred (unselected) target earns the confirmation card.
	if _, ok := e.verifiedInstanceEvidenceThisTurn[id]; ok {
		ev.Verdict = entity.ExistenceVerified
		return ev
	}
	if !allowPointQuery {
		// Inferred target, not actively verified: refuse. NOT an outage, and NOT
		// authorized by a bare background-registry hit (self-election guard).
		return ev
	}
	now := time.Now()
	snap := e.RegistrySnapshot()
	if snap.FreshAndCompleteAt(now) {
		if _, ok := snap.Instances[id]; ok {
			ev.Verdict = entity.ExistenceVerified
		} else {
			ev.Verdict = entity.ExistenceNotFound // authoritative absence, no point-query
		}
		return ev
	}
	ev.Verdict = entity.VerifyExactTarget(snap, id, now, func(qid string) (map[string]any, bool) {
		return e.querySafeReadResult(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds": []string{qid}})
	})
	return ev
}
