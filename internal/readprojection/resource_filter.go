package readprojection

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

func ApplyResourceFilters(instances []entity.InstanceSnapshot, filters ResourceFilterSet) []entity.InstanceSnapshot {
	if filters.IsZero() {
		return append([]entity.InstanceSnapshot(nil), instances...)
	}
	out := make([]entity.InstanceSnapshot, 0, len(instances))
	for _, inst := range instances {
		if filters.State != "" && !strings.EqualFold(inst.State, filters.State) {
			continue
		}
		if filters.GPUType != "" && !MatchesGPUTypeFilter(inst.GpuType, filters.GPUType) {
			continue
		}
		out = append(out, inst)
	}
	return out
}

func MatchesGPUTypeFilter(actual, filter string) bool {
	if normalizeGPUType(filter) == "" {
		return false
	}
	if normalizeGPUType(actual) == normalizeGPUType(filter) {
		return true
	}
	// Family match, derived from the card-name structure the live catalog produces
	// rather than a hardcoded card: a base model matches its catalog variants because
	// the base's token run appears intact inside the variant's — "4090" matches the
	// cards "4090_48G", "4090Pro" and the brand-prefixed "RTX4090"; "V100" matches
	// "V100S". Tokens split on separators AND letter↔digit transitions, so a shorter
	// number can never masquerade as a prefix of a longer one: "A10" ([a 10]) is not a
	// token run of "A100" ([a 100]), keeping A10≠A100, P40≠P400 and H20≠H200 without
	// naming any card. A filter that already carries a variant suffix ("4090_48G")
	// stays exact — its longer token run is absent from the bare base.
	return containsTokenRun(gpuTypeTokens(actual), gpuTypeTokens(filter))
}

// gpuTypeTokens splits a GPU name into ordered alphanumeric tokens, breaking on
// separators (_ - . space) and on every letter↔digit transition. So "4090_48G" →
// [4090 48 g], "RTX4090" → [rtx 4090], "A100" → [a 100] and "A10" → [a 10]. Keeping
// numbers as whole tokens is what lets a family match tell "4090" inside "4090_48G"
// (a real sub-token) from "A10" inside "A100" (a truncated number, not a token).
func gpuTypeTokens(value string) []string {
	value = strings.ToLower(strings.TrimSpace(value))
	var tokens []string
	var cur []rune
	var curDigit bool
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range value {
		isDigit := r >= '0' && r <= '9'
		isLetter := r >= 'a' && r <= 'z'
		if !isDigit && !isLetter {
			flush() // separator
			continue
		}
		if len(cur) > 0 && isDigit != curDigit {
			flush() // letter↔digit transition
		}
		curDigit = isDigit
		cur = append(cur, r)
	}
	flush()
	return tokens
}

// containsTokenRun reports whether sub appears as a contiguous run inside seq.
func containsTokenRun(seq, sub []string) bool {
	if len(sub) == 0 || len(sub) > len(seq) {
		return false
	}
	for i := 0; i+len(sub) <= len(seq); i++ {
		matched := true
		for j := range sub {
			if seq[i+j] != sub[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func normalizeGPUType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("_", "", "-", "", ".", "", " ", "")
	return replacer.Replace(value)
}
