package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/workflow"
)

// TestDeployAlternativesNote: on a stock shortage, the deploy reply must offer the
// image-compatible, VRAM-sufficient cards that are still offered — concrete options
// instead of a bare "换一个规格" — and steer the user into the pinned-GPU re-deploy.
func TestDeployAlternativesNote(t *testing.T) {
	plan := deployPlan{ModelName: "Qwen2.5-7B", Quantization: "fp16", GpuType: "4090"}
	cards := []knowledge.AvailableGPU{
		{Name: "4090", VRAMGB: 24},
		{Name: "5090", VRAMGB: 32},
		{Name: "A800", VRAMGB: 80},
		{Name: "2080", VRAMGB: 8},
	}
	note := deployAlternativesNote(plan, cards)
	require.Contains(t, note, "5090(32GB)")
	require.Contains(t, note, "A800(80GB)")
	require.NotContains(t, note, "4090") // the sold-out recommended card is excluded
	require.NotContains(t, note, "2080") // too small for a 7B
	require.Contains(t, note, "回复「用 5090」")
	require.Contains(t, note, "以创建结果为准") // honest: type-level availability is advisory

	// When the only offered card is the sold-out one, there is nothing to suggest →
	// empty, and the caller keeps the bare stop reply.
	require.Empty(t, deployAlternativesNote(
		deployPlan{ModelName: "Qwen2.5-7B", Quantization: "fp16", GpuType: "5090"},
		[]knowledge.AvailableGPU{{Name: "5090", VRAMGB: 32}}))
}

// TestIsDeployStockShortage pins the signal the alternatives note keys off — the
// capacity gate's "库存不足" message — and that cancel / nil never trip it.
func TestIsDeployStockShortage(t *testing.T) {
	require.True(t, isDeployStockShortage(&workflow.Result{
		Message: "4090 1 卡 / 16C / 64GB 当前库存不足（售罄），请换一个规格或稍后再试。"}))
	require.False(t, isDeployStockShortage(&workflow.Result{Message: "用户取消了操作"}))
	require.False(t, isDeployStockShortage(nil))
}
