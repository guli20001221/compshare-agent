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

// 不会关闭源实例 is true, and saying only that is what makes it misleading: the
// instance goes into ImageMaking, loses its public address, and refuses 开关机 /
// 重装系统 / 变更配置 with 8964 for the duration. A user who is told the machine
// stays up and then cannot SSH into it reads that as a fault.
//
// The workflow supplies the source-specific impact shown on its card.
func TestBothCustomImageSurfacesSayWhatHappensToTheSourceInstance(t *testing.T) {
	reply := customImageWorkflowReply(&workflow.Result{Data: map[string]any{
		"CompShareImageId":   "cimg-async-003",
		"SourceInstanceNote": workflow.CustomImageSourceInstanceNote,
	}})

	for _, fact := range []string{"ImageMaking", "公网地址", "开关机", "重装系统", "变更配置"} {
		assert.Contains(t, reply, fact,
			"the post-write reply drops %q, so the user learns it from the symptom instead", fact)
	}

	// The reply consumes the workflow's sentence instead of re-deriving its facts.
	assert.Contains(t, reply, workflow.CustomImageSourceInstanceNote,
		"the reply must carry the workflow's own sentence, not a second copy that can drift")

	// The earlier fix must survive: 不会关闭源实例 stays true and stays said.
	assert.NotContains(t, reply, "已关闭")
	assert.NotContains(t, reply, "将关闭")
}
