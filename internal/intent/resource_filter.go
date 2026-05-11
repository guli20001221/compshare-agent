package intent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/entity"
)

type ResourceFilter struct {
	Field      string
	Value      string
	Expression string
}

type ResourceFilterSet struct {
	State   string
	GPUType string
}

var gpuTypeFilterValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func ParseResourceFilter(value string) (ResourceFilter, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ResourceFilter{}, fmt.Errorf("empty resource filter")
	}
	normalized := strings.ToLower(raw)
	switch normalized {
	case "all":
		return ResourceFilter{Field: "all", Value: "all", Expression: "all"}, nil
	case "all_running":
		return ResourceFilter{Field: "state", Value: "running", Expression: "state=running"}, nil
	case "all_stopped":
		return ResourceFilter{Field: "state", Value: "stopped", Expression: "state=stopped"}, nil
	}

	field, filterValue, ok := strings.Cut(raw, "=")
	if !ok {
		return ResourceFilter{}, fmt.Errorf("unsupported resource filter %q", value)
	}
	field = strings.ToLower(strings.TrimSpace(field))
	filterValue = strings.TrimSpace(filterValue)
	switch field {
	case "state":
		state := strings.ToLower(filterValue)
		switch state {
		case "running", "stopped":
			return ResourceFilter{Field: "state", Value: state, Expression: "state=" + state}, nil
		default:
			return ResourceFilter{}, fmt.Errorf("unsupported state filter %q", filterValue)
		}
	case "gpu_type":
		if filterValue == "" || !gpuTypeFilterValuePattern.MatchString(filterValue) {
			return ResourceFilter{}, fmt.Errorf("unsupported gpu_type filter %q", filterValue)
		}
		return ResourceFilter{Field: "gpu_type", Value: filterValue, Expression: "gpu_type=" + filterValue}, nil
	default:
		return ResourceFilter{}, fmt.Errorf("unsupported resource filter field %q", field)
	}
}

func ParseResourceFilters(refs []TargetRef) (ResourceFilterSet, error) {
	var filters ResourceFilterSet
	seenFields := map[string]struct{}{}
	for _, ref := range refs {
		if ref.Type != TargetRefFilter {
			return ResourceFilterSet{}, fmt.Errorf("resource filters cannot be mixed with explicit target refs")
		}
		filter, err := ParseResourceFilter(ref.Value)
		if err != nil {
			return ResourceFilterSet{}, err
		}
		if filter.Field == "all" {
			continue
		}
		if _, ok := seenFields[filter.Field]; ok {
			return ResourceFilterSet{}, fmt.Errorf("duplicate resource filter field %q", filter.Field)
		}
		seenFields[filter.Field] = struct{}{}
		switch filter.Field {
		case "state":
			filters.State = filter.Value
		case "gpu_type":
			filters.GPUType = filter.Value
		}
	}
	return filters, nil
}

func (f ResourceFilterSet) IsZero() bool {
	return f.State == "" && f.GPUType == ""
}

func (f ResourceFilterSet) Expressions() []string {
	var values []string
	if f.State != "" {
		values = append(values, "state="+f.State)
	}
	if f.GPUType != "" {
		values = append(values, "gpu_type="+f.GPUType)
	}
	return values
}

func (f ResourceFilterSet) String() string {
	return strings.Join(f.Expressions(), ",")
}

func applyResourceFilters(instances []entity.InstanceSnapshot, filters ResourceFilterSet) []entity.InstanceSnapshot {
	if filters.IsZero() {
		return append([]entity.InstanceSnapshot(nil), instances...)
	}
	out := make([]entity.InstanceSnapshot, 0, len(instances))
	for _, inst := range instances {
		if filters.State != "" && !strings.EqualFold(inst.State, filters.State) {
			continue
		}
		if filters.GPUType != "" && !strings.EqualFold(inst.GpuType, filters.GPUType) {
			continue
		}
		out = append(out, inst)
	}
	return out
}
