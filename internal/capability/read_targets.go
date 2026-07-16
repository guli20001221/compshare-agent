package capability

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/platform"
)

// resolveReadTargetSnapshots resolves structured TargetRefs to registry
// snapshots. It is the typed-capability twin of the legacy intent
// resolveResourceTargetSnapshots: byte-for-byte the same resolution semantics
// (ID / name resolve, ambiguity + miss handling, dedupe + stable UHostId sort),
// but it returns a structured *platform.ReadFallbackReason instead of an
// *intent.HandlerResult, so a read capability never depends on the intent
// router's result type.
//
// An empty ref list returns (nil, nil, nil): whether that is a fallback is
// request-specific (monitor needs a target; resource_info lists everything), so
// the decision is left to the caller.
func resolveReadTargetSnapshots(refs []platform.TargetRef, resolver EntityResolver) ([]entity.InstanceSnapshot, []string, *platform.ReadFallbackReason) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	if resolver == nil {
		return nil, nil, fallbackReason(platform.ReadFallbackUnresolvedTarget)
	}

	ids := make([]string, 0, len(refs))
	instances := make([]entity.InstanceSnapshot, 0, len(refs))
	for _, ref := range refs {
		switch ref.Type {
		case platform.TargetRefUHostIDUserInput:
			inst, res := resolver.ResolveByID(ref.Value)
			if res.Status != entity.ResolveHit || inst == nil {
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
