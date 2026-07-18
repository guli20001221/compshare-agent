package intent

import (
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/readprojection"
)

// The resource-read input pipeline (filter parsing, describe-args, describe-result
// decoding, display truncation) now has a single implementation in
// internal/readprojection. These forwarders keep the legacy handlers/validator
// and their tests compiling with the original bare names; new read capabilities
// call readprojection directly.

type ResourceFilter = readprojection.ResourceFilter

type ResourceFilterSet = readprojection.ResourceFilterSet

const DefaultMaxInstancesPerDisplay = readprojection.DefaultMaxInstancesPerDisplay

func ParseResourceFilter(v string) (ResourceFilter, error) {
	return readprojection.ParseResourceFilter(v)
}

func ParseResourceFilters(refs []TargetRef) (ResourceFilterSet, error) {
	return readprojection.ParseResourceFilters(refs)
}

func matchesGPUTypeFilter(actual, filter string) bool {
	return readprojection.MatchesGPUTypeFilter(actual, filter)
}

func TruncateInstancesForDisplay(instances []entity.InstanceSnapshot, limit int) ([]entity.InstanceSnapshot, int, bool) {
	return readprojection.TruncateInstancesForDisplay(instances, limit)
}

func SortInstancesForDisplay(instances []entity.InstanceSnapshot) {
	readprojection.SortInstancesForDisplay(instances)
}
