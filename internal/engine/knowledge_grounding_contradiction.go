package engine

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

type groundingNegationReplacement struct {
	negative string
	positive string
}

// Keep this deliberately small and high precision. The semantic verifier still
// judges entailment; this local check only catches obvious polarity reversals
// that must remain rejected even if the verifier mistakenly says supported.
var groundingNegationReplacements = []groundingNegationReplacement{
	{"不允许", "允许"}, {"不支持", "支持"}, {"不提供", "提供"},
	{"不包含", "包含"}, {"不预装", "预装"}, {"不收费", "收费"},
	{"不计费", "计费"}, {"不需要", "需要"}, {"不会", "会"},
	{"不能", "能"}, {"无需", "需要"}, {"无法", "可以"},
	{"未提供", "提供"}, {"未包含", "包含"}, {"未预装", "预装"},
	{"未开启", "开启"}, {"未收费", "收费"}, {"未计费", "计费"},
	{"does not support", "supports"}, {"do not support", "support"},
	{"is not allowed", "is allowed"}, {"cannot", "can"}, {"can't", "can"},
}

var groundingPolarityNoiseRE = regexp.MustCompile(`[\p{P}\p{S}\s]+`)

func obviousKnowledgeGroundingContradiction(answerQuote, evidenceQuote string) bool {
	answerClauses := knowledgeClauseBoundaryRE.Split(answerQuote, -1)
	evidenceClauses := knowledgeClauseBoundaryRE.Split(evidenceQuote, -1)
	for _, answerClause := range answerClauses {
		answerCanonical, answerNegative := canonicalGroundingPolarity(answerClause)
		if utf8.RuneCountInString(answerCanonical) < 4 {
			continue
		}
		for _, evidenceClause := range evidenceClauses {
			evidenceCanonical, evidenceNegative := canonicalGroundingPolarity(evidenceClause)
			if answerNegative == evidenceNegative || utf8.RuneCountInString(evidenceCanonical) < 4 {
				continue
			}
			// A near-identical clause with opposite polarity is an obvious
			// contradiction. Requiring containment keeps unrelated positive and
			// negative claims in a multi-clause evidence quote from colliding.
			if strings.Contains(answerCanonical, evidenceCanonical) || strings.Contains(evidenceCanonical, answerCanonical) {
				return true
			}
		}
	}
	return false
}

func canonicalGroundingPolarity(text string) (string, bool) {
	canonical := strings.ToLower(strings.TrimSpace(text))
	negative := false
	for _, replacement := range groundingNegationReplacements {
		if strings.Contains(canonical, replacement.negative) {
			negative = true
			canonical = strings.ReplaceAll(canonical, replacement.negative, replacement.positive)
		}
	}
	return groundingPolarityNoiseRE.ReplaceAllString(canonical, ""), negative
}
