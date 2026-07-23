package readprojection

import (
	"sort"

	"github.com/compshare-agent/internal/entity"
)

func DescribeResourceArgs(ids []string) map[string]any {
	if len(ids) == 0 {
		return map[string]any{"Limit": 100}
	}
	return map[string]any{"UHostIds": append([]string(nil), ids...)}
}

type ResourceDescribeData struct {
	Instances  []entity.InstanceSnapshot
	TotalCount int
	Truncated  bool
}

func InstancesFromDescribeResult(raw map[string]any) (ResourceDescribeData, error) {
	reg := entity.NewRegistry()
	if err := reg.SyncFromDescribe(raw, "handler_resource"); err != nil {
		return ResourceDescribeData{}, err
	}
	snap := reg.Snapshot()
	instances := make([]entity.InstanceSnapshot, 0, len(snap.Instances))
	for _, inst := range snap.Instances {
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].UHostId < instances[j].UHostId
	})
	totalCount := snap.TotalCount
	if totalCount == 0 && len(instances) > 0 {
		totalCount = len(instances)
	}
	return ResourceDescribeData{
		Instances:  instances,
		TotalCount: totalCount,
		Truncated:  snap.Truncated,
	}, nil
}

func ContainsFilterRef(refs []TargetRef) bool {
	for _, ref := range refs {
		if ref.Type == TargetRefFilter {
			return true
		}
	}
	return false
}
