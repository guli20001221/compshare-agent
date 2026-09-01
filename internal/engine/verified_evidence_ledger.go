package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/knowledge"
)

const (
	verifiedEvidenceMaxTurns            = 4
	verifiedEvidenceMaxItems            = 8
	maxPlatformReadEvidenceSnippetRunes = 6000
)

// rememberVerifiedEvidence stores only the evidence that validated an answer.
// Assistant prose is already in the replayed conversation; persisting it here
// would duplicate semantic history under a different retention policy.
func (e *Engine) rememberVerifiedEvidence(question string, ledger knowledge.EvidenceLedger) {
	if e == nil || len(ledger.Items) == 0 {
		return
	}
	question = strings.TrimSpace(question)
	if question == "" {
		question = strings.TrimSpace(ledger.Query)
	}
	entry := VerifiedEvidenceTurn{
		Question:       truncateRunes(question, 600),
		Evidence:       boundedVerifiedEvidenceLedger(ledger),
		VerifiedAtUnix: time.Now().Unix(),
	}
	if len(entry.Evidence.Items) == 0 {
		return
	}
	current := e.sessionState.VerifiedEvidence
	out := make([]VerifiedEvidenceTurn, 0, len(current)+1)
	for _, old := range current {
		if strings.EqualFold(strings.TrimSpace(old.Question), entry.Question) {
			continue
		}
		out = append(out, old)
	}
	out = append(out, entry)
	if len(out) > verifiedEvidenceMaxTurns {
		out = append([]VerifiedEvidenceTurn(nil), out[len(out)-verifiedEvidenceMaxTurns:]...)
	}
	e.sessionState.VerifiedEvidence = out
	e.sessionState.SchemaVersion = SessionStateSchemaCurrent
	e.markVerifiedEvidenceUpdated()
}

func boundedVerifiedEvidenceLedger(in knowledge.EvidenceLedger) knowledge.EvidenceLedger {
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

// verifiedEvidenceLedgerForQuestion combines the most recent verified source
// sets and relabels the ledger with the current resolved question. The verifier
// can therefore check a follow-up such as "粘贴呢" against persisted prior RAG
// evidence without pretending that the prior assistant text itself is proof.
func (e *Engine) verifiedEvidenceLedgerForQuestion(question string) knowledge.EvidenceLedger {
	if e == nil {
		return knowledge.EvidenceLedger{}
	}
	out := knowledge.EvidenceLedger{Query: strings.TrimSpace(question), Items: []knowledge.EvidenceItem{}}
	entries := e.sessionState.VerifiedEvidence
	for i := len(entries) - 1; i >= 0; i-- {
		ledger := entries[i].Evidence
		ledger.Query = ""
		out = knowledge.MergeEvidenceLedgers(out, ledger, verifiedEvidenceMaxItems)
		if len(out.Items) >= verifiedEvidenceMaxItems {
			break
		}
	}
	out.Query = strings.TrimSpace(question)
	return out
}

// currentTurnEvidenceLedger is everything THIS turn gathered: platform reads plus
// this turn's retrieval. It is the only ledger an answer may be GENERATED from and
// the only one that may be STORED as an answer's evidence.
//
// The distinction against knowledgeLedgerForVerification is not stylistic. Prior
// verified evidence is legitimate to CHECK a new answer against — a follow-up that
// retrieves nothing of its own has nothing else to be checked against. It is not
// legitimate to WRITE a new answer from, because "the question we retrieved for"
// and "the question being answered" are then different questions, and the user
// cannot tell. Nor may it be stored again: re-stamping a chunk into each new entry
// gives it a fresh VerifiedAtUnix every turn, so evidence retrieved once never
// leaves the 4-turn window.
func (e *Engine) currentTurnEvidenceLedger(question string) knowledge.EvidenceLedger {
	question = strings.TrimSpace(question)
	tools := e.currentReadEvidenceLedger(question)
	current := e.searchKnowledgeLedgerThisTurn
	current.Query = question
	// The Agent has already seen every current-turn tool and retrieval item. The
	// verifier must judge against that same evidence set; the small persisted-ledger
	// cap must not truncate later searches and create false negatives.
	limit := len(tools.Items) + len(current.Items)
	if limit == 0 {
		return knowledge.EvidenceLedger{Query: question, Items: []knowledge.EvidenceItem{}}
	}
	out := knowledge.MergeEvidenceLedgers(tools, current, limit)
	out.Query = question
	return out
}

func (e *Engine) knowledgeLedgerForVerification(question string) knowledge.EvidenceLedger {
	question = strings.TrimSpace(question)
	out := e.currentTurnEvidenceLedger(question)
	// Prior verified evidence remains separately bounded. It is supplementary and
	// never displaces evidence gathered for the current answer.
	prior := e.verifiedEvidenceLedgerForQuestion(question)
	out = knowledge.MergeEvidenceLedgers(out, prior, len(out.Items)+verifiedEvidenceMaxItems)
	out.Query = question
	return out
}

// currentReadEvidenceLedger projects server-produced platform facts into the
// same per-response proof ledger as RAG evidence. It is deliberately turn-local:
// current image lists, prices, stock and instance state must never be promoted
// into cross-turn knowledge evidence.
func (e *Engine) currentReadEvidenceLedger(question string) knowledge.EvidenceLedger {
	out := knowledge.EvidenceLedger{Query: strings.TrimSpace(question), Items: []knowledge.EvidenceItem{}}
	for index, evidence := range e.platformReadEvidenceThisTurn {
		raw, err := json.Marshal(evidence.Envelope)
		if err != nil {
			continue
		}
		snippet := strings.TrimSpace(evidence.Reply)
		if snippet != "" {
			snippet += "\n"
		}
		snippet += string(raw)
		out.Items = append(out.Items, knowledge.EvidenceItem{
			ChunkID:    fmt.Sprintf("turn-read-%d", index+1),
			Title:      truncateRunes("本轮平台查询："+evidence.Capability, 80),
			SourceType: "platform_tool",
			Summary:    truncateRunes(strings.TrimSpace(evidence.Reply), 600),
			Snippet:    truncateRunes(snippet, maxPlatformReadEvidenceSnippetRunes-1),
		})
	}
	return out
}

func mergeVerifiedEvidence(first, second []VerifiedEvidenceTurn) []VerifiedEvidenceTurn {
	combined := append(append([]VerifiedEvidenceTurn(nil), first...), second...)
	out := make([]VerifiedEvidenceTurn, 0, len(combined))
	positions := map[string]int{}
	for _, entry := range combined {
		key := strings.ToLower(strings.TrimSpace(entry.Question))
		if key == "" || len(entry.Evidence.Items) == 0 {
			continue
		}
		entry.Evidence = boundedVerifiedEvidenceLedger(entry.Evidence)
		if pos, ok := positions[key]; ok {
			if entry.VerifiedAtUnix >= out[pos].VerifiedAtUnix {
				out[pos] = entry
			}
			continue
		}
		positions[key] = len(out)
		out = append(out, entry)
	}
	if len(out) > verifiedEvidenceMaxTurns {
		out = append([]VerifiedEvidenceTurn(nil), out[len(out)-verifiedEvidenceMaxTurns:]...)
	}
	return out
}
