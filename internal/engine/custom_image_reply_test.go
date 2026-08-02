package engine

import (
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
)

func TestCustomImageWorkflowReplyStatesAsynchronousMakingState(t *testing.T) {
	reply := customImageWorkflowReply(&workflow.Result{Data: map[string]any{
		"CompShareImageId": "cimg-async-001",
	}})

	assert.Contains(t, reply, "已发起")
	assert.Contains(t, reply, "cimg-async-001")
	assert.Contains(t, reply, "Making")
	assert.Contains(t, reply, "Available")
	assert.NotContains(t, reply, "制作完成")
	assert.NotContains(t, reply, "已可用")
}

func TestCommittedWriteFallbackReplyKeepsCustomImageAsyncMeaning(t *testing.T) {
	reply := committedWriteFallbackReply("CreateCustomImageWorkflow", nil, &workflow.Result{Data: map[string]any{
		"CompShareImageId": "cimg-async-002",
	}})

	assert.Contains(t, reply, "cimg-async-002")
	assert.Contains(t, reply, "Making")
	assert.NotContains(t, reply, "已执行成功")
}
