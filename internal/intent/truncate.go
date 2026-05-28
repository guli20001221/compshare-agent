package intent

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
)

// DefaultMaxInstancesPerDisplay caps how many instances are surfaced to the
// LLM and user in a single resource-list reply. Aligns with console pagination
// so large accounts don't push the full inventory through token budgets.
const DefaultMaxInstancesPerDisplay = 10

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
