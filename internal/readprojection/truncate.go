package readprojection

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
)

// DefaultMaxInstancesPerDisplay caps how many instances are surfaced to the
// LLM and user in a single resource-list reply.
//
// It was 10, chosen to align with console pagination and to keep large accounts
// out of the token budget. That cost was measured on a real 18-instance account
// 2026-07-30 and it is not a display cost — the cap runs BEFORE the read result
// is built (read_resource.go), so instances 11..18 reach neither the reply nor
// the Agent. Three separate failures, all one root cause:
//
//   - 「host-不要删除验证七天回收」 is the account's only uniquely-named instance and
//     sorts 18th. Asked for its state, the Agent answered 「暂时无法按这个名称定位到
//     实例」 5 runs out of 5. It was not a reasoning failure; the row was gone.
//   - 「我那台 4090 的内存是多少」 could only ever see 7 of the 12 4090s.
//   - 「我有哪些实例」 answered 10/18 and sent the user to the console.
//
// 50 because the token pressure that motivated 10 no longer binds, and because a
// cap that silently removes the answer is worse than a long reply. The truncation
// notice still fires above it, so a genuinely large account is told what it did
// not see rather than shown a short list that looks complete.
const DefaultMaxInstancesPerDisplay = 50

// stateDisplayRank orders instance states so the most likely operation
// targets show first: Running > Stopped > Install > Install Fail > Starting
// > Stopping > Rebooting > others. Operation intents (关机/重启) most often
// target Running; rebuild/start most often target Stopped — keeping these
// at the top reduces the chance of an interesting instance being truncated.
func stateDisplayRank(state string) int {
	switch state {
	case "Running":
		return 0
	case "Stopped":
		return 1
	case "Install":
		return 2
	case "Install Fail":
		return 3
	case "Starting":
		return 4
	case "Stopping":
		return 5
	case "Rebooting":
		return 6
	default:
		return 100
	}
}

// InstanceDisplayLess reports whether a should sort before b in the
// display order: state priority first, then StartTime DESC (zero last),
// then UHostId ASC as a stable tiebreaker. Exposed so callers operating
// on raw map rows (e.g. ReAct tool-result post-processors) can share the
// same ordering without re-implementing it.
func InstanceDisplayLess(a, b entity.InstanceSnapshot) bool {
	ra, rb := stateDisplayRank(a.State), stateDisplayRank(b.State)
	if ra != rb {
		return ra < rb
	}
	sa, sb := a.StartTime, b.StartTime
	switch {
	case sa == 0 && sb != 0:
		return false
	case sa != 0 && sb == 0:
		return true
	case sa != sb:
		return sa > sb
	}
	return a.UHostId < b.UHostId
}

// SortInstancesForDisplay orders instances in place using InstanceDisplayLess.
func SortInstancesForDisplay(instances []entity.InstanceSnapshot) {
	sort.SliceStable(instances, func(i, j int) bool {
		return InstanceDisplayLess(instances[i], instances[j])
	})
}

// TruncateInstancesForDisplay returns a sorted-and-truncated copy of the
// instance list along with how many were kept and whether truncation
// happened. limit <= 0 falls back to DefaultMaxInstancesPerDisplay. The
// input slice is not mutated.
func TruncateInstancesForDisplay(instances []entity.InstanceSnapshot, limit int) (out []entity.InstanceSnapshot, shown int, truncated bool) {
	if limit <= 0 {
		limit = DefaultMaxInstancesPerDisplay
	}
	out = append([]entity.InstanceSnapshot(nil), instances...)
	SortInstancesForDisplay(out)
	if len(out) <= limit {
		return out, len(out), false
	}
	return out[:limit], limit, true
}
