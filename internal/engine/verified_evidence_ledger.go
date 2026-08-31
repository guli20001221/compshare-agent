package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/tools"
)

const (
	verifiedEvidenceMaxTurns = 4
	verifiedEvidenceMaxItems = 8
	// Entries written before scope metadata existed cannot prove product/source
	// compatibility or that a hit was below the retrieval floor. Treating those
	// missing fields as trusted zero values would silently widen old evidence.
	verifiedEvidenceMetadataVersion = 1
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
		Question:                truncateRunes(question, 600),
		Evidence:                boundedVerifiedEvidenceLedger(ledger),
		EvidenceMetadataVersion: verifiedEvidenceMetadataVersion,
	}
	entry.EvidenceMetadata = e.verifiedEvidenceMetadata(entry.Evidence)
	entry.VerifiedAtUnix = time.Now().Unix()
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
		item.ProductArea = truncateRunes(strings.TrimSpace(item.ProductArea), 80)
		item.SourceOrigin = truncateRunes(strings.TrimSpace(item.SourceOrigin), 80)
		item.Confidence = truncateRunes(strings.TrimSpace(item.Confidence), 40)
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
		if entries[i].EvidenceMetadataVersion != verifiedEvidenceMetadataVersion {
			continue
		}
		ledger := evidenceWithVerifiedMetadata(entries[i])
		ledger.Query = ""
		out = knowledge.MergeEvidenceLedgers(out, ledger, verifiedEvidenceMaxItems)
		if len(out.Items) >= verifiedEvidenceMaxItems {
			break
		}
	}
	out.Query = strings.TrimSpace(question)
	return out
}

func (e *Engine) verifiedEvidenceMetadata(ledger knowledge.EvidenceLedger) map[string]VerifiedEvidenceMetadata {
	out := make(map[string]VerifiedEvidenceMetadata)
	for _, item := range ledger.Items {
		id := strings.TrimSpace(item.ChunkID)
		metadata := VerifiedEvidenceMetadata{
			ProductArea:  strings.TrimSpace(item.ProductArea),
			SourceOrigin: strings.TrimSpace(item.SourceOrigin),
			Confidence:   strings.TrimSpace(item.Confidence),
			BelowFloor:   item.BelowFloor,
		}
		if _, ok := e.belowFloorKnowledgeIDsThisTurn[id]; ok {
			metadata.BelowFloor = true
		}
		if id != "" && metadata != (VerifiedEvidenceMetadata{}) {
			out[id] = metadata
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func evidenceWithVerifiedMetadata(entry VerifiedEvidenceTurn) knowledge.EvidenceLedger {
	ledger := entry.Evidence
	for i := range ledger.Items {
		metadata := entry.EvidenceMetadata[strings.TrimSpace(ledger.Items[i].ChunkID)]
		ledger.Items[i].ProductArea = metadata.ProductArea
		ledger.Items[i].SourceOrigin = metadata.SourceOrigin
		ledger.Items[i].Confidence = metadata.Confidence
		ledger.Items[i].BelowFloor = metadata.BelowFloor
	}
	return ledger
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
	toolEvidence := knowledge.EvidenceLedger{Query: question, Items: e.agentToolEvidenceThisTurn}
	out = knowledge.MergeEvidenceLedgers(out, toolEvidence, len(out.Items)+len(toolEvidence.Items))
	// A committed write is an authoritative fact for this final narration, but it
	// must never become cross-turn evidence. The durable session already records
	// resource identity through its dedicated state; replaying an old success as
	// proof of current state would be unsafe.
	committed := e.currentCommittedWriteEvidenceLedger(question)
	out = knowledge.MergeEvidenceLedgers(out, committed, len(out.Items)+len(committed.Items))
	// Prior verified evidence remains separately bounded. It is supplementary and
	// never displaces evidence gathered for the current answer.
	prior := e.verifiedEvidenceLedgerForQuestion(question)
	out = knowledge.MergeEvidenceLedgers(out, prior, len(out.Items)+verifiedEvidenceMaxItems)
	out.Query = question
	return out
}

func (e *Engine) recordAgentToolEvidence(action, observation string) {
	result, ok := tools.ParseAgentToolResult(observation)
	if !ok || result.Status != tools.AgentToolStatusSuccess {
		return
	}
	e.agentToolEvidenceThisTurn = append(e.agentToolEvidenceThisTurn, knowledge.EvidenceItem{
		ChunkID:      fmt.Sprintf("turn-tool-%d", len(e.agentToolEvidenceThisTurn)+1),
		SourceOrigin: "live_platform",
		Confidence:   "current",
		SourceType:   "platform_tool",
		ScoreBucket:  "high",
		Title:        truncateRunes("本轮工具结果："+strings.TrimSpace(action), 80),
		Summary:      "本轮已成功执行该工具；只能使用返回数据直接支持的结论。",
		Snippet:      truncateRunes(strings.TrimSpace(observation), 2000),
	})
}

func (e *Engine) currentCommittedWriteEvidenceLedger(question string) knowledge.EvidenceLedger {
	out := knowledge.EvidenceLedger{Query: strings.TrimSpace(question), Items: []knowledge.EvidenceItem{}}
	for index, reply := range e.committedWriteRepliesThisTurn {
		if reply = strings.TrimSpace(reply); reply == "" {
			continue
		}
		out.Items = append(out.Items, knowledge.EvidenceItem{
			ChunkID:      fmt.Sprintf("turn-write-%d", index+1),
			SourceOrigin: "live_platform",
			Confidence:   "current",
			SourceType:   "platform_tool",
			ScoreBucket:  "high",
			Title:        "本轮已提交的写操作",
			Summary:      truncateRunes(reply, 600),
			Snippet:      truncateRunes(reply, 2000),
		})
	}
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
			ChunkID:      fmt.Sprintf("turn-read-%d", index+1),
			SourceOrigin: "live_platform",
			Confidence:   "current",
			SourceType:   "platform_tool",
			ScoreBucket:  "high",
			Title:        truncateRunes("本轮平台查询："+evidence.Capability, 80),
			Summary:      truncateRunes(strings.TrimSpace(evidence.Reply), 600),
			Snippet:      truncateRunes(snippet, maxEvidenceGatewayFactRunes-1),
		})
	}
	if len(e.verbatimBlocksThisTurn) > 0 {
		out.Items = append(out.Items, knowledge.EvidenceItem{
			ChunkID:      "turn-read-billing-card",
			SourceOrigin: "live_platform",
			Confidence:   "current",
			SourceType:   "platform_tool",
			ScoreBucket:  "high",
			Title:        "本轮结构化费用卡",
			Summary:      "服务器已向用户展示本轮实时费用卡；具体金额不在模型证据中，答复不得复述或估算金额。",
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
