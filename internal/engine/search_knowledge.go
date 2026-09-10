package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

// SearchKnowledge is the Agent's one retrieval capability: it executes the
// Agent's query once against the configured MCP and returns a bounded evidence
// ledger. Everything in this file is that call and the trace it emits — the
// evidence floor, the weak/ambiguous classifiers, and the retrieval activity
// records. Ranking policy stays in internal/knowledge; this file only decides
// what the Agent observes and what the trace records.

func evidencesFromRetrievalHits(items []knowledge.RetrievalHit, queryNormalized string) ([]envelope.Evidence, error) {
	evidences := make([]envelope.Evidence, 0, len(items))
	producedAt := time.Now().UTC()
	for _, item := range items {
		score := item.Score
		var surfaceURL *string
		if strings.TrimSpace(item.Chunk.SourceURL) != "" {
			url := strings.TrimSpace(item.Chunk.SourceURL)
			surfaceURL = &url
		}
		evidence, err := envelope.NewEvidence(envelope.EvidenceInput{
			SourceTitle:     item.Chunk.Title,
			Snippet:         item.Chunk.Content,
			SurfaceURL:      surfaceURL,
			EvidenceKind:    envelope.EvidenceKindKnowledge,
			ChunkID:         item.Chunk.ChunkID,
			KBVersion:       item.Chunk.KBVersion,
			RetrievalScore:  &score,
			QueryNormalized: queryNormalized,
			ProducedAt:      producedAt,
		})
		if err != nil {
			return nil, err
		}
		evidences = append(evidences, evidence)
	}
	return evidences, nil
}

func projectEvidenceTraceHits(evidences []envelope.Evidence, items []knowledge.RetrievalHit) []observability.RetrievalHit {
	hits := make([]observability.RetrievalHit, 0, len(evidences))
	for index, evidence := range evidences {
		view := evidence.ForTrace()
		kept := true
		var item knowledge.RetrievalHit
		if index < len(items) {
			item = items[index]
			kept = item.Kept
		}
		hits = append(hits, observability.RetrievalHit{
			ChunkID: view.ChunkID,
			// SourceArea is the chunk's declared product_area.
			SourceArea: item.Chunk.ProductArea,
			Score:      view.RetrievalScore,
			Kept:       kept,
			// RRF trace fields. Zero values omitted via json omitempty
			// for non-qwen3_rrf modes; populated when knowledge.Retriever
			// ran the qwen3_rrf branch.
			BM25Rank:    item.BM25Rank,
			DenseRank:   item.DenseRank,
			FusionRank:  item.FusionRank,
			FusionScore: item.FusionScore,
		})
	}
	return hits
}

func retrievalReferencesFromHits(items []knowledge.RetrievalHit, activityID string) []observability.RetrievalReference {
	refs := make([]observability.RetrievalReference, 0, len(items))
	for i, item := range items {
		chunkID := strings.TrimSpace(item.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		ref := observability.RetrievalReference{
			RefID:      strconv.Itoa(len(refs) + 1),
			ChunkID:    chunkID,
			Title:      strings.TrimSpace(item.Chunk.Title),
			SourceArea: strings.TrimSpace(item.Chunk.ProductArea),
			Score:      item.Score,
			Rank:       i + 1,
		}
		if activityID != "" {
			ref.ActivityIDs = []string{activityID}
		}
		refs = append(refs, ref)
	}
	return refs
}

func retrievalReferencesFromLedgerActivities(ledger knowledge.EvidenceLedger, hits []knowledge.RetrievalHit, activityIDsByChunkID map[string][]string, fallbackActivityID string) []observability.RetrievalReference {
	if len(ledger.Items) == 0 {
		return retrievalReferencesFromHits(hits, fallbackActivityID)
	}
	hitByChunkID := make(map[string]knowledge.RetrievalHit, len(hits))
	for _, hit := range hits {
		if id := strings.TrimSpace(hit.Chunk.ChunkID); id != "" {
			hitByChunkID[id] = hit
		}
	}
	refs := make([]observability.RetrievalReference, 0, len(ledger.Items))
	for _, item := range ledger.Items {
		chunkID := strings.TrimSpace(item.ChunkID)
		if chunkID == "" {
			continue
		}
		ref := observability.RetrievalReference{
			RefID:      strconv.Itoa(len(refs) + 1),
			ChunkID:    chunkID,
			Title:      strings.TrimSpace(item.Title),
			SourceArea: strings.TrimSpace(item.ProductArea),
			Rank:       len(refs) + 1,
		}
		if hit, ok := hitByChunkID[chunkID]; ok {
			if ref.Title == "" {
				ref.Title = strings.TrimSpace(hit.Chunk.Title)
			}
			if ref.SourceArea == "" {
				ref.SourceArea = strings.TrimSpace(hit.Chunk.ProductArea)
			}
			ref.Score = hit.Score
		}
		if ids := append([]string(nil), activityIDsByChunkID[chunkID]...); len(ids) > 0 {
			ref.ActivityIDs = ids
		} else if fallbackActivityID != "" {
			ref.ActivityIDs = []string{fallbackActivityID}
		}
		refs = append(refs, ref)
	}
	return refs
}

func (e *Engine) recordSearchKnowledgeActivity(activity observability.RetrievalActivity, hits []knowledge.RetrievalHit) {
	if strings.TrimSpace(activity.ID) == "" {
		return
	}
	e.searchKnowledgeActivitiesThisTurn = append(e.searchKnowledgeActivitiesThisTurn, activity)
	if len(hits) == 0 {
		return
	}
	if e.searchKnowledgeActivityIDsByChunkID == nil {
		e.searchKnowledgeActivityIDsByChunkID = map[string][]string{}
	}
	for _, hit := range hits {
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		e.searchKnowledgeActivityIDsByChunkID[chunkID] = appendUniqueString(e.searchKnowledgeActivityIDsByChunkID[chunkID], activity.ID)
	}
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func citedRefsFromChunkIDs(chunkIDs []string, refs []observability.RetrievalReference) []observability.RetrievalCitedRef {
	if len(chunkIDs) == 0 || len(refs) == 0 {
		return nil
	}
	byChunkID := make(map[string]observability.RetrievalReference, len(refs))
	for _, ref := range refs {
		if ref.ChunkID != "" {
			byChunkID[ref.ChunkID] = ref
		}
	}
	out := make([]observability.RetrievalCitedRef, 0, len(chunkIDs))
	seen := map[string]struct{}{}
	for _, chunkID := range chunkIDs {
		ref, ok := byChunkID[chunkID]
		if !ok {
			continue
		}
		key := ref.RefID + "\x00" + ref.ChunkID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, observability.RetrievalCitedRef{RefID: ref.RefID, ChunkID: ref.ChunkID})
	}
	return out
}

// isWeakEvidence reports whether the top hit's score is below the weak-evidence
// threshold for the retrieval path that produced it. hybridMode comes from
// knowledge.RetrievalResult.HybridMode and tracks the actual scoring path used
// (including bm25_fallback when a hybrid mode degraded to BM25 mid-flight).
// Treat unknown or empty values as BM25 for conservative score handling.
//
// rerankerScored distinguishes qwen3_rrf reranker scores from its fallback RRF
// scores. The latter use a different scale, so no reranker floor is applied.
func isWeakEvidence(items []knowledge.RetrievalHit, hybridMode string, rerankerScored bool) bool {
	return knowledge.IsWeakEvidence(items, hybridMode, rerankerScored)
}

// appliedFloor is the SINGLE producer of "was a relevance floor applied to these
// hits, and what was it". Both the verdict (isWeakEvidence) and the trace
// (RetrievalTrace.FloorValue) read it, so they cannot describe different events.
//
// judged is false in every case where no comparison occurs:
//   - no hits: there is nothing to compare.
//   - unknown score scale: a remote reported a scoring path this build never
//     calibrated. Guessing picks BM25's 55.0, which rejects an entire [0,1]
//     scale — see normalizeRemoteScoreScale.
//   - qwen3_rrf without reranker scores: fallback scores use the RRF scale.
func appliedFloor(items []knowledge.RetrievalHit, hybridMode string, rerankerScored bool) (float64, bool) {
	return knowledge.AppliedEvidenceFloor(items, hybridMode, rerankerScored)
}

// strongKnowledgeBodyEligibleIDs returns only ledger items whose individual
// score crossed a calibrated relevance floor. A strong top hit does not make
// every lower-ranked item strong, and an unknown/RRF-fallback score scale is not
// guessed. Those entries keep their bounded snippet and remain explicitly
// readable through ReadChunk.
func strongKnowledgeBodyEligibleIDs(
	hits []knowledge.RetrievalHit,
	ledger knowledge.EvidenceLedger,
	hybridMode string,
	rerankerScored bool,
) []string {
	floor, judged := appliedFloor(hits, hybridMode, rerankerScored)
	if !judged || len(ledger.Items) == 0 {
		return nil
	}
	inLedger := make(map[string]struct{}, len(ledger.Items))
	for _, item := range ledger.Items {
		if id := strings.TrimSpace(item.ChunkID); id != "" {
			inLedger[id] = struct{}{}
		}
	}
	eligible := make(map[string]struct{}, len(inLedger))
	for _, hit := range hits {
		id := strings.TrimSpace(hit.Chunk.ChunkID)
		if !hit.Kept || id == "" || hit.Score < floor {
			continue
		}
		if _, ok := inLedger[id]; !ok {
			continue
		}
		eligible[id] = struct{}{}
	}
	ids := make([]string, 0, len(eligible))
	for _, item := range ledger.Items {
		id := strings.TrimSpace(item.ChunkID)
		if _, ok := eligible[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// isRankingAmbiguous reports whether the top two hits are close enough on the
// scoring scale that ranking is essentially a tie. Only feeds telemetry
// (trace.RankingErrorCandidate); does NOT influence the RAG prompt or refusal
// path. Mode-aware so the spread threshold matches the score scale in use.
func isRankingAmbiguous(items []knowledge.RetrievalHit, hybridMode string) bool {
	if len(items) < 2 {
		return false
	}
	if knowledge.ScoreScaleFor(hybridMode) == knowledge.ScoreScaleUnknown {
		// A spread is only meaningful against a known scale, for the same reason
		// the floor is. This one is telemetry-only, so guessing would not change
		// an answer — it would mark nearly every remote turn a ranking-error
		// candidate (the BM25 spread is wide relative to a [0,1] scale) and make
		// the metric useless exactly when someone is using it to diagnose the
		// remote.
		return false
	}
	return items[0].Score-items[1].Score < rankingAmbiguousSpreadFor(hybridMode)
}

// weakEvidenceThresholdFor maps a knowledge.RetrievalResult.HybridMode value to
// the appropriate weak-evidence floor. See cited_guard.go for the rationale
// behind each scale. The empty string and any unrecognized value default to
// the BM25 threshold so existing tests with mock RetrievalResult{} keep their
// fixture-pinned behavior.
func weakEvidenceThresholdFor(hybridMode string) float64 {
	return knowledge.WeakEvidenceThresholdFor(hybridMode)
}

// rankingAmbiguousSpreadFor maps a score scale to the spread under which the top
// two hits are considered tied. Keyed by scale for the same reason the floor is:
// a spread is a distance on a scale, not a property of a pipeline.
func rankingAmbiguousSpreadFor(hybridMode string) float64 {
	switch knowledge.ScoreScaleFor(hybridMode) {
	case knowledge.ScoreScaleSemantic:
		return rankingAmbiguousSemanticSpread
	default:
		return rankingAmbiguousBM25Spread
	}
}

func (e *Engine) emitRetrievalTrace(trace observability.RetrievalTrace) {
	if e.retrievalTraceObserver == nil {
		return
	}
	e.retrievalTraceObserver(trace)
}

// executeSearchKnowledge retrieves bounded evidence for the Agent to cite. It is
// read-only and does not pass through SafeToolExecutor.
func (e *Engine) executeSearchKnowledge(ctx context.Context, args map[string]any, onStep func(StepEvent)) string {
	query := strings.TrimSpace(searchKnowledgeArg(args, "query"))
	hint := strings.TrimSpace(searchKnowledgeArg(args, "context_hint"))
	if query == "" {
		query = hint
	}
	answerQuestion := e.knowledgeAnswerQuestion(query)
	knowledgeSource := e.knowledgeToolSource()
	onStep(StepEvent{
		Type: StepToolCall, Action: "SearchKnowledge", Source: knowledgeSource,
		Args: map[string]any{"answer_question": answerQuestion, "queries": []string{query}},
	})
	if e.agentToolCallsThisTurn("SearchKnowledge") >= maxSearchKnowledgeCallsPerTurn {
		onStep(StepEvent{Type: StepToolResult, Action: "SearchKnowledge", Source: knowledgeSource, Message: "本轮检索次数已达上限"})
		return "{\"EvidenceLedger\":{\"items\":[]},\"empty\":true,\"search_limit_reached\":true}"
	}
	if e.knowledgeRetriever == nil || query == "" {
		onStep(StepEvent{Type: StepToolResult, Action: "SearchKnowledge", Source: knowledgeSource, Message: "知识库不可用"})
		return searchKnowledgeResultJSON(knowledge.EvidenceLedger{Query: answerQuestion}, "", nil)
	}

	// One activity is recorded per retrieval below, so the activity list numbers
	// the queries; a call with no query or retriever returned above and adds none.
	activityID := fmt.Sprintf("search_%d", len(e.searchKnowledgeActivitiesThisTurn)+1)
	retrieved := e.knowledgeRetriever.RetrieveContext(ctx, query, hint)
	rawHits := retrieved.HitItems
	if retrieved.Unavailable {
		rawHits = nil
	}
	hits := rawHits
	floorDroppedAll := isWeakEvidence(rawHits, retrieved.HybridMode, retrieved.RerankerMode != "") && len(rawHits) > 0
	if floorDroppedAll {
		hits = nil
	}
	ledger := knowledge.BuildSubstantiveEvidenceLedger(answerQuestion, hits, knowledge.DefaultEvidenceLedgerMaxItems, 0)
	e.recordSearchKnowledgeCapabilities(retrieved.SearchID, ledger)
	e.searchKnowledgeHitsThisTurn = append(e.searchKnowledgeHitsThisTurn, hits...)
	activity := observability.RetrievalActivity{ID: activityID, Query: query, FloorDroppedAll: floorDroppedAll}
	if !retrieved.Unavailable {
		activity.Hits = len(retrieved.Hits)
	}
	e.recordSearchKnowledgeActivity(activity, hits)
	e.emitSearchKnowledgeRetrievalTrace(answerQuestion, query, retrieved, rawHits, floorDroppedAll, activityID)

	resultMeta := map[string]any{}
	var autoExpandedIDs []string
	strongBodyIDs := strongKnowledgeBodyEligibleIDs(rawHits, ledger, retrieved.HybridMode, retrieved.RerankerMode != "")
	if len(strongBodyIDs) > 0 {
		expanded := e.autoMaterializeKnowledgeChunks(ctx, &ledger, strongBodyIDs)
		autoExpandedIDs = expanded.ReadIDs
		if len(expanded.ReadIDs) > 0 {
			resultMeta["auto_expanded_chunk_ids"] = expanded.ReadIDs
		}
		if len(expanded.TruncatedIDs) > 0 {
			resultMeta["auto_expansion_truncated_ids"] = expanded.TruncatedIDs
		}
		if expanded.Unavailable {
			resultMeta["auto_expansion_unavailable"] = true
		}
	}
	e.searchKnowledgeLedgerThisTurn = knowledge.MergeEvidenceLedgers(e.searchKnowledgeLedgerThisTurn, ledger, searchKnowledgeLedgerTurnMaxItems)
	overwriteEvidenceSnippets(&e.searchKnowledgeLedgerThisTurn, ledger, autoExpandedIDs)
	message := "搜索完成"
	unavailableQueries := 0
	if retrieved.Unavailable {
		message = "知识库服务暂时不可用"
		unavailableQueries = 1
	}
	onStep(StepEvent{
		Type: StepToolResult, Action: "SearchKnowledge", Source: knowledgeSource, Message: message,
		TraceResult: map[string]any{"items": len(ledger.Items), "queries": 1, "unavailable_queries": unavailableQueries},
	})
	if retrieved.Unavailable {
		resultMeta["knowledge_unavailable"] = true
		resultMeta["error"] = "知识库服务暂时不可用，请稍后重试。"
		return searchKnowledgeResultJSON(ledger, "", resultMeta)
	}
	if len(ledger.Items) == 0 && floorDroppedAll {
		candidates := belowFloorKnowledgeCandidates(rawHits, retrieved.SearchID)
		e.recordBelowFloorKnowledgeCapabilities(candidates)
		resultMeta["floor_dropped_all"] = true
		resultMeta["below_floor_candidates"] = candidates
		resultMeta["note"] = "候选内容均低于相关性门槛，尚未形成可引用证据。可先用 ReadChunk 读取待核验候选全文，或改写后重新检索；" +
			"读取前不得引用；读取后仍按低置信证据使用，必要时说明不确定性，不得当成高置信证据。"
	}
	return searchKnowledgeResultJSON(ledger, "", resultMeta)
}

const maxBelowFloorKnowledgeCandidates = 3

type belowFloorKnowledgeCandidate struct {
	ChunkID  string `json:"chunk_id"`
	Title    string `json:"title,omitempty"`
	Strength string `json:"strength"`
	searchID string
}

func belowFloorKnowledgeCandidates(
	hits []knowledge.RetrievalHit,
	searchID string,
) []belowFloorKnowledgeCandidate {
	candidates := make([]belowFloorKnowledgeCandidate, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if !hit.Kept || chunkID == "" {
			continue
		}
		if _, exists := seen[chunkID]; exists {
			continue
		}
		seen[chunkID] = struct{}{}
		candidates = append(candidates, belowFloorKnowledgeCandidate{
			ChunkID:  chunkID,
			Title:    truncateRunes(strings.TrimSpace(hit.Chunk.Title), 80),
			Strength: "below_floor",
			searchID: strings.TrimSpace(searchID),
		})
		if len(candidates) == maxBelowFloorKnowledgeCandidates {
			break
		}
	}
	return candidates
}

func (e *Engine) recordBelowFloorKnowledgeCapabilities(candidates []belowFloorKnowledgeCandidate) {
	for _, candidate := range candidates {
		if candidate.ChunkID == "" {
			continue
		}
		if e.belowFloorKnowledgeIDsThisTurn == nil {
			e.belowFloorKnowledgeIDsThisTurn = map[string]struct{}{}
		}
		e.belowFloorKnowledgeIDsThisTurn[candidate.ChunkID] = struct{}{}
		if candidate.searchID == "" {
			continue
		}
		if e.searchKnowledgeCapabilitiesThisTurn == nil {
			e.searchKnowledgeCapabilitiesThisTurn = map[string]string{}
		}
		e.searchKnowledgeCapabilitiesThisTurn[candidate.ChunkID] = candidate.searchID
	}
}

func (e *Engine) recordSearchKnowledgeCapabilities(searchID string, ledger knowledge.EvidenceLedger) {
	searchID = strings.TrimSpace(searchID)
	if searchID == "" {
		return
	}
	if e.searchKnowledgeCapabilitiesThisTurn == nil {
		e.searchKnowledgeCapabilitiesThisTurn = map[string]string{}
	}
	for _, item := range ledger.Items {
		chunkID := strings.TrimSpace(item.ChunkID)
		if chunkID != "" {
			// A new search supersedes a prior capability for the same chunk. The
			// previous search_id is deliberately not retained as a fallback.
			e.searchKnowledgeCapabilitiesThisTurn[chunkID] = searchID
			// A later calibrated strong result supersedes an earlier weak-candidate
			// view of the same chunk in this turn.
			delete(e.belowFloorKnowledgeIDsThisTurn, chunkID)
		}
	}
}

// emitSearchKnowledgeRetrievalTrace records the agent-lane SearchKnowledge
// retrieval as a RetrievalTrace so it is observable in traces/eval. Enabled + Hits +
// HitItems (the retrieved chunk_ids) populate rec.retrieval; an empty/no-hit
// retrieval honestly records refused_reason=no_evidence (so a corpus-gap query
// is visible, not silently presented as grounded). This is the RETRIEVED set,
// not the cited set.
func (e *Engine) emitSearchKnowledgeRetrievalTrace(answerQuestion, query string, retrieved knowledge.RetrievalResult, hitItems []knowledge.RetrievalHit, floorDroppedAll bool, activityID string) {
	if len(hitItems) == 0 && len(retrieved.Hits) > 0 {
		hitItems = make([]knowledge.RetrievalHit, 0, len(retrieved.Hits))
		for _, chunk := range retrieved.Hits {
			hitItems = append(hitItems, knowledge.RetrievalHit{Chunk: chunk, Kept: true})
		}
	}
	// hitItems is what the floor was (or was not) applied to, so the trace value
	// comes from the same producer as the verdict rather than being re-derived
	// from the mode alone. An unavailable retrieval and an empty result both
	// arrive here with no hits, and both correctly record no floor.
	appliedFloorValue, _ := appliedFloor(hitItems, retrieved.HybridMode, retrieved.RerankerMode != "")
	trace := observability.RetrievalTrace{
		Enabled:                retrieved.Enabled,
		Unavailable:            retrieved.Unavailable,
		FailureReason:          retrieved.FailureReason,
		KBVersion:              retrieved.KBVersion,
		AnswerQuestion:         answerQuestion,
		QueryRaw:               query,
		QueryNormalized:        retrieved.QueryNormalized,
		Hits:                   len(retrieved.Hits),
		HybridMode:             retrieved.HybridMode,
		HybridFallbackReason:   retrieved.HybridFallbackReason,
		EmbeddingLatencyMS:     retrieved.EmbeddingLatencyMS,
		EmbeddingModel:         retrieved.EmbeddingModel,
		RerankerMode:           retrieved.RerankerMode,
		RerankerLatencyMS:      retrieved.RerankerLatencyMS,
		RerankerFallbackReason: retrieved.RerankerFallbackReason,
		FloorDroppedAll:        floorDroppedAll,
		FloorValue:             appliedFloorValue,
		Activities: []observability.RetrievalActivity{{
			ID:              activityID,
			Query:           query,
			Hits:            len(retrieved.Hits),
			FloorDroppedAll: floorDroppedAll,
		}},
	}
	if trace.QueryNormalized == "" {
		trace.QueryNormalized = knowledge.NormalizeQuery(query)
	}
	evidences, evidenceErr := evidencesFromRetrievalHits(hitItems, trace.QueryNormalized)
	trace.HitItems = projectEvidenceTraceHits(evidences, hitItems)
	trace.References = retrievalReferencesFromHits(hitItems, activityID)
	if retrieved.Unavailable {
		// Service health is explicitly separate from corpus coverage. Do not
		// stamp no_evidence here: that would send operators to edit corpus data
		// for an MCP network/auth/readiness failure.
	} else if retrieved.Empty || len(retrieved.Hits) == 0 || len(evidences) == 0 || evidenceErr != nil {
		trace.RefusedReason = "no_evidence"
		trace.RankingErrorCandidate = true
	} else {
		if isWeakEvidence(hitItems, retrieved.HybridMode, retrieved.RerankerMode != "") {
			trace.WeakEvidence = true
		}
		if isRankingAmbiguous(hitItems, retrieved.HybridMode) {
			trace.RankingErrorCandidate = true
		}
	}
	e.emitRetrievalTrace(trace)
}

func (e *Engine) emitSearchKnowledgeTurnTrace(citedChunkIDs []string) {
	if len(e.searchKnowledgeHitsThisTurn) == 0 && len(citedChunkIDs) == 0 && e.answerEchoedChunkIDThisTurn == "" {
		return
	}
	query := strings.TrimSpace(e.searchKnowledgeLedgerThisTurn.Query)
	if query == "" {
		query = strings.TrimSpace(e.lastUserMsg)
	}
	queryNormalized := knowledge.NormalizeQuery(query)
	turnHits := retrievalHitsFromLedger(e.searchKnowledgeLedgerThisTurn, e.searchKnowledgeHitsThisTurn)
	evidences, _ := evidencesFromRetrievalHits(turnHits, queryNormalized)
	refs := retrievalReferencesFromLedgerActivities(e.searchKnowledgeLedgerThisTurn, turnHits, e.searchKnowledgeActivityIDsByChunkID, "")
	kbVersion := ""
	if len(e.searchKnowledgeHitsThisTurn) > 0 {
		kbVersion = strings.TrimSpace(e.searchKnowledgeHitsThisTurn[0].Chunk.KBVersion)
	}
	trace := observability.RetrievalTrace{
		Enabled:         true,
		TurnAggregate:   true,
		KBVersion:       kbVersion,
		AnswerQuestion:  query,
		QueryRaw:        query,
		QueryNormalized: queryNormalized,
		Hits:            len(turnHits),
		Activities:      append([]observability.RetrievalActivity(nil), e.searchKnowledgeActivitiesThisTurn...),
		HitItems:        projectEvidenceTraceHits(evidences, turnHits),
		References:      refs,
		CitedChunkIDs:   append([]string(nil), citedChunkIDs...),
		CitedRefs:       citedRefsFromChunkIDs(citedChunkIDs, refs),
		// Telemetry only — see answerEchoedChunkIDThisTurn.
		AnswerEchoedChunkID: e.answerEchoedChunkIDThisTurn,
	}
	e.emitRetrievalTrace(trace)
}

func (e *Engine) emitSearchKnowledgeCitationTrace(report knowledge.GroundedAnswerReport) {
	if !report.Grounded() {
		return
	}
	e.emitSearchKnowledgeTurnTrace(report.CitedChunkIDs)
}

// retrievalHitsFromLedger projects the exact de-duplicated evidence set that was
// available to the answer verifier. A chunk may be returned by several searches;
// the reference records retain every activity ID without duplicating the chunk.
func retrievalHitsFromLedger(ledger knowledge.EvidenceLedger, hits []knowledge.RetrievalHit) []knowledge.RetrievalHit {
	if len(ledger.Items) == 0 {
		return nil
	}
	byChunkID := make(map[string]knowledge.RetrievalHit, len(hits))
	for _, hit := range hits {
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID != "" {
			byChunkID[chunkID] = hit
		}
	}
	out := make([]knowledge.RetrievalHit, 0, len(ledger.Items))
	for _, item := range ledger.Items {
		if hit, ok := byChunkID[strings.TrimSpace(item.ChunkID)]; ok {
			out = append(out, hit)
		}
	}
	return out
}

func searchKnowledgeArg(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// searchKnowledgeResultJSON renders every SearchKnowledge observation through
// one serializer. meta carries only exceptional state such as a remote outage.
func searchKnowledgeResultJSON(ledger knowledge.EvidenceLedger, followUp string, meta map[string]any) string {
	result := map[string]any{"EvidenceLedger": ledger}
	if len(ledger.Items) == 0 {
		result["empty"] = true
	}
	if followUp != "" {
		result["follow_up"] = followUp
	}
	for key, value := range meta {
		result[key] = value
	}
	b, err := json.Marshal(result)
	if err != nil {
		return `{"EvidenceLedger":{"items":[]},"empty":true}`
	}
	return string(b)
}
