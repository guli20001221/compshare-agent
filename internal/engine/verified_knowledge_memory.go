package engine

import (
	"strings"
	"time"

	"github.com/compshare-agent/internal/knowledge"
)

const (
	verifiedKnowledgeMaxTurns       = 4
	verifiedKnowledgeMaxItems       = 8
	verifiedKnowledgeAnswerMaxRunes = 1200
)

func (e *Engine) hasReusableVerifiedKnowledge() bool {
	return e != nil && len(e.sessionState.VerifiedKnowledge) > 0
}

// rememberVerifiedKnowledge stores only answers that have already passed the
// semantic verifier. The bounded evidence snippets are the durable trust root;
// arbitrary assistant prose is never promoted merely because it was emitted.
func (e *Engine) rememberVerifiedKnowledge(question, answer string, ledger knowledge.EvidenceLedger) {
	if e == nil || len(ledger.Items) == 0 || strings.TrimSpace(answer) == "" {
		return
	}
	question = strings.TrimSpace(question)
	if question == "" {
		question = strings.TrimSpace(ledger.Query)
	}
	entry := VerifiedKnowledgeTurn{
		Question:       truncateRunes(question, 600),
		Answer:         truncateRunes(strings.TrimSpace(answer), verifiedKnowledgeAnswerMaxRunes),
		Evidence:       boundedVerifiedKnowledgeLedger(ledger),
		VerifiedAtUnix: time.Now().Unix(),
	}
	if len(entry.Evidence.Items) == 0 {
		return
	}
	current := e.sessionState.VerifiedKnowledge
	out := make([]VerifiedKnowledgeTurn, 0, len(current)+1)
	for _, old := range current {
		if strings.EqualFold(strings.TrimSpace(old.Question), entry.Question) {
			continue
		}
		out = append(out, old)
	}
	out = append(out, entry)
	if len(out) > verifiedKnowledgeMaxTurns {
		out = append([]VerifiedKnowledgeTurn(nil), out[len(out)-verifiedKnowledgeMaxTurns:]...)
	}
	e.sessionState.VerifiedKnowledge = out
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
}

func boundedVerifiedKnowledgeLedger(in knowledge.EvidenceLedger) knowledge.EvidenceLedger {
	out := knowledge.EvidenceLedger{Query: truncateRunes(strings.TrimSpace(in.Query), 600), Items: []knowledge.EvidenceItem{}}
	for _, item := range in.Items {
		if strings.TrimSpace(item.ChunkID) == "" {
			continue
		}
		item.Title = truncateRunes(strings.TrimSpace(item.Title), 80)
		item.SourceType = truncateRunes(strings.TrimSpace(item.SourceType), 40)
		item.ScoreBucket = truncateRunes(strings.TrimSpace(item.ScoreBucket), 40)
		item.Summary = truncateRunes(strings.TrimSpace(item.Summary), 160)
		item.Snippet = truncateRunes(strings.TrimSpace(item.Snippet), knowledge.DefaultEvidenceSnippetMaxRunes)
		out.Items = append(out.Items, item)
		if len(out.Items) >= knowledge.DefaultEvidenceLedgerMaxItems {
			break
		}
	}
	return out
}

// verifiedKnowledgeLedgerForQuestion combines the most recent verified source
// sets and relabels the ledger with the current resolved question. The verifier
// can therefore check a follow-up such as "粘贴呢" against durable prior RAG
// evidence without pretending that the prior assistant text itself is proof.
func (e *Engine) verifiedKnowledgeLedgerForQuestion(question string) knowledge.EvidenceLedger {
	if e == nil {
		return knowledge.EvidenceLedger{}
	}
	out := knowledge.EvidenceLedger{Query: strings.TrimSpace(question), Items: []knowledge.EvidenceItem{}}
	entries := e.sessionState.VerifiedKnowledge
	for i := len(entries) - 1; i >= 0; i-- {
		ledger := entries[i].Evidence
		ledger.Query = ""
		out = knowledge.MergeEvidenceLedgers(out, ledger, verifiedKnowledgeMaxItems)
		if len(out.Items) >= verifiedKnowledgeMaxItems {
			break
		}
	}
	out.Query = strings.TrimSpace(question)
	return out
}

func (e *Engine) knowledgeLedgerForVerification(question string) knowledge.EvidenceLedger {
	question = strings.TrimSpace(question)
	current := e.searchKnowledgeLedgerThisTurn
	current.Query = question
	prior := e.verifiedKnowledgeLedgerForQuestion(question)
	out := knowledge.MergeEvidenceLedgers(current, prior, verifiedKnowledgeMaxItems)
	out.Query = question
	return out
}

func mergeVerifiedKnowledge(first, second []VerifiedKnowledgeTurn) []VerifiedKnowledgeTurn {
	combined := append(append([]VerifiedKnowledgeTurn(nil), first...), second...)
	out := make([]VerifiedKnowledgeTurn, 0, len(combined))
	positions := map[string]int{}
	for _, entry := range combined {
		key := strings.ToLower(strings.TrimSpace(entry.Question))
		if key == "" || len(entry.Evidence.Items) == 0 {
			continue
		}
		entry.Evidence = boundedVerifiedKnowledgeLedger(entry.Evidence)
		entry.Answer = truncateRunes(strings.TrimSpace(entry.Answer), verifiedKnowledgeAnswerMaxRunes)
		if pos, ok := positions[key]; ok {
			if entry.VerifiedAtUnix >= out[pos].VerifiedAtUnix {
				out[pos] = entry
			}
			continue
		}
		positions[key] = len(out)
		out = append(out, entry)
	}
	if len(out) > verifiedKnowledgeMaxTurns {
		out = append([]VerifiedKnowledgeTurn(nil), out[len(out)-verifiedKnowledgeMaxTurns:]...)
	}
	return out
}
