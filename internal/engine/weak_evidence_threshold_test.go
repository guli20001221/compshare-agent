package engine

import (
	"testing"

	"github.com/compshare-agent/internal/knowledge"
)

// TestIsWeakEvidenceByHybridMode locks the mode-aware weak-evidence threshold
// against the pre-fix bug where a single 55.0 floor was applied uniformly to
// BM25 (0..100) and semantic (0..1) scores alike. Production trace data
// (cli-smoke-stage5-prompt-full-20260519: 94/95 = 99% of qwen3_full queries
// were flagged weak with top score median 0.947) showed the uniform floor
// systematically forced the weak-mode RAG prompt and inflated Tier-3 refusals.
//
// Each case binds a (hybridMode, top1 score) pair to the expected weak verdict
// at the chosen threshold boundary. Adjusting weakEvidenceBM25Threshold or
// weakEvidenceSemanticThreshold without updating both this test AND the prompt
// boundary it gates is meant to fail loudly.
func TestIsWeakEvidenceByHybridMode(t *testing.T) {
	cases := []struct {
		name       string
		hybridMode string
		top1Score  float64
		wantWeak   bool
	}{
		// BM25 scale (0..100). 55.0 is the pre-existing threshold preserved
		// for backward compat with engine_test.go:3585,3626 fixtures.
		{"bm25_only above threshold", "bm25_only", 80.0, false},
		{"bm25_only at boundary 55", "bm25_only", 55.0, false},
		{"bm25_only just below 55", "bm25_only", 54.9, true},
		{"bm25_only well below", "bm25_only", 30.0, true},

		// bm25_fallback shares BM25 scale (hybrid path degraded to BM25 mid-flight).
		{"bm25_fallback above threshold", "bm25_fallback", 80.0, false},
		{"bm25_fallback below threshold", "bm25_fallback", 54.9, true},

		// hybrid_cosine: cosine similarity, theoretically [-1,1], in practice
		// 0..1 per trace evidence. 0.5 is the conservative weak floor.
		{"hybrid_cosine strong 0.93", "hybrid_cosine", 0.93, false},
		{"hybrid_cosine at boundary 0.5", "hybrid_cosine", 0.5, false},
		{"hybrid_cosine just below 0.5", "hybrid_cosine", 0.49, true},
		{"hybrid_cosine very low", "hybrid_cosine", 0.05, true},

		// hybrid_rerank: cross-encoder relevance_score, 0..1 family (not a
		// calibrated probability). Same threshold as cosine for now.
		{"hybrid_rerank strong 0.93", "hybrid_rerank", 0.93, false},
		{"hybrid_rerank just below 0.5", "hybrid_rerank", 0.49, true},

		// qwen3_full: qwen3-reranker-8b cross-encoder, 0..1 by convention.
		{"qwen3_full strong 0.93", "qwen3_full", 0.93, false},
		{"qwen3_full at boundary 0.5", "qwen3_full", 0.5, false},
		{"qwen3_full just below 0.5", "qwen3_full", 0.49, true},

		// qwen3_rrf: same qwen3-reranker-8b cross-encoder produces final
		// Score (the RRF fusion happens BEFORE the reranker, which then
		// overwrites Score with relevance_score). Same 0..1 threshold as
		// qwen3_full. Without this case isWeakEvidence would default to
		// the BM25 threshold (54.9) and false-refuse on a 0..1 cross-
		// encoder score — the e03 regression caught by CLI judge.
		{"qwen3_rrf strong 0.93", "qwen3_rrf", 0.93, false},
		{"qwen3_rrf at boundary 0.5", "qwen3_rrf", 0.5, false},
		{"qwen3_rrf just below 0.5", "qwen3_rrf", 0.49, true},

		// Empty / unknown HybridMode defaults to BM25 — protects existing
		// engine_test.go mocks that don't set HybridMode explicitly.
		{"empty mode defaults to BM25 80", "", 80.0, false},
		{"empty mode defaults to BM25 54.9", "", 54.9, true},
		{"unknown mode defaults to BM25 80", "unknown_future_mode", 80.0, false},
		{"unknown mode defaults to BM25 30", "unknown_future_mode", 30.0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := []knowledge.RetrievalHit{{Score: tc.top1Score, Kept: true}}
			got := isWeakEvidence(items, tc.hybridMode)
			if got != tc.wantWeak {
				t.Fatalf("isWeakEvidence(top1=%v, mode=%q) = %v; want %v",
					tc.top1Score, tc.hybridMode, got, tc.wantWeak)
			}
		})
	}
}

// TestIsWeakEvidenceEmptyItems verifies the no-hits short-circuit. Pre-fix
// behavior is preserved: an empty hit list is NEVER weak (the no_evidence
// branch upstream handles that case via ragNoEvidenceReply).
func TestIsWeakEvidenceEmptyItems(t *testing.T) {
	if isWeakEvidence(nil, "qwen3_full") {
		t.Fatal("nil items must not be weak (caller short-circuits on no_evidence)")
	}
	if isWeakEvidence([]knowledge.RetrievalHit{}, "bm25_only") {
		t.Fatal("empty items slice must not be weak")
	}
}

// TestIsRankingAmbiguousByHybridMode locks the mode-aware spread threshold.
// Ranking-ambiguous gates trace.RankingErrorCandidate only; it does NOT change
// the prompt or refusal behavior. Mode-aware so spread units match score units.
func TestIsRankingAmbiguousByHybridMode(t *testing.T) {
	cases := []struct {
		name          string
		hybridMode    string
		top1, top2    float64
		wantAmbiguous bool
	}{
		// BM25 scale: 5.0 spread is the pre-existing threshold.
		{"bm25 wide spread 80 vs 60", "bm25_only", 80.0, 60.0, false},
		{"bm25 just above 5 spread", "bm25_only", 80.0, 74.9, false},
		{"bm25 just below 5 spread", "bm25_only", 80.0, 75.1, true},
		{"bm25 tied", "bm25_only", 80.0, 80.0, true},

		// Semantic scale: 0.05 spread (1/100th of the 5.0 BM25 spread).
		{"qwen3_full wide spread 0.93 vs 0.50", "qwen3_full", 0.93, 0.50, false},
		{"qwen3_full just above 0.05 spread", "qwen3_full", 0.93, 0.87, false},
		{"qwen3_full just below 0.05 spread", "qwen3_full", 0.93, 0.89, true},
		{"hybrid_cosine tied", "hybrid_cosine", 0.93, 0.93, true},

		// qwen3_rrf uses the same cross-encoder Score scale as qwen3_full
		// (reranker overwrites the fused score), so same spread threshold.
		{"qwen3_rrf wide spread 0.93 vs 0.50", "qwen3_rrf", 0.93, 0.50, false},
		{"qwen3_rrf just below 0.05 spread", "qwen3_rrf", 0.93, 0.89, true},

		// Empty defaults to BM25 spread (consistent with weak threshold default).
		{"empty mode defaults to BM25 spread", "", 80.0, 70.0, false},
		{"empty mode tied under BM25", "", 80.0, 78.0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := []knowledge.RetrievalHit{
				{Score: tc.top1, Kept: true},
				{Score: tc.top2, Kept: true},
			}
			got := isRankingAmbiguous(items, tc.hybridMode)
			if got != tc.wantAmbiguous {
				t.Fatalf("isRankingAmbiguous(top1=%v top2=%v mode=%q) = %v; want %v",
					tc.top1, tc.top2, tc.hybridMode, got, tc.wantAmbiguous)
			}
		})
	}
}

// TestIsRankingAmbiguousSingleItem locks the pre-fix "fewer than 2 items
// means unambiguous by definition" semantic. Important so a single-hit
// retrieval result doesn't accidentally flip the telemetry flag.
func TestIsRankingAmbiguousSingleItem(t *testing.T) {
	items := []knowledge.RetrievalHit{{Score: 0.93, Kept: true}}
	if isRankingAmbiguous(items, "qwen3_full") {
		t.Fatal("single-item list must not be ambiguous")
	}
	if isRankingAmbiguous(nil, "bm25_only") {
		t.Fatal("nil items must not be ambiguous")
	}
}
