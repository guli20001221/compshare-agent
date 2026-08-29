package capability

import (
	"context"
	"sort"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
)

// resolveReadTargetSnapshots resolves IDs and names with stable deduplication.
// An empty ref list is valid; each caller decides whether it needs a target.
//
// A registry that cannot assert absence must not turn a cache miss into a
// refusal. Exact IDs pass through for an upstream point query. Names require a
// full-list sync before resolution because the upstream query accepts IDs only.
// A fresh, complete registry remains authoritative.
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
	raw, err := describeAllAccountInstances(ctx, rt.Executor)
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
	instances := make([]entity.InstanceSnapshot, 0, len(refs))
	for _, ref := range refs {
		switch ref.Type {
		case platform.TargetRefUHostIDUserInput:
			inst, res := resolver.ResolveByID(ref.Value)
			if res.Status != entity.ResolveHit || inst == nil {
				if !resolver.CanAssertAbsenceAt(now) {
					// Unverifiable locally, not absent: pass the exact id through so
					// the caller's DescribeCompShareInstance point-query decides.
					instances = append(instances, entity.InstanceSnapshot{UHostId: ref.Value})
					continue
				}
				return nil, nil, fallbackReason(platform.ReadFallbackUnresolvedTarget)
			}
			instances = append(instances, *inst)
		case platform.TargetRefName:
			matches, res := resolver.ResolveByName(ref.Value)
			if res.Status == entity.ResolveAmbiguous || len(matches) > 1 {
				return nil, nil, fallbackReason(platform.ReadFallbackAmbiguousTarget)
			}
			if res.Status != entity.ResolveHit || len(matches) == 0 || matches[0] == nil {
				return nil, nil, fallbackReason(platform.ReadFallbackUnresolvedTarget)
			}
			instances = append(instances, *matches[0])
		default:
			return nil, nil, fallbackReason(platform.ReadFallbackValidation)
		}
	}
	instances = dedupeInstanceSnapshots(instances)
	ids := make([]string, 0, len(instances))
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

// dedupeInstanceSnapshots removes duplicate UHostIds in first-seen order.
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
