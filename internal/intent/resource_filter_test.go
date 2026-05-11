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
