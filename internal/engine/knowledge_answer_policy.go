package engine

import "strings"

const (
	ragNoEvidenceReply   = "当前知识库未覆盖该问题,我无法回答。"
	ragUngroundableReply = "我查到了相关资料，但没能据此整理出完全有依据的回答。请把问题描述得更具体一些，我再试一次。"

	weakEvidenceBM25Threshold     = 55.0
	weakEvidenceSemanticThreshold = 0.5

	rankingAmbiguousBM25Spread     = 5.0
	rankingAmbiguousSemanticSpread = 0.05
)

func isKnowledgeRefusal(answer string) bool {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return false
	}
	for _, phrase := range []string{
		ragNoEvidenceReply,
		"知识库未覆盖",
		"当前知识库只收录",
		"没有找到可靠资料",
		"知识库暂未收录",
		"无法根据知识库回答",
		"没能据此整理出完全有依据的回答",
	} {
		if strings.Contains(trimmed, phrase) {
			return true
		}
	}
	return false
}
