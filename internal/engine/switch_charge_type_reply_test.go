package engine

import (
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSwitchChargeTypeReplyReflectsReadbackInsteadOfAssumingSuccess(t *testing.T) {
	tests := []struct {
		name     string
		data     map[string]any
		contains []string
	}{
		{
			name: "verified",
			data: map[string]any{
				"ReadbackAvailable": true, "Verified": true, "ObservedChargeType": "Month",
			},
			contains: []string{"✅", "切换为包月", "实时回读确认"},
		},
		{
			name: "stale readback",
			data: map[string]any{
				"ReadbackAvailable": true, "Verified": false, "ObservedChargeType": "Postpay",
			},
			contains: []string{"已提交", "仍显示为按量付费", "尚未确认", "请勿重复提交"},
		},
		{
			name: "readback unavailable",
			data: map[string]any{
				"ReadbackAvailable": false, "Verified": false,
			},
			contains: []string{"已提交", "未能回读", "请勿重复提交"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, ok := switchChargeTypeWorkflowReply("SwitchChargeTypeWorkflow", map[string]any{
				"UHostId": "uhost-1", "DestChargeType": "Month",
			}, &workflow.Result{Data: tt.data})
			require.True(t, ok)
			for _, want := range tt.contains {
				assert.Contains(t, reply, want)
			}
		})
	}
}
