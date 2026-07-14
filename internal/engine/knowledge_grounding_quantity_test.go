package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGroundingQuantitiesConsistent(t *testing.T) {
	tests := []struct {
		name     string
		answer   string
		evidence string
		want     bool
	}{
		// Sanitized from the real Coding Plan retrieval record in
		// eval/trace_gate/billing_jitter_ext1on.jsonl.
		{"real_record_same_duration", "额度采用固定 5 小时窗口刷新", "Coding Plan 采用固定 5小时窗口刷新额度", true},
		{"real_record_reversed_duration", "额度采用固定 30 小时窗口刷新", "Coding Plan 采用固定 5 小时窗口刷新额度", false},
		{"decimal_currency_formatting", "价格是 0.50 元", "价格为 0.5元", true},
		{"different_currency_amount", "价格是 5 元", "价格为 0.5元", false},
		{"percentage_reversal", "最多使用 100%", "最多使用 10%", false},
		{"cuda_version_reversal", "需要 CUDA 11.8", "需要 CUDA 12.4", false},
		{"capacity_reversal", "显存为 48 GB", "显存为 24GB", false},
		{"no_quantity_is_semantic_verifier_job", "平台支持退款", "该订单支持退款", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, groundingQuantitiesConsistent(tt.answer, tt.evidence))
		})
	}
}
