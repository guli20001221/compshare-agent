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

type resourceDescribeData = readprojection.ResourceDescribeData

const DefaultMaxInstancesPerDisplay = readprojection.DefaultMaxInstancesPerDisplay

func ParseResourceFilter(v string) (ResourceFilter, error) {
	return readprojection.ParseResourceFilter(v)
}

func ParseResourceFilters(refs []TargetRef) (ResourceFilterSet, error) {
	return readprojection.ParseResourceFilters(refs)
}

func applyResourceFilters(instances []entity.InstanceSnapshot, filters ResourceFilterSet) []entity.InstanceSnapshot {
	return readprojection.ApplyResourceFilters(instances, filters)
}

func matchesGPUTypeFilter(actual, filter string) bool {
	return readprojection.MatchesGPUTypeFilter(actual, filter)
}

func containsFilterRef(refs []TargetRef) bool {
	return readprojection.ContainsFilterRef(refs)
}

func describeResourceArgs(ids []string) map[string]any {
	return readprojection.DescribeResourceArgs(ids)
}

func instancesFromDescribeResult(raw map[string]any) (resourceDescribeData, error) {
	return readprojection.InstancesFromDescribeResult(raw)
}

func TruncateInstancesForDisplay(instances []entity.InstanceSnapshot, limit int) ([]entity.InstanceSnapshot, int, bool) {
	return readprojection.TruncateInstancesForDisplay(instances, limit)
}

func SortInstancesForDisplay(instances []entity.InstanceSnapshot) {
	readprojection.SortInstancesForDisplay(instances)
}

func InstanceDisplayLess(a, b entity.InstanceSnapshot) bool {
	return readprojection.InstanceDisplayLess(a, b)
}
