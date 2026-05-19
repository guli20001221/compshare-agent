package engine

import (
	"regexp"
	"strings"
)

const (
	ragNoEvidenceReply = "当前知识库未覆盖该问题,我无法回答。"

	// Weak-evidence and ranking-ambiguous thresholds are score-scale-aware.
	// internal/knowledge/retriever.go produces RetrievalHit.Score values whose
	// scale depends on the retrieval path actually used (RetrievalResult.HybridMode):
	//   - "bm25_only" / "bm25_fallback" / "" : BM25 score, typically 0..100+.
	//     Threshold 55.0 is preserved from the pre-mode-aware era (the value
	//     existing engine_test.go assertions are pinned against, e.g. 54.9).
	//   - "hybrid_cosine"                    : cosine similarity, theoretically
	//     [-1,1] but trace data (2026-05-19 qwen3_full × 115) shows scores
	//     fall in 0..1 in practice. 0.5 is a conservative weak floor for
	//     this 0..1 range; calibrate by smoke distribution if needed.
	//   - "hybrid_rerank" / "qwen3_full"     : cross-encoder relevance_score,
	//     also in the 0..1 family by convention (not a calibrated probability).
	//     Same 0.5 floor for now — kept identical to cosine for simplicity;
	//     split if smoke distributions diverge.
	// The pre-fix global 55.0 applied uniformly meant that hybrid/qwen3 score
	// 0.95 was treated as weak (0.95 < 55.0), which systematically forced the
	// weak-mode RAG prompt and inflated Tier-3 refusals. See trace evidence:
	// F:/compshare-agent-runs/cli-smoke-stage5-prompt-full-20260519, where
	// 94/95 queries (99%) were misflagged weak.
	weakEvidenceBM25Threshold     = 55.0
	weakEvidenceSemanticThreshold = 0.5

	// Ranking-ambiguous thresholds gate trace.RankingErrorCandidate only;
	// they are NOT passed to the RAG prompt and therefore don't directly
	// change refusal behavior. Kept mode-aware for telemetry accuracy:
	// a 5.0 spread is sensible on BM25 0..100 but impossible on cosine 0..1.
	rankingAmbiguousBM25Spread     = 5.0
	rankingAmbiguousSemanticSpread = 0.05
)

// Citations are 1-indexed in the RAG prompt; [0] is treated as missing and
// triggers the retry/no-evidence guard.
var numberedCitationRE = regexp.MustCompile(`\[[1-9][0-9]*\]`)

func hasNumberedCitation(answer string) bool {
	return numberedCitationRE.MatchString(answer)
}

func isKnowledgeRefusal(answer string) bool {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return false
	}
	refusalPhrases := []string{
		ragNoEvidenceReply,
		"知识库未覆盖",
		"当前知识库只收录",
		"没有找到可靠资料",
		"知识库暂未收录",
		"无法根据知识库回答",
	}
	for _, phrase := range refusalPhrases {
		if strings.Contains(trimmed, phrase) {
			return true
		}
	}
	return false
}
