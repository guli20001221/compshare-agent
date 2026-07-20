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
	assert.True(t, IsWorkflowTool("ResizeDiskWorkflow"))
	assert.True(t, IsWorkflowTool("CreateCustomImageWorkflow"))

	// Non-workflow actions should return false
	assert.False(t, IsWorkflowTool("DescribeCompShareInstance"))
	assert.False(t, IsWorkflowTool(""))
}

func TestRegisteredWorkflowActionsMatchRegistry(t *testing.T) {
	actions := RegisteredWorkflowActions()
	seen := map[string]bool{}
	for _, action := range actions {
		assert.True(t, IsWorkflowTool(action), "registered action list contains unknown workflow %s", action)
		assert.False(t, seen[action], "duplicate workflow action %s", action)
		seen[action] = true
	}
	assert.Len(t, actions, len(workflowRegistry), "registered workflow action list must match registry size")
	for action := range workflowRegistry {
		assert.True(t, seen[action], "workflow action %s missing from stable list", action)
	}
}

func TestGetWorkflow(t *testing.T) {
	// CreateInstanceWorkflow: 9 steps
	def, ok := GetWorkflow("CreateInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 9)

	// StopInstanceWorkflow: query -> query support zones -> confirm -> stop
	def, ok = GetWorkflow("StopInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 4)

	// StartInstanceWorkflow: query -> confirm -> start (no-GPU spec passed
	// directly on start; upstream resizes internally, no separate step)
	def, ok = GetWorkflow("StartInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// RebootInstanceWorkflow: query -> query support zones -> confirm -> reboot
	def, ok = GetWorkflow("RebootInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 4)

	// RenameInstanceWorkflow: query -> query support zones -> confirm -> rename
	def, ok = GetWorkflow("RenameInstanceWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 4)

	// ResetPasswordWorkflow: query -> query support zones -> confirm -> reset -> verify
	def, ok = GetWorkflow("ResetPasswordWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 5)

	// SetStopSchedulerWorkflow: query -> resolve one timestamp -> confirm -> set
	def, ok = GetWorkflow("SetStopSchedulerWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 4)

	// CancelStopSchedulerWorkflow: 3 steps
	def, ok = GetWorkflow("CancelStopSchedulerWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 3)

	// CreateCustomImageWorkflow: 4 steps
	def, ok = GetWorkflow("CreateCustomImageWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 4)

	// ResizeDiskWorkflow: query instance -> query zones -> check -> price -> confirm -> resize
	def, ok = GetWorkflow("ResizeDiskWorkflow")
	assert.True(t, ok)
	assert.NotNil(t, def)
	assert.Len(t, def.Steps, 6)

	// Unknown workflow returns nil, false
	def, ok = GetWorkflow("UnknownWorkflow")
	assert.False(t, ok)
	assert.Nil(t, def)
}
