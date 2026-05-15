package engine

import (
	"regexp"
	"strings"
)

const (
	ragNoEvidenceReply       = "当前知识库未覆盖该问题,我无法回答。"
	ragWeakEvidenceThreshold = 55.0
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
