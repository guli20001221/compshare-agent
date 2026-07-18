package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseResourceFiltersAcceptsSupportedFilters(t *testing.T) {
	filters, err := ParseResourceFilters([]TargetRef{
		{Type: TargetRefFilter, Value: "state=running"},
		{Type: TargetRefFilter, Value: "gpu_type=RTX.4090"},
	})

	require.NoError(t, err)
	assert.Equal(t, "running", filters.State)
	assert.Equal(t, "RTX.4090", filters.GPUType)
	assert.Equal(t, []string{"state=running", "gpu_type=RTX.4090"}, filters.Expressions())
}

func TestParseResourceFiltersAcceptsLegacyAliases(t *testing.T) {
	filters, err := ParseResourceFilters([]TargetRef{{Type: TargetRefFilter, Value: "all_stopped"}})

	require.NoError(t, err)
	assert.Equal(t, "stopped", filters.State)
	assert.Equal(t, []string{"state=stopped"}, filters.Expressions())
}

func TestParseResourceFiltersRejectsDuplicateOrConflictingFields(t *testing.T) {
	cases := []struct {
		name string
		refs []TargetRef
	}{
		{
			name: "same field duplicate",
			refs: []TargetRef{
				{Type: TargetRefFilter, Value: "state=running"},
				{Type: TargetRefFilter, Value: "state=running"},
			},
		},
		{
			name: "same field conflict",
			refs: []TargetRef{
				{Type: TargetRefFilter, Value: "state=running"},
				{Type: TargetRefFilter, Value: "state=stopped"},
			},
		},
		{
			name: "filter with explicit target",
			refs: []TargetRef{
				{Type: TargetRefFilter, Value: "state=running"},
				{Type: TargetRefName, Value: "train-a", Source: SourceUserText, SourceSpan: "train-a"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseResourceFilters(tc.refs)
			require.Error(t, err)
		})
	}
}

func TestParseResourceFilterRejectsUnsupportedValues(t *testing.T) {
	for _, value := range []string{
		"",
		"state=deleted",
		"charge_type=Dynamic",
		"gpu_type=",
		"state=running;rm",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseResourceFilter(value)
			require.Error(t, err)
		})
	}
}

func TestMatchesGPUTypeFilterUsesNarrowFamilyRules(t *testing.T) {
	assert.True(t, matchesGPUTypeFilter("4090_48G", "4090"))
	assert.True(t, matchesGPUTypeFilter("RTX4090", "4090"))
	assert.True(t, matchesGPUTypeFilter("4090_48G", "4090_48G"))
	assert.False(t, matchesGPUTypeFilter("4090", "4090_48G"))
	assert.False(t, matchesGPUTypeFilter("A100", "A10"))
	assert.False(t, matchesGPUTypeFilter("P400", "P40"))

	// The family relationship is derived from token structure, not a hardcoded 4090:
	// a base fans out to its variants for ANY card, and the narrow rule (a shorter
	// number is not a prefix of a longer one) holds for card pairs other than A10/A100.
	assert.True(t, matchesGPUTypeFilter("4090Pro", "4090"), "another real 4090 variant is family")
	assert.True(t, matchesGPUTypeFilter("V100S", "V100"), "a non-4090 base fans out to its variant")
	assert.False(t, matchesGPUTypeFilter("V100", "V100S"), "a base does not match a specific-variant filter")
	assert.False(t, matchesGPUTypeFilter("H200", "H20"), "H20 must not match H200, same narrow rule as A10/A100")
}
