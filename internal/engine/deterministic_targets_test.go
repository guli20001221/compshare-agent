package engine

import (
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
)

func TestInferLifecycleAction_DoesNotStealScheduledShutdown(t *testing.T) {
	for _, text := range []string{
		"给实例 uhost-xxx 设置 30 分钟后自动关机",
		"uhost-xxx 1小时后关机",
		"帮我给 uhost-xxx 设置定时关机",
		"取消 uhost-xxx 的定时关机",
	} {
		assert.Empty(t, inferLifecycleAction(text), text)
	}

	assert.Equal(t, intent.LifecycleActionStop, inferLifecycleAction("关机实例 uhost-xxx"))
}

func TestInferLifecycleAction_CreateDisk(t *testing.T) {
	for _, text := range []string{
		"给 uhost-xxx 加 200G 数据盘",
		"给实例 host 创建一块 20G 数据盘",
		"新建 50GB datadisk 到 uhost-xxx",
	} {
		assert.Equal(t, intent.LifecycleActionCreateDisk, inferLifecycleAction(text), text)
	}
}

func TestCreateDiskSizeFromUserText(t *testing.T) {
	assert.Equal(t, float64(20), createDiskSizeFromUserText("给实例 host 创建一块 20G 数据盘"))
	assert.Equal(t, float64(50), createDiskSizeFromUserText("新建 50GB datadisk 到 uhost-xxx"))
	assert.Zero(t, createDiskSizeFromUserText("给实例 host 创建一块数据盘"))
}

func TestCFSWriteWorkflowFromUserText(t *testing.T) {
	workflow, ok := cfsWriteWorkflowFromUserText("在华北一C创建一个100G的CFS共享文件存储，名字叫codex-cfs-test")
	assert.True(t, ok)
	assert.Equal(t, "CreateCFSWorkflow", workflow)

	workflow, ok = cfsWriteWorkflowFromUserText("cfs-test 扩容到 200GB")
	assert.True(t, ok)
	assert.Equal(t, "ResizeCFSWorkflow", workflow)

	_, ok = cfsWriteWorkflowFromUserText("在华北一C创建100G CFS多少钱")
	assert.False(t, ok)
}

func TestCFSWorkflowArgsFromUserText(t *testing.T) {
	args, reply, ok := cfsWorkflowArgsFromUserText("CreateCFSWorkflow", "帮我在华北一C创建一个100G的CFS共享文件存储，名字叫codex-cfs-test")
	assert.True(t, ok)
	assert.Empty(t, reply)
	assert.Equal(t, "codex-cfs-test", args["Name"])
	assert.Equal(t, float64(100), args["Size"])
	assert.Equal(t, "华北一C", args["Zone"])

	args, reply, ok = cfsWorkflowArgsFromUserText("ResizeCFSWorkflow", "把 cfs-abc123 扩容到 200GB")
	assert.True(t, ok)
	assert.Empty(t, reply)
	assert.Equal(t, "cfs-abc123", args["CfsId"])
	assert.Equal(t, float64(200), args["Size"])
}
