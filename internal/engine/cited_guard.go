package engine

import (
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
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

// extractCitedChunkIDs reads all [n] markers in the answer, dedupes while
// preserving first-occurrence order, and maps each n (1-indexed) to the
// corresponding hit's ChunkID. Markers that point beyond len(hits) or to
// [0] are dropped. Returns nil when no in-range citation is found.
//
// Called AFTER the cited contract gate (hasNumberedCitation) has already
// validated the answer has citations, and BEFORE stripCitationMarkers
// removes the [n] tokens — so the answer string still contains markers.
// The output is recorded in trace.CitedChunkIDs for MySQL audit ingestion;
// the user-facing reply gets the stripped version.
func extractCitedChunkIDs(answer string, hits []knowledge.RetrievalHit) []string {
	matches := numberedCitationRE.FindAllString(answer, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		// m is "[N]" — peel brackets.
		raw := strings.TrimSuffix(strings.TrimPrefix(m, "["), "]")
		n := 0
		for _, r := range raw {
			if r < '0' || r > '9' {
				n = -1
				break
			}
			n = n*10 + int(r-'0')
		}
		if n <= 0 || n > len(hits) {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, hits[n-1].Chunk.ChunkID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stripCitationMarkers removes all [n] markers from the answer text before
// it is shown to the user. The cited-chunk mapping survives in trace.
// CitedChunkIDs (extracted before this call). After removal we collapse
// the two cosmetic side-effects of stripping: " ." → "." (citation right
// before sentence end) and double spaces left by adjacent removals.
//
// Why not change the RAG prompt to skip [n] entirely: the [n] presence is
// the anti-fabrication anchor that gates the cited contract — removing it
// from the prompt would let the LLM regress to unsourced prose. We keep
// [n] in the LLM contract, strip cosmetically at the boundary.
func stripCitationMarkers(answer string) string {
	if answer == "" {
		return answer
	}
	out := numberedCitationRE.ReplaceAllString(answer, "")
	// Tidy up CJK + ASCII punctuation that immediately preceded a stripped
	// marker. Order matters: collapse spaces last so combined " [1]." -> "."
	// rather than "  ." -> " .". The CJK enumeration comma 、 (U+3001) and
	// fullwidth ! ? are common in LLM Chinese output — "方案A [1]、方案B [2]"
	// would otherwise leave orphan spaces before 、 (caught in PR #125 review).
	out = strings.ReplaceAll(out, " ，", "，")
	out = strings.ReplaceAll(out, " 。", "。")
	out = strings.ReplaceAll(out, " ；", "；")
	out = strings.ReplaceAll(out, " ：", "：")
	out = strings.ReplaceAll(out, " 、", "、")
	out = strings.ReplaceAll(out, " ！", "！")
	out = strings.ReplaceAll(out, " ？", "？")
	out = strings.ReplaceAll(out, " ,", ",")
	out = strings.ReplaceAll(out, " .", ".")
	out = strings.ReplaceAll(out, " ;", ";")
	out = strings.ReplaceAll(out, " :", ":")
	out = strings.ReplaceAll(out, " !", "!")
	out = strings.ReplaceAll(out, " ?", "?")
	// Collapse runs of spaces to a single space; do NOT touch newlines so
	// markdown structure (lists, tables) survives.
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	// Trim trailing ASCII spaces on each line — markers right before \n or
	// at end-of-string leave a hanging space that breaks markdown table
	// cell padding and looks ragged in the CLI renderer.
	if strings.Contains(out, " \n") || strings.HasSuffix(out, " ") {
		lines := strings.Split(out, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		out = strings.Join(lines, "\n")
	}
	return out
}
