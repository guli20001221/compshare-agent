package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntentToolSubset_DiagnosisReturns9Tools(t *testing.T) {
	subset := IntentToolSubset(IntentDiagnosis)
	require.Len(t, subset, 9)
	assert.Contains(t, subset, "DiagnoseSSH")
	assert.Contains(t, subset, "DiagnosePortOrFirewall")
	assert.Contains(t, subset, "DescribeCompShareInstance")
	assert.Contains(t, subset, "DescribeCompShareSoftwarePort")
	assert.NotContains(t, subset, "GetCompShareInstancePrice")
	assert.NotContains(t, subset, "DescribeCompShareImages")
	assert.NotContains(t, subset, "CreateInstanceWorkflow")
}

func TestIntentToolSubset_VagueFailureSameAsDiagnosis(t *testing.T) {
	assert.Equal(t, IntentToolSubset(IntentDiagnosis), IntentToolSubset(IntentVagueFailure))
}

func TestIntentToolSubset_UnknownReturnsNil(t *testing.T) {
	assert.Nil(t, IntentToolSubset(IntentUnknown))
	assert.Nil(t, IntentToolSubset(IntentGPUSpecsQuery))
	assert.Nil(t, IntentToolSubset(IntentResourceInfo))
	assert.Nil(t, IntentToolSubset(""))
}
