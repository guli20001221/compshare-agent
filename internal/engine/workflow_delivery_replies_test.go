package engine

import (
	"testing"
	"time"

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

func TestCancelSchedulerReplyDoesNotClaimUnverifiableDeletion(t *testing.T) {
	reply, ok := scheduledShutdownWorkflowReply("CancelStopSchedulerWorkflow",
		map[string]any{"UHostId": "uhost-1"}, &workflow.Result{Data: map[string]any{"Verified": true}})
	assert.True(t, ok)
	assert.Contains(t, reply, "无法独立确认")
	assert.NotContains(t, reply, "已取消")
}

func TestCancelSchedulerReplyReportsObservedScheduleStillPresent(t *testing.T) {
	const observed = int64(1778420000)
	reply, ok := scheduledShutdownWorkflowReply("CancelStopSchedulerWorkflow",
		map[string]any{"UHostId": "uhost-1"}, &workflow.Result{Data: map[string]any{
			"Verified":         false,
			"ObservedStopTime": observed,
		}})

	assert.True(t, ok)
	assert.Contains(t, reply, "实时回读仍显示")
	assert.Contains(t, reply, time.Unix(observed, 0).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04（北京时间）"))
	assert.Contains(t, reply, "尚未确认取消生效")
	assert.Contains(t, reply, "请勿重复提交")
	assert.NotContains(t, reply, "已取消")
}
