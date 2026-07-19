package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/tools"
)

// Dual-proof write-target authority.
//
// A write TARGET (the instance/disk/CFS a mutating workflow acts on) reaches the
// server confirmation card only when the server independently establishes that it
// EXISTS in this account this turn (ExistenceProof), and it is authorized to EXECUTE
// only after the user confirms the card. The confirmation card IS the SelectionProof:
// the Agent understands the user and proposes a concrete target; the server verifies
// existence and shows a card naming the exact id; the user's confirm authorizes it.
//
// There is NO source-based gate before the card: a deterministic binding, a carried
// referent and a fresh inference are all verified UNIFORMLY (a source check —
// "is this in SelectedEntities" — would re-introduce candidate-set-membership as a
// trust signal, the exact bug class the P0 dual-proof closed). The SelectionBinder's
// only jobs are to (1) bind/correct a target the user referenced explicitly by
// id/name/ordinal, (2) refuse when two explicit references conflict, and (3) leave the
// Agent's proposed concrete target alone when the user made no deterministic reference.
//
// Existence and the network to establish it live HERE, in the engine, so the
// actionresolver stays a pure, replayable function of its inputs. The model-supplied
// source label is advisory (trace only) and never authorizes: deriveProposalProvenance
// recomputes provenance server-side.

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

// targetEvidenceKey identifies existence evidence by (field, kind, id) — NEVER by a
// bare id string. Two targets of DIFFERENT kinds (an instance UHostId and a disk
// DiskId, say) can carry the same id string; keying by value alone would let the
// instance's existence proof authorize the disk. The field+kind+id triple keeps each
// target's evidence its own, so a lookup can never cross resource kinds.
type targetEvidenceKey struct {
	field string
	kind  string
	id    string
}

// verifyTargetExistence establishes, account-scoped and this-turn, whether a concrete
// write target currently exists, returning a three-state verdict. It runs UNIFORMLY
// for every concrete target the Agent proposes — there is no source-based gate — but
// the verifier is chosen by resource kind, since the three kinds have different
// existence oracles:
//
//	instance -> DescribeCompShareInstance; the RESPONSE must echo the same UHostId. A
//	            same-id-verified resource read this turn, or a fresh+complete registry
//	            hit, short-circuits the point-query.
//	cfs      -> DescribeCFS; the RESPONSE must echo the same CfsId (there is no CFS
//	            registry cache, so this is always a point-query).
//	disk     -> there is no standalone disk describe: a disk lives in its parent
//	            instance's DiskSet. Existence = the (already-identified) parent instance
//	            exists AND its DiskSet carries this exact disk id — verified HERE, before
//	            the card, never deferred to the workflow (which would be a second
//	            target-verification center).
//
// A failed query yields Unavailable so the caller reports a dependency failure, never
// a false "the target does not exist".
func (e *Engine) verifyTargetExistence(ctx context.Context, kind, id, parentInstanceID string) targetEvidence {
	ev := targetEvidence{ExactID: id, ObservedUnix: time.Now().Unix(), Verdict: entity.ExistenceNotFound}
	if u, ok := tools.UserFrom(ctx); ok {
		ev.AccountKey = fmt.Sprintf("%d/%d", u.TopOrganizationID, u.OrganizationID)
		ev.UserEmail = u.UserEmail
	}
	if id == "" {
		return ev
	}
	switch kind {
	case "instance":
		ev.Verdict = e.verifyInstanceExists(ctx, id)
	case "cfs":
		ev.Verdict = e.verifyCFSExists(ctx, id)
	case "disk":
		ev.Verdict = e.verifyDiskExists(ctx, parentInstanceID, id)
	default:
		// An unknown target kind has no verifier: refuse rather than silently verify
		// it as an instance. Existence cannot be confirmed, so the target is rejected —
		// adding a new target kind to the catalog must add its verifier here too.
		ev.Verdict = entity.ExistenceNotFound
	}
	return ev
}

// verifyInstanceExists resolves an exact instance id against the freshness-gated
// registry / a point-query whose response must echo the same UHostId. A resource_info
// response that already echoed the id this turn is existence without re-querying.
func (e *Engine) verifyInstanceExists(ctx context.Context, id string) entity.ExistenceVerdict {
	if _, ok := e.verifiedInstanceEvidenceThisTurn[id]; ok {
		return entity.ExistenceVerified
	}
	now := time.Now()
	snap := e.RegistrySnapshot()
	return entity.VerifyExactTarget(snap, id, now, func(qid string) (map[string]any, bool) {
		return e.querySafeReadResult(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds": []string{qid}})
	})
}

// verifyCFSExists point-queries DescribeCFS; existence is proven only by the RESPONSE
// echoing the same CfsId (the API can ignore an unknown filter and return an empty or
// unrelated set). A failed query is Unavailable, never a false NotFound.
func (e *Engine) verifyCFSExists(ctx context.Context, id string) entity.ExistenceVerdict {
	raw, ok := e.querySafeReadResult(ctx, "DescribeCFS", map[string]any{"CfsId": id})
	if !ok {
		return entity.ExistenceUnavailable
	}
	if describeCFSResponseHasID(raw, id) {
		return entity.ExistenceVerified
	}
	return entity.ExistenceNotFound
}

// verifyDiskExists verifies a disk id against its parent instance's DiskSet. There is
// no standalone disk describe (disks are returned inside DescribeCompShareInstance),
// so existence = the parent instance is present in the response AND its DiskSet carries
// the exact disk id. This runs at proposal time, before the confirmation card — the
// disk is dual-proven (existence here, selection on the card) exactly like an instance,
// never punted to the workflow's execution stage.
func (e *Engine) verifyDiskExists(ctx context.Context, parentInstanceID, diskID string) entity.ExistenceVerdict {
	if strings.TrimSpace(parentInstanceID) == "" {
		// No identified parent instance to scope the disk to — existence cannot be proven.
		return entity.ExistenceNotFound
	}
	raw, ok := e.querySafeReadResult(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds": []string{parentInstanceID}})
	if !ok {
		return entity.ExistenceUnavailable
	}
	if !entity.DescribeResponseHasID(raw, parentInstanceID) {
		// The parent instance itself is unconfirmed — the disk under it cannot be proven.
		return entity.ExistenceNotFound
	}
	if describeInstanceDiskSetHasID(raw, parentInstanceID, diskID) {
		return entity.ExistenceVerified
	}
	return entity.ExistenceNotFound
}

// describeCFSResponseHasID reports whether a DescribeCFS response really carries the
// exact CfsId — via a CFSSet[] row echoing CfsId/CFSId, or a flat single-CFS object.
// Existence is proven by the RESPONSE echoing the id, never by the request asking.
func describeCFSResponseHasID(raw map[string]any, id string) bool {
	if raw == nil || strings.TrimSpace(id) == "" {
		return false
	}
	if rowHasCFSID(raw, id) {
		return true
	}
	set, _ := raw["CFSSet"].([]any)
	for _, row := range set {
		if m, ok := row.(map[string]any); ok && rowHasCFSID(m, id) {
			return true
		}
	}
	return false
}

func rowHasCFSID(m map[string]any, id string) bool {
	for _, key := range []string{"CfsId", "CFSId"} {
		if got, _ := m[key].(string); got == id {
			return true
		}
	}
	return false
}

// describeInstanceDiskSetHasID reports whether the DescribeCompShareInstance response
// for the parent instance carries a disk with the exact id in its DiskSet. Upstream
// exposes disk ids under several keys (DiskId/UDiskId/Id/DiskShortId), matching the
// workflow's own findDiskByID.
func describeInstanceDiskSetHasID(raw map[string]any, instanceID, diskID string) bool {
	if raw == nil || strings.TrimSpace(diskID) == "" {
		return false
	}
	set, _ := raw["UHostSet"].([]any)
	for _, row := range set {
		host, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if got, _ := host["UHostId"].(string); got != instanceID {
			continue
		}
		disks, _ := host["DiskSet"].([]any)
		for _, d := range disks {
			disk, ok := d.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"DiskId", "UDiskId", "Id", "DiskShortId"} {
				if got, _ := disk[key].(string); got == diskID {
					return true
				}
			}
		}
	}
	return false
}
