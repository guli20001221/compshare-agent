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

func TestCancelSchedulerReplyOnlyClaimsCancellationAfterReadback(t *testing.T) {
	verified, ok := scheduledShutdownWorkflowReply("CancelStopSchedulerWorkflow",
		map[string]any{"UHostId": "uhost-1"}, &workflow.Result{Data: map[string]any{"Verified": true}})
	assert.True(t, ok)
	assert.Contains(t, verified, "已取消")
	assert.Contains(t, verified, "回读确认")

	unverified, ok := scheduledShutdownWorkflowReply("CancelStopSchedulerWorkflow",
		map[string]any{"UHostId": "uhost-1"}, &workflow.Result{Data: map[string]any{"Verified": false}})
	assert.True(t, ok)
	assert.Contains(t, unverified, "尚未确认")
	assert.NotContains(t, unverified, "已取消")
}
