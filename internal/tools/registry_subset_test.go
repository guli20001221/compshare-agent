package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisibleRegistryForSubset_NilFallsBackToFull(t *testing.T) {
	full := VisibleRegistry(false)
	subset := VisibleRegistryForSubset(nil, false)
	require.Len(t, subset, len(full))
}

func TestVisibleRegistryForSubset_DiagnosisToolsOnly(t *testing.T) {
	// GPU / image / port-firewall / init-failure diagnosis tool defs were removed
	// (migrating to the SSH-ops harness / deleted), so only SSH + Billing survive
	// alongside the read tools.
	subset := VisibleRegistryForSubset([]string{
		"DiagnoseSSH", "DiagnoseBilling",
		"DescribeCompShareInstance", "GetCompShareInstanceMonitor",
		"DescribeCompShareSoftwarePort",
	}, false)

	require.Len(t, subset, 5)
	names := make([]string, 0, len(subset))
	for _, tool := range subset {
		names = append(names, tool.Function.Name)
	}
	assert.Contains(t, names, "DiagnoseSSH")
	assert.Contains(t, names, "DescribeCompShareSoftwarePort")
	assert.NotContains(t, names, "GetCompShareInstancePrice")
	assert.NotContains(t, names, "DescribeAvailableCompShareInstanceTypes")
}

func TestVisibleRegistryForSubset_RespectsReadOnlyFilter(t *testing.T) {
	subset := VisibleRegistryForSubset([]string{
		"StopInstanceWorkflow",
		"DiagnoseSSH",
	}, false)

	names := make([]string, 0, len(subset))
	for _, tool := range subset {
		names = append(names, tool.Function.Name)
	}
	assert.Contains(t, names, "DiagnoseSSH")
	assert.NotContains(t, names, "StopInstanceWorkflow",
		"workflow tools must be hidden in read-only mode even when listed in subset")
}
