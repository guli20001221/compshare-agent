package engine

import (
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
)

func TestCloneProgressDoesNotClaimTheImageIsUsable(t *testing.T) {
	reply := cloneCustomImageWorkflowReply(&workflow.Result{Data: map[string]any{
		"CompShareImageId": "cimg-target",
		"DeliveryState":    "pending",
		"Progress":         map[string]any{"Progress": "100"},
	}})

	assert.Contains(t, reply, "进度为 100%")
	assert.Contains(t, reply, "最终可用状态")
	assert.NotContains(t, reply, "已完成跨区同步")
}
