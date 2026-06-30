package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/knowledge/agentic"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
)

const (
	agenticRetrievalMaxSubqueries = 5
	agenticRetrievalMaxReferences = knowledge.DefaultEvidenceLedgerMaxItems
	agenticRetrievalTimeout       = 8 * time.Second
	agenticRefIDScheme            = "turn_1_based"
)

type rerankableKnowledgeRetriever interface {
	RerankHitItems(question string, hits []knowledge.RetrievalHit, topK int) ([]knowledge.RetrievalHit, string, int64)
}

type agenticRetrievalHit struct {
	hit        knowledge.RetrievalHit
	activityID string
	rank       int
}

type referenceCandidate struct {
	hit         knowledge.RetrievalHit
	activityIDs []string
	score       float64
	bestRank    int
}

type agenticSubqueryResult struct {
	index    int
	activity agentic.RetrievalActivity
	hits     []agenticRetrievalHit
	rawHits  []knowledge.RetrievalHit
}

func (e *Engine) runAgenticKnowledgeRetrieval(ctx context.Context, userMsg, query, hint string) (agentic.AgenticRetrievalResult, []knowledge.RetrievalHit, knowledge.EvidenceLedger) {
	plan := e.planAgenticRetrieval(ctx, userMsg, query, hint)
	if len(plan.Subqueries) == 0 {
		plan = fallbackAgenticQueryPlan(query, hint)
	}
	if len(plan.Subqueries) > agenticRetrievalMaxSubqueries {
		plan.Subqueries = plan.Subqueries[:agenticRetrievalMaxSubqueries]
	}

	ctx, cancel := context.WithTimeout(ctx, agenticRetrievalTimeout)
	defer cancel()

	resultCh := make(chan agenticSubqueryResult, len(plan.Subqueries))
	for i, sq := range plan.Subqueries {
		i, sq := i, sq
		go func() {
			resultCh <- e.retrieveAgenticSubquery(ctx, i, sq)
		}()
	}

	results := make([]agenticSubqueryResult, 0, len(plan.Subqueries))
	seen := map[int]struct{}{}
	for len(results) < len(plan.Subqueries) {
		select {
		case r := <-resultCh:
			results = append(results, r)
			seen[r.index] = struct{}{}
		case <-ctx.Done():
			for i, sq := range plan.Subqueries {
				if _, ok := seen[i]; ok {
					continue
				}
				results = append(results, timeoutAgenticSubqueryResult(i, sq))
			}
			sort.SliceStable(results, func(i, j int) bool { return results[i].index < results[j].index })
			return e.finishAgenticKnowledgeRetrieval(ctx, query, plan, results, "retrieval_timeout", 0)
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].index < results[j].index })
	return e.finishAgenticKnowledgeRetrieval(ctx, query, plan, results, "", 0)
}

func (e *Engine) finishAgenticKnowledgeRetrieval(ctx context.Context, query string, plan agentic.QueryPlan, results []agenticSubqueryResult, earlyFallbackReason string, earlyLatencyMS int64) (agentic.AgenticRetrievalResult, []knowledge.RetrievalHit, knowledge.EvidenceLedger) {
	activities := make([]agentic.RetrievalActivity, 0, len(results))
	flatHits := make([]agenticRetrievalHit, 0)
	rawChunkIDs := []string{}
	for _, r := range results {
		if r.activity.ID != "" {
			activities = append(activities, r.activity)
		}
		flatHits = append(flatHits, r.hits...)
		for _, h := range r.rawHits {
			rawChunkIDs = appendUniqueString(rawChunkIDs, strings.TrimSpace(h.Chunk.ChunkID))
		}
	}

	finalHits, fusionRerankerFallbackReason, fusionRerankerLatencyMS := e.fuseAgenticHits(ctx, query, flatHits)
	if fusionRerankerFallbackReason == "" && earlyFallbackReason != "" {
		fusionRerankerFallbackReason = earlyFallbackReason
		fusionRerankerLatencyMS = earlyLatencyMS
	}
	references := agenticReferencesFromHits(finalHits, flatHits)
	ledger := evidenceLedgerFromReferences(query, references, finalHits)
	result := agentic.AgenticRetrievalResult{
		QueryPlan:  plan,
		Activities: activities,
		ReferenceLedger: agentic.ReferenceLedger{
			RefIDScheme: agenticRefIDScheme,
			References:  references,
		},
		RetrievedChunkIDs:            rawChunkIDs,
		FusionRerankerFallbackReason: fusionRerankerFallbackReason,
		FusionRerankerLatencyMS:      fusionRerankerLatencyMS,
	}
	return result, finalHits, ledger
}

func timeoutAgenticSubqueryResult(index int, sq agentic.Subquery) agenticSubqueryResult {
	return agenticSubqueryResult{
		index: index,
		activity: agentic.RetrievalActivity{
			ID:              fmt.Sprintf("act-%d", index+1),
			SubqueryID:      strings.TrimSpace(sq.ID),
			Query:           strings.TrimSpace(sq.Query),
			Purpose:         strings.TrimSpace(sq.Purpose),
			ProductAreaHint: strings.TrimSpace(sq.ProductAreaHint),
			Required:        sq.Required,
			Error:           "retrieval_timeout",
		},
	}
}

func (e *Engine) retrieveAgenticSubquery(ctx context.Context, index int, sq agentic.Subquery) agenticSubqueryResult {
	activityID := fmt.Sprintf("act-%d", index+1)
	query := strings.TrimSpace(sq.Query)
	activity := agentic.RetrievalActivity{
		ID:              activityID,
		SubqueryID:      strings.TrimSpace(sq.ID),
		Query:           query,
		Purpose:         strings.TrimSpace(sq.Purpose),
		ProductAreaHint: strings.TrimSpace(sq.ProductAreaHint),
		Required:        sq.Required,
	}
	if e.knowledgeRetriever == nil || query == "" {
		activity.Error = "retriever_unavailable"
		return agenticSubqueryResult{index: index, activity: activity}
	}
	select {
	case <-ctx.Done():
		activity.Error = "retrieval_timeout"
		return agenticSubqueryResult{index: index, activity: activity}
	default:
	}
	start := time.Now()
	areaText := query
	if activity.ProductAreaHint != "" {
		areaText = activity.ProductAreaHint + " " + query
	}
	retrieved := e.knowledgeRetriever.Retrieve(query, inferKnowledgeProductArea(areaText))
	activity.LatencyMS = time.Since(start).Milliseconds()
	activity.HybridMode = retrieved.HybridMode
	activity.HybridFallbackReason = retrieved.HybridFallbackReason
	activity.RerankerMode = retrieved.RerankerMode
	activity.RerankerFallbackReason = retrieved.RerankerFallbackReason

	rawHits := retrieved.HitItems
	if len(rawHits) == 0 && len(retrieved.Hits) > 0 {
		rawHits = make([]knowledge.RetrievalHit, 0, len(retrieved.Hits))
		for _, chunk := range retrieved.Hits {
			rawHits = append(rawHits, knowledge.RetrievalHit{Chunk: chunk, Score: 80, Kept: true})
		}
	}
	activity.Hits = len(rawHits)
	for _, hit := range rawHits {
		activity.RetrievedChunkIDs = appendUniqueString(activity.RetrievedChunkIDs, strings.TrimSpace(hit.Chunk.ChunkID))
	}
	hits := rawHits
	if isWeakEvidence(rawHits, retrieved.HybridMode) {
		hits = nil
		activity.FloorDroppedAll = len(rawHits) > 0
	}
	outHits := make([]agenticRetrievalHit, 0, len(hits))
	for rank, hit := range hits {
		if !hit.Kept {
			continue
		}
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		activity.KeptChunkIDs = appendUniqueString(activity.KeptChunkIDs, chunkID)
		outHits = append(outHits, agenticRetrievalHit{hit: hit, activityID: activityID, rank: rank + 1})
	}
	activity.KeptHits = len(outHits)
	if retrieved.Empty || len(rawHits) == 0 {
		activity.Error = "no_evidence"
	}
	return agenticSubqueryResult{index: index, activity: activity, hits: outHits, rawHits: rawHits}
}

func (e *Engine) planAgenticRetrieval(ctx context.Context, userMsg, query, hint string) agentic.QueryPlan {
	fallback := fallbackAgenticQueryPlan(query, hint)
	if e.llmClient == nil {
		return fallback
	}
	system := "你是优云智算知识库检索规划器。请把用户问题拆成 1-5 个可并行检索的子查询。只输出 JSON，格式为 {\"subqueries\":[{\"query\":\"...\",\"purpose\":\"...\",\"product_area_hint\":\"billing|pricing|stock|diagnosis|ops|platform|\",\"required\":true}]}。不要输出解释。"
	user := "用户问题：\n" + strings.TrimSpace(userMsg) + "\n\n初始检索词：\n" + strings.TrimSpace(query)
	if strings.TrimSpace(hint) != "" {
		user += "\n\n领域提示：\n" + strings.TrimSpace(hint)
	}
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: system},
			{Role: openai.ChatMessageRoleUser, Content: user},
		},
		ResponseFormat: agenticQueryPlanResponseFormat(),
	})
	if err != nil || resp == nil {
		return fallback
	}
	e.emitTokenUsage(resp.Usage)
	plan, ok := parseAgenticQueryPlan(resp.Content, query, hint)
	if !ok {
		return fallback
	}
	return plan
}

func agenticQueryPlanResponseFormat() *openai.ChatCompletionResponseFormat {
	return &openai.ChatCompletionResponseFormat{
		Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
			Name:   "agentic_retrieval_plan",
			Strict: false,
			Schema: json.RawMessage(`{
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"subqueries":{
						"type":"array",
						"minItems":1,
						"maxItems":5,
						"items":{
							"type":"object",
							"additionalProperties":false,
							"properties":{
								"id":{"type":"string"},
								"query":{"type":"string"},
								"purpose":{"type":"string"},
								"product_area_hint":{"type":"string"},
								"required":{"type":"boolean"}
							},
							"required":["query","required"]
						}
					}
				},
				"required":["subqueries"]
			}`),
		},
	}
}

func parseAgenticQueryPlan(content, fallbackQuery, fallbackHint string) (agentic.QueryPlan, bool) {
	var plan agentic.QueryPlan
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &plan); err != nil {
		return agentic.QueryPlan{}, false
	}
	plan = normalizeAgenticQueryPlan(plan, fallbackQuery, fallbackHint)
	return plan, len(plan.Subqueries) > 0
}

func normalizeAgenticQueryPlan(plan agentic.QueryPlan, fallbackQuery, fallbackHint string) agentic.QueryPlan {
	out := agentic.QueryPlan{Subqueries: []agentic.Subquery{}}
	seen := map[string]struct{}{}
	for _, sq := range plan.Subqueries {
		query := strings.TrimSpace(sq.Query)
		if query == "" {
			continue
		}
		key := strings.ToLower(query)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if len(out.Subqueries) >= agenticRetrievalMaxSubqueries {
			break
		}
		id := strings.TrimSpace(sq.ID)
		if id == "" {
			id = fmt.Sprintf("q%d", len(out.Subqueries)+1)
		}
		hint := strings.TrimSpace(sq.ProductAreaHint)
		if hint == "" {
			hint = strings.TrimSpace(fallbackHint)
		}
		out.Subqueries = append(out.Subqueries, agentic.Subquery{
			ID:              id,
			Query:           query,
			Purpose:         strings.TrimSpace(sq.Purpose),
			ProductAreaHint: hint,
			Required:        sq.Required || len(out.Subqueries) == 0,
		})
	}
	if len(out.Subqueries) == 0 && strings.TrimSpace(fallbackQuery) != "" {
		return fallbackAgenticQueryPlan(fallbackQuery, fallbackHint)
	}
	return out
}

func fallbackAgenticQueryPlan(query, hint string) agentic.QueryPlan {
	query = strings.TrimSpace(query)
	if query == "" {
		query = strings.TrimSpace(hint)
	}
	if query == "" {
		return agentic.QueryPlan{}
	}
	return agentic.QueryPlan{Subqueries: []agentic.Subquery{{
		ID:              "q1",
		Query:           query,
		Purpose:         "answer_user_question",
		ProductAreaHint: strings.TrimSpace(hint),
		Required:        true,
	}}}
}

type agenticRerankResult struct {
	hits      []knowledge.RetrievalHit
	reason    string
	latencyMS int64
}

func (e *Engine) fuseAgenticHits(ctx context.Context, query string, hits []agenticRetrievalHit) ([]knowledge.RetrievalHit, string, int64) {
	if len(hits) == 0 {
		return nil, "", 0
	}
	candidates := map[string]*referenceCandidate{}
	for _, h := range hits {
		chunkID := strings.TrimSpace(h.hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		contribution := h.hit.Score + 1/float64(h.rank+1)
		c, ok := candidates[chunkID]
		if !ok {
			candidates[chunkID] = &referenceCandidate{
				hit:         h.hit,
				activityIDs: []string{h.activityID},
				score:       contribution,
				bestRank:    h.rank,
			}
			continue
		}
		c.score += contribution
		c.activityIDs = appendUniqueString(c.activityIDs, h.activityID)
		if h.rank < c.bestRank || h.hit.Score > c.hit.Score {
			c.hit = h.hit
			c.bestRank = h.rank
		}
	}
	ordered := make([]*referenceCandidate, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, c)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score != ordered[j].score {
			return ordered[i].score > ordered[j].score
		}
		if ordered[i].bestRank != ordered[j].bestRank {
			return ordered[i].bestRank < ordered[j].bestRank
		}
		return ordered[i].hit.Chunk.ChunkID < ordered[j].hit.Chunk.ChunkID
	})
	fused := make([]knowledge.RetrievalHit, 0, len(ordered))
	for _, c := range ordered {
		fused = append(fused, c.hit)
	}
	truncate := func(items []knowledge.RetrievalHit) []knowledge.RetrievalHit {
		if len(items) > agenticRetrievalMaxReferences {
			return items[:agenticRetrievalMaxReferences]
		}
		return items
	}
	if rr, ok := e.knowledgeRetriever.(rerankableKnowledgeRetriever); ok {
		if ctx.Err() != nil {
			return truncate(fused), "reranker_timeout", 0
		}
		rerankCh := make(chan agenticRerankResult, 1)
		go func() {
			reranked, reason, latency := rr.RerankHitItems(query, fused, agenticRetrievalMaxReferences)
			rerankCh <- agenticRerankResult{hits: reranked, reason: reason, latencyMS: latency}
		}()
		var reranked []knowledge.RetrievalHit
		var reason string
		var latency int64
		select {
		case result := <-rerankCh:
			reranked, reason, latency = result.hits, result.reason, result.latencyMS
		case <-ctx.Done():
			return truncate(fused), "reranker_timeout", 0
		}
		if reason == "" && len(reranked) > 0 {
			fused = reranked
			return truncate(fused), "", latency
		}
		if reason != "" {
			return truncate(fused), reason, latency
		}
	}
	return truncate(fused), "", 0
}

func agenticReferencesFromHits(finalHits []knowledge.RetrievalHit, allHits []agenticRetrievalHit) []agentic.Reference {
	activityByChunk := map[string][]string{}
	for _, h := range allHits {
		chunkID := strings.TrimSpace(h.hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		activityByChunk[chunkID] = appendUniqueString(activityByChunk[chunkID], h.activityID)
	}
	refs := make([]agentic.Reference, 0, len(finalHits))
	for i, hit := range finalHits {
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		sourceArea := strings.TrimSpace(hit.Chunk.ProductArea)
		if sourceArea == "" {
			sourceArea = inferKnowledgeProductArea(hit.Chunk.Title + " " + hit.Chunk.SourceURL + " " + hit.Chunk.Content)
		}
		refs = append(refs, agentic.Reference{
			RefID:       strconv.Itoa(len(refs) + 1),
			ChunkID:     chunkID,
			Title:       strings.TrimSpace(hit.Chunk.Title),
			SourceURL:   strings.TrimSpace(hit.Chunk.SourceURL),
			Score:       hit.Score,
			SourceArea:  sourceArea,
			ActivityIDs: activityByChunk[chunkID],
			Rank:        i + 1,
		})
	}
	return refs
}

func evidenceLedgerFromReferences(query string, refs []agentic.Reference, hits []knowledge.RetrievalHit) knowledge.EvidenceLedger {
	ledger := knowledge.BuildSubstantiveEvidenceLedger(query, hits, len(hits), 0)
	refByChunk := map[string]string{}
	for _, ref := range refs {
		refByChunk[ref.ChunkID] = ref.RefID
	}
	for i := range ledger.Items {
		ledger.Items[i].RefID = refByChunk[ledger.Items[i].ChunkID]
	}
	return ledger
}

func (e *Engine) emitAgenticKnowledgeRetrievalTrace(query string, result agentic.AgenticRetrievalResult, hits []knowledge.RetrievalHit) {
	hybridMode, hybridFallback, rerankerMode, rerankerFallback := agenticTraceModes(result.Activities)
	trace := observability.RetrievalTrace{
		Enabled:                true,
		QueryRaw:               strings.TrimSpace(query),
		QueryPlan:              &result.QueryPlan,
		Activities:             result.Activities,
		References:             result.ReferenceLedger.References,
		RefIDScheme:            result.ReferenceLedger.RefIDScheme,
		Hits:                   len(result.ReferenceLedger.References),
		QueryExpansions:        agenticQueryExpansions(result.QueryPlan, query),
		HybridMode:             hybridMode,
		HybridFallbackReason:   hybridFallback,
		RerankerMode:           rerankerMode,
		RerankerFallbackReason: rerankerFallback,
		FloorValue:             weakEvidenceThresholdFor(hybridMode),
	}
	if len(hits) > 0 {
		if result.FusionRerankerFallbackReason != "" {
			trace.RerankerFallbackReason = result.FusionRerankerFallbackReason
		}
		if result.FusionRerankerLatencyMS > 0 {
			latency := result.FusionRerankerLatencyMS
			trace.RerankerLatencyMS = &latency
		}
		trace.KBVersion = hits[0].Chunk.KBVersion
		trace.QueryNormalized = knowledge.NormalizeQuery(query)
		evidences, _ := evidencesFromRetrievalHits(hits, trace.QueryNormalized)
		trace.HitItems = projectEvidenceTraceHits(evidences, hits)
		if isWeakEvidence(hits, trace.HybridMode) {
			trace.WeakEvidence = true
		}
		if isRankingAmbiguous(hits, trace.HybridMode) {
			trace.RankingErrorCandidate = true
		}
	} else {
		trace.RefusedReason = "no_evidence"
		trace.RankingErrorCandidate = true
	}
	allOff, inferEmpty := allCitedOffDomain(inferKnowledgeProductArea(query), ledgerReferenceAreas(result.ReferenceLedger))
	trace.DomainInferenceEmpty = inferEmpty
	trace.AllCitedOffDomain = allOff
	if domainMatchGuardOn && allOff && trace.RefusedReason == "" {
		trace.RefusedReason = "wrong_domain"
	}
	e.emitRetrievalTrace(trace)
}

func agenticTraceModes(activities []agentic.RetrievalActivity) (hybridMode, hybridFallback, rerankerMode, rerankerFallback string) {
	for _, a := range activities {
		if hybridMode == "" {
			hybridMode = strings.TrimSpace(a.HybridMode)
		}
		if hybridFallback == "" {
			hybridFallback = strings.TrimSpace(a.HybridFallbackReason)
		}
		if rerankerMode == "" {
			rerankerMode = strings.TrimSpace(a.RerankerMode)
		}
		if rerankerFallback == "" {
			rerankerFallback = strings.TrimSpace(a.RerankerFallbackReason)
		}
	}
	return hybridMode, hybridFallback, rerankerMode, rerankerFallback
}

func agenticQueryExpansions(plan agentic.QueryPlan, original string) []string {
	out := []string{}
	orig := strings.TrimSpace(original)
	for _, sq := range plan.Subqueries {
		q := strings.TrimSpace(sq.Query)
		if q == "" || q == orig {
			continue
		}
		out = appendUniqueString(out, q)
	}
	return out
}

func ledgerReferenceAreas(ledger agentic.ReferenceLedger) []string {
	areas := make([]string, 0, len(ledger.References))
	for _, ref := range ledger.References {
		areas = append(areas, strings.TrimSpace(ref.SourceArea))
	}
	return areas
}

func appendUniqueString(out []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return out
	}
	for _, existing := range out {
		if existing == value {
			return out
		}
	}
	return append(out, value)
}
