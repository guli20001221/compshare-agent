package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/refusal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanAgentTransferRequest(t *testing.T) {
	for _, input := range []string{"转人工", "我要转人工", "人工客服怎么联系", "帮我联系人工", "找人工", "叫人工", "帮我转接人工"} {
		assert.True(t, isHumanAgentTransferRequest(input), input)
	}
	for _, input := range []string{"人工智能是什么", "人工费怎么算", "人工成本", "4090 多少钱一小时"} {
		assert.False(t, isHumanAgentTransferRequest(input), input)
	}
}

func TestChatExplicitHumanHandoffBypassesModel(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) { hardBlocks = append(hardBlocks, trace) })

	reply, err := eng.Chat(context.Background(), "帮我转接人工", noopStep)
	require.NoError(t, err)
	assert.Equal(t, refusal.HumanAgentTransfer, reply)
	assert.Empty(t, mock.calls)
	require.Len(t, hardBlocks, 1)
	assert.Equal(t, refusal.CategoryHumanAgent, hardBlocks[0].Category)
}

func TestHumanAgentTransferReplyContainsSupportQR(t *testing.T) {
	assert.Contains(t, refusal.HumanAgentTransfer, "ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png")
	assert.True(t, strings.HasPrefix(refusal.HumanAgentTransfer, "好的，已为您转接人工客服"))
}
