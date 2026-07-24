//go:build live

// Pins whether the coding-plan tier chunk is a retrieval miss or a synthesis
// denial for case real-c5c136fe. The corpus provably contains the tiers
// (v2-resource_purchase-ac94d9679403ee37 lists Mini/Lite/Basic/Pro/Max/Ultra),
// yet the pro agent answered that the platform "does not use these tier names".
// Those are two different bugs with two different fixes: if production qwen3_rrf
// never surfaces the chunk, it is a retrieval hole; if it surfaces it and the
// agent denied it anyway, it is a grounding/synthesis hole.
//
//	go test ./internal/engine -tags live -run TestLiveCodingPlanRetrievalRank -v -timeout 10m
package engine

import (
	"testing"
)

func TestLiveCodingPlanRetrievalRank(t *testing.T) {
	cfg := loadLiveConfig(t)
	corpus, sidecar := mergedProductionIndex(t)
	retriever := productionAnswerRetriever(t, cfg, corpus, sidecar)

	const tierChunk = "v2-resource_purchase-ac94d9679403ee37"
	queries := []string{
		"coding plan mini lite basic pro max ultra 套餐 速度 区别",
		"Coding Plan 套餐 区别",
		"套餐 档位 Mini Lite Basic Pro Max Ultra",
		"套餐包规格 扣除模式",
	}
	for _, q := range queries {
		hits := retriever.Retrieve(q, "").HitItems
		rank := 0
		for i, h := range hits {
			if h.Chunk.ChunkID == tierChunk {
				rank = i + 1
				break
			}
		}
		top := make([]string, 0, len(hits))
		for i, h := range hits {
			if i >= 8 {
				break
			}
			top = append(top, h.Chunk.ChunkID)
		}
		if rank > 0 {
			t.Logf("query=%q\n  tier chunk rank = %d / %d  ✓ 检索到了", q, rank, len(hits))
		} else {
			t.Logf("query=%q\n  tier chunk NOT in top %d (共 %d 条) ✗ 检索没捞到\n  top: %v", q, len(hits), len(hits), top)
		}
	}
}
