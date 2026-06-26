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
