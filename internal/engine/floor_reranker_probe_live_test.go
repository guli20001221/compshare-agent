//go:build live

// Settles whether #9/#6's "retrieved-but-hallucinated" is a real synthesis
// failure or a probe artifact. The pro-probe wired qwen3_rrf WITHOUT the reranker
// (the adapter lived in package cmd), so its final Score is the RRF fusion score
// (~0.01–0.03), NOT the qwen3-reranker-8b [0,1] relevance score. But the engine's
// isWeakEvidence floor for qwen3_rrf is 0.5 — calibrated for the RERANKER scale.
// So without the reranker, isWeakEvidence(top1 < 0.5) is true for essentially
// every query → the agent's ledger is emptied → it free-writes from prior. That
// would make #9's "语料命中 rank 1-2 却否认" a floor artifact, not the model
// ignoring evidence.
//
// This probe retrieves the two target chunks under BOTH configs and prints
// top-1 score, the target chunk's rank+score, and the isWeakEvidence verdict, so
// the artifact-vs-real question is answered by measurement, not inference.
//
//	go test ./internal/engine -tags live -run TestLiveFloorRerankerArtifact -v -timeout 15m
package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/embedding"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/reranker"
)

// testRerankerAdapter mirrors cmd/trace.go's rerankerClientAdapter so the test
// can wire a production-faithful reranker without importing package cmd.
type testRerankerAdapter struct{ c reranker.Client }

func (a testRerankerAdapter) Rerank(ctx context.Context, query string, docs []string, topN int) ([]knowledge.RerankerResult, error) {
	res, err := a.c.Rerank(ctx, query, docs, topN)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.RerankerResult, 0, len(res))
	for _, r := range res {
		out = append(out, knowledge.RerankerResult{Index: r.Index, Score: r.Score})
	}
	return out, nil
}

func rerankedProductionRetriever(t *testing.T, cfg *config.Config, corpus knowledge.Corpus, sidecar knowledge.EmbeddingSidecar) *knowledge.Retriever {
	t.Helper()
	embedModel := "qwen3-embedding-8b"
	embedClient, err := embedding.NewClient(embedding.ClientOptions{
		BaseURL: cfg.Agent.LLM.BaseURL, APIKey: cfg.Agent.LLM.APIKey, Model: embedModel,
	})
	if err != nil {
		t.Fatalf("embedding client: %v", err)
	}
	rClient, err := reranker.NewModelverseClient(reranker.ClientOptions{
		BaseURL: cfg.Agent.LLM.BaseURL, APIKey: cfg.Agent.LLM.APIKey, Model: "qwen3-reranker-8b",
	})
	if err != nil {
		t.Fatalf("reranker client: %v", err)
	}
	return knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{
		TopK:             8,
		Mode:             knowledge.RetrievalModeQwen3RRF,
		EmbeddingSidecar: &sidecar,
		Embedder:         embedClient,
		EmbeddingModel:   embedModel,
		Reranker:         testRerankerAdapter{c: rClient},
		RerankerModel:    "qwen3-reranker-8b",
		Now:              realCorpusRecallNow,
	})
}

func TestLiveFloorRerankerArtifact(t *testing.T) {
	cfg := loadLiveConfig(t)
	corpus, sidecar := mergedProductionIndex(t)
	noRerank := productionAnswerRetriever(t, cfg, corpus, sidecar) // what every prior engine-path probe used
	withRerank := rerankedProductionRetriever(t, cfg, corpus, sidecar)

	type target struct {
		label   string
		chunkID string
		queries []string
	}
	targets := []target{
		{
			label:   "#9 套餐档位 tier chunk",
			chunkID: "v2-resource_purchase-ac94d9679403ee37",
			queries: []string{
				"coding plan mini lite basic pro max ultra 套餐 速度 区别",
				"套餐 档位 Mini Lite Basic Pro Max Ultra 区别",
			},
		},
		{
			label:   "#6 计费概览 (独占=按量, 关机不收费)",
			chunkID: "v2-resource_purchase-581fc3349d6dff35",
			queries: []string{
				"独占式实例 关机 资源保留 计费",
				"实例 关机 是否 继续 收费 计费方式",
			},
		},
	}

	report := func(tag string, r *knowledge.Retriever, tgt target, q string) {
		res := r.Retrieve(q, "")
		hits := res.HitItems
		floor := weakEvidenceThresholdFor(res.HybridMode)
		dropped := isWeakEvidence(hits, res.HybridMode, res.RerankerMode != "")
		top1 := -1.0
		if len(hits) > 0 {
			top1 = hits[0].Score
		}
		rank, score := 0, -1.0
		for i, h := range hits {
			if h.Chunk.ChunkID == tgt.chunkID {
				rank, score = i+1, h.Score
				break
			}
		}
		present := "缺席"
		if rank > 0 {
			present = fmt.Sprintf("rank %d, score %.4f", rank, score)
		}
		t.Logf("  [%-11s mode=%-9s] top1=%.4f floor=%.2f isWeak=%-5v 目标=%s | q=%q",
			tag, res.HybridMode, top1, floor, dropped, present, q)
		if dropped {
			t.Logf("      ⇒ isWeakEvidence=true → 引擎会把整批丢成空 ledger,agent 看不到目标 chunk")
		} else if rank > 0 {
			t.Logf("      ⇒ 目标 chunk 保留且过floor → agent 确实看得到它")
		}
	}

	for _, tgt := range targets {
		t.Logf("== %s (%s) ==", tgt.label, tgt.chunkID)
		for _, q := range tgt.queries {
			report("no-reranker", noRerank, tgt, q)
			report("+reranker", withRerank, tgt, q)
		}
	}
}
