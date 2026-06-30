package engine

import (
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/knowledge/agentic"
	"github.com/compshare-agent/internal/observability"
)

func (e *Engine) adoptAgenticReferenceLedger(result *agentic.AgenticRetrievalResult, ledger *knowledge.EvidenceLedger) {
	if result == nil {
		return
	}
	result.ReferenceLedger.RefIDScheme = agenticRefIDScheme
	existing := append([]agentic.Reference(nil), e.searchKnowledgeReferenceLedgerThisTurn.References...)
	refByChunk := map[string]string{}
	maxRef := 0
	for _, ref := range existing {
		chunkID := strings.TrimSpace(ref.ChunkID)
		refID := strings.TrimSpace(ref.RefID)
		if chunkID != "" && refID != "" {
			refByChunk[chunkID] = refID
		}
		if n, err := strconv.Atoi(refID); err == nil && n > maxRef {
			maxRef = n
		}
	}
	for i := range result.ReferenceLedger.References {
		ref := &result.ReferenceLedger.References[i]
		chunkID := strings.TrimSpace(ref.ChunkID)
		if chunkID == "" {
			continue
		}
		if refID := refByChunk[chunkID]; refID != "" {
			ref.RefID = refID
			continue
		}
		maxRef++
		ref.RefID = strconv.Itoa(maxRef)
		ref.Rank = len(existing) + 1
		refByChunk[chunkID] = ref.RefID
		existing = append(existing, *ref)
	}
	if ledger != nil {
		for i := range ledger.Items {
			if refID := refByChunk[strings.TrimSpace(ledger.Items[i].ChunkID)]; refID != "" {
				ledger.Items[i].RefID = refID
			}
		}
	}
	e.searchKnowledgeReferenceLedgerThisTurn = agentic.ReferenceLedger{
		RefIDScheme: agenticRefIDScheme,
		References:  existing,
	}
}

func (e *Engine) currentSearchKnowledgeCitationLedger(query string) knowledge.EvidenceLedger {
	ledger := e.searchKnowledgeLedgerThisTurn
	if len(ledger.Items) == 0 && len(e.searchKnowledgeHitsThisTurn) > 0 {
		ledger = knowledge.BuildSubstantiveEvidenceLedger(query, e.searchKnowledgeHitsThisTurn, searchKnowledgeLedgerTurnMaxItems, 0)
	}
	if len(ledger.Items) == 0 {
		return ledger
	}
	refByChunk := map[string]string{}
	for _, ref := range e.searchKnowledgeReferenceLedgerThisTurn.References {
		chunkID := strings.TrimSpace(ref.ChunkID)
		refID := strings.TrimSpace(ref.RefID)
		if chunkID != "" && refID != "" {
			refByChunk[chunkID] = refID
		}
	}
	for i := range ledger.Items {
		if strings.TrimSpace(ledger.Items[i].RefID) != "" {
			continue
		}
		if refID := refByChunk[strings.TrimSpace(ledger.Items[i].ChunkID)]; refID != "" {
			ledger.Items[i].RefID = refID
			continue
		}
		ledger.Items[i].RefID = strconv.Itoa(i + 1)
	}
	return ledger
}

func (e *Engine) searchKnowledgeHitsInReferenceOrder() []knowledge.RetrievalHit {
	if len(e.searchKnowledgeHitsThisTurn) == 0 {
		return nil
	}
	hitByChunk := map[string]knowledge.RetrievalHit{}
	for _, hit := range e.searchKnowledgeHitsThisTurn {
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		if _, exists := hitByChunk[chunkID]; !exists {
			hitByChunk[chunkID] = hit
		}
	}
	ordered := make([]knowledge.RetrievalHit, 0, len(hitByChunk))
	seen := map[string]struct{}{}
	for _, ref := range e.searchKnowledgeReferenceLedgerThisTurn.References {
		chunkID := strings.TrimSpace(ref.ChunkID)
		if chunkID == "" {
			continue
		}
		hit, ok := hitByChunk[chunkID]
		if !ok {
			continue
		}
		ordered = append(ordered, hit)
		seen[chunkID] = struct{}{}
	}
	if len(ordered) > 0 {
		return ordered
	}
	for _, hit := range e.searchKnowledgeHitsThisTurn {
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		if _, ok := seen[chunkID]; ok {
			continue
		}
		ordered = append(ordered, hit)
		seen[chunkID] = struct{}{}
	}
	return ordered
}

func (e *Engine) citedRefsForChunkIDs(chunkIDs []string) []string {
	if len(chunkIDs) == 0 {
		return nil
	}
	byChunk := map[string]string{}
	for _, ref := range e.searchKnowledgeReferenceLedgerThisTurn.References {
		chunkID := strings.TrimSpace(ref.ChunkID)
		refID := strings.TrimSpace(ref.RefID)
		if chunkID != "" && refID != "" {
			byChunk[chunkID] = refID
		}
	}
	out := []string{}
	for _, chunkID := range chunkIDs {
		if refID := byChunk[strings.TrimSpace(chunkID)]; refID != "" {
			out = appendUniqueString(out, refID)
			continue
		}
		for i, hit := range e.searchKnowledgeHitsThisTurn {
			if strings.TrimSpace(hit.Chunk.ChunkID) == strings.TrimSpace(chunkID) {
				out = appendUniqueString(out, strconv.Itoa(i+1))
				break
			}
		}
	}
	return out
}

func (e *Engine) emitCitedReferenceTrace(citedRefs, citedChunkIDs []string) {
	if len(citedRefs) == 0 && len(citedChunkIDs) == 0 {
		return
	}
	e.emitRetrievalTrace(observability.RetrievalTrace{
		RefIDScheme:   agenticRefIDScheme,
		CitedRefs:     citedRefs,
		CitedChunkIDs: citedChunkIDs,
	})
}
