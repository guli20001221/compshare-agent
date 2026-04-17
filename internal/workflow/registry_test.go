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
	assert.True(t, IsWorkflowTool("RebootInstanceWorkflow"))
	assert.True(t, IsWorkflowTool("RenameInstanceWorkflow"))
	assert.True(t, IsWorkflowTool("ResetPasswordWorkflow"))
	assert.True(t, IsWorkflowTool("SetStopSchedulerWorkflow"))
	assert.True(t, IsWorkflowTool("CancelStopSchedulerWorkflow"))

	// Non-workflow actions should return false
	assert.False(t, IsWorkflowTool("DescribeCompShareInstance"))
	assert.False(t, IsWorkflowTool(""))
}

func TestGetWorkflow(t *testing.T) {
	// CreateInstanceWorkflow: 7 steps
	def, ok := GetWorkflow("CreateInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 7)

	// StopInstanceWorkflow: 3 steps
	def, ok = GetWorkflow("StopInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// StartInstanceWorkflow: 3 steps
	def, ok = GetWorkflow("StartInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// RebootInstanceWorkflow: 3 steps
	def, ok = GetWorkflow("RebootInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// RenameInstanceWorkflow: 3 steps
	def, ok = GetWorkflow("RenameInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// ResetPasswordWorkflow: 4 steps
	def, ok = GetWorkflow("ResetPasswordWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 4)

	// SetStopSchedulerWorkflow: 3 steps
	def, ok = GetWorkflow("SetStopSchedulerWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// CancelStopSchedulerWorkflow: 3 steps
	def, ok = GetWorkflow("CancelStopSchedulerWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// Unknown workflow returns nil, false
	def, ok = GetWorkflow("UnknownWorkflow")
	assert.False(t, ok)
	assert.Nil(t, def)
}
