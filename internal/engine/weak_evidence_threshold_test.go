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
		name           string
		hybridMode     string
		top1Score      float64
		rerankerScored bool
		wantWeak       bool
	}{
		// BM25 scale (0..100). 55.0 is the pre-existing threshold preserved
		// for backward compat with engine_test.go:3585,3626 fixtures.
		// rerankerScored is irrelevant for every mode except qwen3_rrf.
		{"bm25_only above threshold", "bm25_only", 80.0, false, false},
		{"bm25_only at boundary 55", "bm25_only", 55.0, false, false},
		{"bm25_only just below 55", "bm25_only", 54.9, false, true},
		{"bm25_only well below", "bm25_only", 30.0, false, true},

		// bm25_fallback shares BM25 scale (hybrid path degraded to BM25 mid-flight).
		{"bm25_fallback above threshold", "bm25_fallback", 80.0, false, false},
		{"bm25_fallback below threshold", "bm25_fallback", 54.9, false, true},

		// hybrid_cosine: cosine similarity, theoretically [-1,1], in practice
		// 0..1 per trace evidence. 0.5 is the conservative weak floor.
		{"hybrid_cosine strong 0.93", "hybrid_cosine", 0.93, false, false},
		{"hybrid_cosine at boundary 0.5", "hybrid_cosine", 0.5, false, false},
		{"hybrid_cosine just below 0.5", "hybrid_cosine", 0.49, false, true},
		{"hybrid_cosine very low", "hybrid_cosine", 0.05, false, true},

		// hybrid_rerank: cross-encoder relevance_score, 0..1 family (not a
		// calibrated probability). Same threshold as cosine for now.
		{"hybrid_rerank strong 0.93", "hybrid_rerank", 0.93, false, false},
		{"hybrid_rerank just below 0.5", "hybrid_rerank", 0.49, false, true},

		// qwen3_full: qwen3-reranker-8b cross-encoder, 0..1 by convention.
		{"qwen3_full strong 0.93", "qwen3_full", 0.93, false, false},
		{"qwen3_full at boundary 0.5", "qwen3_full", 0.5, false, false},
		{"qwen3_full just below 0.5", "qwen3_full", 0.49, false, true},

		// qwen3_rrf WHEN THE RERANKER SCORED: final Score is the qwen3-reranker-8b
		// [0,1] relevance score (the reranker overwrites the fused score), so the
		// 0.5 semantic floor applies exactly like qwen3_full.
		{"qwen3_rrf reranked strong 0.93", "qwen3_rrf", 0.93, true, false},
		{"qwen3_rrf reranked at boundary 0.5", "qwen3_rrf", 0.5, true, false},
		{"qwen3_rrf reranked just below 0.5", "qwen3_rrf", 0.49, true, true},

		// qwen3_rrf WHEN THE RERANKER DID NOT SCORE (fallback / not configured):
		// the label stays qwen3_rrf but Score reverts to the RRF-fusion scale
		// (~0.03). The 0.5 reranker floor must NOT apply — otherwise every query is
		// weak, the ledger empties, and the agent fabricates from prior (the
		// floor_reranker probe's demonstrated failure). Never weak here, even at a
		// tiny fusion score.
		{"qwen3_rrf fallback fusion 0.03 not weak", "qwen3_rrf", 0.031, false, false},
		{"qwen3_rrf fallback even 0.001 not weak", "qwen3_rrf", 0.001, false, false},
		{"qwen3_rrf fallback high fusion not weak", "qwen3_rrf", 0.2, false, false},

		// Empty HybridMode still defaults to BM25 — protects existing
		// engine_test.go mocks that don't set HybridMode explicitly. This is the
		// fixture artifact, and knowledge.ScoreScaleFor keeps it classified.
		{"empty mode defaults to BM25 80", "", 80.0, false, false},
		{"empty mode defaults to BM25 54.9", "", 54.9, false, true},

		// An UNRECOGNIZED mode is not judged as BM25: the BM25 floor is 55.0 while
		// a semantic scale tops out at 1.0, so guessing BM25 for a mode whose scale
		// nobody classified rejects every hit on a [0,1] scale and empties the
		// ledger. There is no safe guess; the only safe move is not to guess.
		//
		// Nothing in-tree produces an unrecognized mode — the MCP adapter
		// normalizes anything it cannot classify to unknown_remote before the
		// engine sees it (normalizeRemoteScoreScale) — so this is a defence for a
		// caller that skips the adapter: it degrades to "decline to judge" instead
		// of to a scale it never verified.
		{"unrecognized mode is not judged, high score", "unknown_future_mode", 80.0, false, false},
		{"unrecognized mode is not judged, low score", "unknown_future_mode", 30.0, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := []knowledge.RetrievalHit{{Score: tc.top1Score, Kept: true}}
			got := isWeakEvidence(items, tc.hybridMode, tc.rerankerScored)
			if got != tc.wantWeak {
				t.Fatalf("isWeakEvidence(top1=%v, mode=%q, reranked=%v) = %v; want %v",
					tc.top1Score, tc.hybridMode, tc.rerankerScored, got, tc.wantWeak)
			}
		})
	}
}

// TestIsWeakEvidenceEmptyItems verifies the no-hits short-circuit. Pre-fix
// behavior is preserved: an empty hit list is NEVER weak (an empty ledger is
// handled upstream — the Agent answers directly and the final gate ships it
// fail-open, never a canned no-evidence refusal).
func TestIsWeakEvidenceEmptyItems(t *testing.T) {
	if isWeakEvidence(nil, "qwen3_full", true) {
		t.Fatal("nil items must not be weak (caller short-circuits on no_evidence)")
	}
	if isWeakEvidence([]knowledge.RetrievalHit{}, "bm25_only", false) {
		t.Fatal("empty items slice must not be weak")
	}
}
