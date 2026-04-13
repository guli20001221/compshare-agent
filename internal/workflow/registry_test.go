package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsWorkflowTool(t *testing.T) {
	// All registered workflows should return true
	assert.True(t, IsWorkflowTool("CreateInstanceWorkflow"))
	assert.True(t, IsWorkflowTool("StopInstanceWorkflow"))
	assert.True(t, IsWorkflowTool("StartInstanceWorkflow"))

	// Non-workflow actions should return false
	assert.False(t, IsWorkflowTool("DescribeCompShareInstance"))
	assert.False(t, IsWorkflowTool(""))
}

func TestGetWorkflow(t *testing.T) {
	// CreateInstanceWorkflow: 6 steps
	def, ok := GetWorkflow("CreateInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 6)

	// StopInstanceWorkflow: 3 steps
	def, ok = GetWorkflow("StopInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// StartInstanceWorkflow: 2 steps
	def, ok = GetWorkflow("StartInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 2)

	// Unknown workflow returns nil, false
	def, ok = GetWorkflow("UnknownWorkflow")
	assert.False(t, ok)
	assert.Nil(t, def)
}
