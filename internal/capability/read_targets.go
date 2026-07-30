package capability

import (
	"context"
	"sort"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/readprojection"
)

// resolveReadTargetSnapshots resolves structured TargetRefs to registry
// snapshots. It began as the typed-capability twin of the legacy intent
// resolveResourceTargetSnapshots and keeps its shape (ID / name resolve,
// ambiguity handling, dedupe + stable UHostId sort) and its structured
// *platform.ReadFallbackReason return, so a read capability never depends on the
// intent router's result type. MISS handling deliberately no longer matches the
// legacy behaviour — that parity is what the invariant below had to break.
//
// An empty ref list returns (nil, nil, nil): whether that is a fallback is
// request-specific (monitor needs a target; resource_info lists everything), so
// the decision is left to the caller.
//
// THE INVARIANT, stated once: a registry WITHOUT standing to assert absence
// (never-synced / stale / truncated — see entity.CanAssertAbsence) must never
// turn a lookup into a refusal. "I have not seen it" is not "it does not exist",
// and refusing on it produces the worst failure this system has: the user names
// an instance they are looking at and is told it cannot be found.
//
// It is enforced here in two shapes, because the two ref kinds can be rescued
// differently:
//
//   - an exact ID passes THROUGH: the caller's DescribeCompShareInstance point-
//     query confirms or denies it. The synthesized snapshot carries only the id;
//     callers derive the real instance from the response (monitor re-Describes
//     via monitorTargetsNeedVerification, resource_info re-parses, refund renders
//     RefundPriceSet rows and only decorates labels with names it has).
//   - a NAME cannot pass through — DescribeCompShareInstance takes ids, so there
//     is nothing to hand upstream. Instead we sync one full listing and re-resolve
//     against it. Whatever the warm registry then says is a fact about the
//     account rather than an artefact of an empty cache.
//
// The production HTTP/WS path never calls engine.Init(), so its registry is cold
// for the whole session unless something warms it: the name branch's warm-up is
// the ordinary path there, not an edge case. Measured 2026-07-29: 「host-不要删除
// 验证七天回收 这台实例现在是什么状态」 answered 「暂时无法按这个名称定位到实例」 5/5
// with ZERO upstream calls, while the same instance by id answered normally and
// the same name against a warm registry resolved exactly. The matcher was never
// the problem.
//
// A registry WITH standing stays authoritative — a real miss is a real "not in
// your account", and no upstream call is made to second-guess it.
//
// rt.Now is the freshness reference clock: absence is asserted only against a
// registry still fresh as of that instant, so a stale-but-complete snapshot no
// longer refuses a just-created id before the upstream call.
func resolveReadTargetSnapshots(ctx context.Context, refs []platform.TargetRef, rt ReadRuntime) ([]entity.InstanceSnapshot, []string, *platform.ReadFallbackReason) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	if rt.Resolver == nil {
		return nil, nil, fallbackReason(platform.ReadFallbackUnresolvedTarget)
	}
	now := rt.Now
	if now.IsZero() {
		// A caller that forgot to wire the clock must not silently re-open the
		// stale-trust hole: default to real time so absence stays freshness-gated.
		now = time.Now()
	}

	instances, ids, reason := resolveTargetsAgainst(refs, rt.Resolver, now)
	if reason == nil || !warmupCanChangeAnswer(*reason) || rt.Resolver.CanAssertAbsenceAt(now) {
		return instances, ids, reason
	}
	warm, ok := warmedResolver(ctx, rt)
	if !ok {
		// Best effort: an upstream that will not list is no reason to answer
		// worse than before, so the original outcome stands.
		return nil, nil, reason
	}
	// Re-resolve everything, not just the ref that missed: the warm snapshot is
	// strictly better evidence for every ref in the request.
	return resolveTargetsAgainst(refs, warm, now)
}

// warmupCanChangeAnswer reports whether re-resolving against a complete listing
// could produce a different outcome. A miss can become a hit; an ambiguity read
// off a truncated registry can collapse to one exact-name match. A malformed ref
// type is a client error that no amount of upstream data fixes.
func warmupCanChangeAnswer(reason platform.ReadFallbackReason) bool {
	return reason == platform.ReadFallbackUnresolvedTarget || reason == platform.ReadFallbackAmbiguousTarget
}

// warmedResolver lists the account once and returns a resolver built from that
// response. It reports false — never an error — on anything that goes wrong: the
// warm-up is an attempt to answer better, so its failure must leave the caller
// exactly where it was rather than convert a resolution miss into a read failure.
func warmedResolver(ctx context.Context, rt ReadRuntime) (EntityResolver, bool) {
	if rt.Executor == nil {
		return nil, false
	}
	raw, err := rt.Executor.Execute(ctx, resourceInfoAction, readprojection.DescribeResourceArgs(nil))
	if err != nil {
		return nil, false
	}
	registry := entity.NewRegistry()
	if err := registry.SyncFromDescribe(raw, string(entity.SyncEventSyncRefresh)); err != nil {
		return nil, false
	}
	if rt.SyncRegistry != nil {
		// Hand the same listing to the session's own registry so the next turn
		// resolves names without repeating this call.
		rt.SyncRegistry(raw)
	}
	return registry.Snapshot(), true
}

// resolveTargetsAgainst is the pure matching pass: one specific resolver, no
// upstream calls, no retry.
func resolveTargetsAgainst(refs []platform.TargetRef, resolver EntityResolver, now time.Time) ([]entity.InstanceSnapshot, []string, *platform.ReadFallbackReason) {
	ids := make([]string, 0, len(refs))
	instances := make([]entity.InstanceSnapshot, 0, len(refs))
	for _, ref := range refs {
		switch ref.Type {
		case platform.TargetRefUHostIDUserInput:
			inst, res := resolver.ResolveByID(ref.Value)
			if res.Status != entity.ResolveHit || inst == nil {
				if !resolver.CanAssertAbsenceAt(now) {
					// Unverifiable locally, not absent: pass the exact id through so
					// the caller's DescribeCompShareInstance point-query decides.
					ids = append(ids, ref.Value)
					instances = append(instances, entity.InstanceSnapshot{UHostId: ref.Value})
					continue
				}
				return nil, nil, fallbackReason(platform.ReadFallbackUnresolvedTarget)
			}
			ids = append(ids, inst.UHostId)
			instances = append(instances, *inst)
		case platform.TargetRefName:
			matches, res := resolver.ResolveByName(ref.Value)
			if res.Status == entity.ResolveAmbiguous || len(matches) > 1 {
				return nil, nil, fallbackReason(platform.ReadFallbackAmbiguousTarget)
			}
			if res.Status != entity.ResolveHit || len(matches) == 0 || matches[0] == nil {
				return nil, nil, fallbackReason(platform.ReadFallbackUnresolvedTarget)
			}
			ids = append(ids, matches[0].UHostId)
			instances = append(instances, *matches[0])
		default:
			return nil, nil, fallbackReason(platform.ReadFallbackValidation)
		}
	}
	instances = dedupeInstanceSnapshots(instances)
	ids = make([]string, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.UHostId)
	}
	sort.Strings(ids)
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].UHostId < instances[j].UHostId
	})
	return instances, ids, nil
}

func fallbackReason(reason platform.ReadFallbackReason) *platform.ReadFallbackReason {
	return &reason
}

// readTargetFallbackResult maps a target-resolution fallback reason to a read
// result. An ambiguous reference — the request resolves to more than one
// instance — is a structured Conflict so the Agent asks which one, asserted as a
// status rather than only a fallback reason plus follow-up prose. Every other
// reason stays a pre-tool fallback.
func readTargetFallbackResult(reason platform.ReadFallbackReason) ReadResult {
	if reason == platform.ReadFallbackAmbiguousTarget {
		return ReadConflict("你指定的名称匹配到多个实例，请用实例 ID 或更精确的名称重新指定要操作的实例。")
	}
	return ReadFallbackBeforeTool(reason)
}

// dedupeInstanceSnapshots removes duplicate snapshots by UHostId, preserving
// first-seen order. Relocated verbatim from intent.dedupeInstanceSnapshots so a
// migrated capability resolves targets without the intent package.
func dedupeInstanceSnapshots(values []entity.InstanceSnapshot) []entity.InstanceSnapshot {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]entity.InstanceSnapshot, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value.UHostId]; ok {
			continue
		}
		seen[value.UHostId] = struct{}{}
		out = append(out, value)
	}
	return out
}
