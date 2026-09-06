package readprojection

import (
	"testing"

	"github.com/compshare-agent/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseResourceFiltersAcceptsSupportedFilters(t *testing.T) {
	filters, err := ParseResourceFilters([]platform.TargetRef{
		{Type: platform.TargetRefFilter, Value: "state=running"},
		{Type: platform.TargetRefFilter, Value: "gpu_type=RTX.4090"},
	})

	require.NoError(t, err)
	assert.Equal(t, "running", filters.State)
	assert.Equal(t, "RTX.4090", filters.GPUType)
	assert.Equal(t, []string{"state=running", "gpu_type=RTX.4090"}, filters.Expressions())
}

func TestParseResourceFiltersAcceptsLegacyAliases(t *testing.T) {
	filters, err := ParseResourceFilters([]platform.TargetRef{{Type: platform.TargetRefFilter, Value: "all_stopped"}})

	require.NoError(t, err)
	assert.Equal(t, "stopped", filters.State)
	assert.Equal(t, []string{"state=stopped"}, filters.Expressions())
}

func TestParseResourceFiltersRejectsDuplicateOrConflictingFields(t *testing.T) {
	cases := []struct {
		name string
		refs []platform.TargetRef
	}{
		{
			name: "same field duplicate",
			refs: []platform.TargetRef{
				{Type: platform.TargetRefFilter, Value: "state=running"},
				{Type: platform.TargetRefFilter, Value: "state=running"},
			},
		},
		{
			name: "same field conflict",
			refs: []platform.TargetRef{
				{Type: platform.TargetRefFilter, Value: "state=running"},
				{Type: platform.TargetRefFilter, Value: "state=stopped"},
			},
		},
		{
			name: "filter with explicit target",
			refs: []platform.TargetRef{
				{Type: platform.TargetRefFilter, Value: "state=running"},
				{Type: platform.TargetRefName, Value: "train-a", Source: platform.SourceUserText},
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
	assert.True(t, MatchesGPUTypeFilter("4090_48G", "4090"))
	assert.True(t, MatchesGPUTypeFilter("RTX4090", "4090"))
	assert.True(t, MatchesGPUTypeFilter("4090_48G", "4090_48G"))
	assert.False(t, MatchesGPUTypeFilter("4090", "4090_48G"))
	assert.False(t, MatchesGPUTypeFilter("A100", "A10"))
	assert.False(t, MatchesGPUTypeFilter("P400", "P40"))

	// The family relationship is derived from token structure, not a hardcoded 4090:
	// a base fans out to its variants for ANY card, and the narrow rule (a shorter
	// number is not a prefix of a longer one) holds for card pairs other than A10/A100.
	assert.True(t, MatchesGPUTypeFilter("4090Pro", "4090"), "another real 4090 variant is family")
	assert.True(t, MatchesGPUTypeFilter("V100S", "V100"), "a non-4090 base fans out to its variant")
	assert.False(t, MatchesGPUTypeFilter("V100", "V100S"), "a base does not match a specific-variant filter")
	assert.False(t, MatchesGPUTypeFilter("H200", "H20"), "H20 must not match H200, same narrow rule as A10/A100")
}
