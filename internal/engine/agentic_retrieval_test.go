package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/knowledge/agentic"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticAgenticRetriever struct {
	mu            sync.Mutex
	results       map[string]knowledge.RetrievalResult
	calls         []knowledgeRetrievalCall
	reranked      []knowledge.RetrievalHit
	rerankReason  string
	rerankLatency int64
	delay         time.Duration
	rerankDelay   time.Duration
}

func (r *staticAgenticRetriever) Retrieve(question, productArea string) knowledge.RetrievalResult {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, knowledgeRetrievalCall{question: question, productArea: productArea})
	result, ok := r.results[question]
	if !ok {
		return knowledge.RetrievalResult{Enabled: true, Empty: true}
	}
	if len(result.HitItems) == 0 && len(result.Hits) > 0 {
		result.HitItems = make([]knowledge.RetrievalHit, 0, len(result.Hits))
		for _, chunk := range result.Hits {
			result.HitItems = append(result.HitItems, knowledge.RetrievalHit{Chunk: chunk, Score: 80, Kept: true})
		}
	}
	return result
}

func TestAgenticSearchKnowledgeRepeatedCallsUseTurnScopedRefIDs(t *testing.T) {
	chunkA := agenticTestChunk("chunk-a", "linux_ops", "A", "A")
	chunkB := agenticTestChunk("chunk-b", "linux_ops", "B", "B")
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		agenticRetrievalResult(chunkA, 91),
		agenticRetrievalResult(chunkB, 92),
	}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.lastUserMsg = "怎么部署"

	first := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "q1"}, noopStep)
	second := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "q2"}, noopStep)

	assert.Contains(t, first, `"ref_id":"1"`)
	assert.Contains(t, second, `"ref_id":"2"`)
	require.Len(t, eng.searchKnowledgeReferenceLedgerThisTurn.References, 2)
	assert.Equal(t, "1", eng.searchKnowledgeReferenceLedgerThisTurn.References[0].RefID)
	assert.Equal(t, "chunk-a", eng.searchKnowledgeReferenceLedgerThisTurn.References[0].ChunkID)
	assert.Equal(t, "2", eng.searchKnowledgeReferenceLedgerThisTurn.References[1].RefID)
	assert.Equal(t, "chunk-b", eng.searchKnowledgeReferenceLedgerThisTurn.References[1].ChunkID)
	ledger := eng.currentSearchKnowledgeCitationLedger("q")
	require.Len(t, ledger.Items, 2)
	assert.Equal(t, "1", ledger.Items[0].RefID)
	assert.Equal(t, "2", ledger.Items[1].RefID)
}

func TestAgenticKnowledgeRetrievalHonorsParentTimeoutBudget(t *testing.T) {
	chunk := agenticTestChunk("slow-chunk", "linux_ops", "slow", "slow")
	retriever := &staticAgenticRetriever{
		results: map[string]knowledge.RetrievalResult{"slow query": agenticRetrievalResult(chunk, 90)},
		delay:   200 * time.Millisecond,
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, hits, ledger := eng.runAgenticKnowledgeRetrieval(ctx, "slow query", "slow query", "")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 150*time.Millisecond)
	assert.Empty(t, hits)
	assert.Empty(t, ledger.Items)
	require.Len(t, result.Activities, 1)
	assert.Equal(t, "retrieval_timeout", result.Activities[0].Error)
	assert.Equal(t, "retrieval_timeout", result.FusionRerankerFallbackReason)
}

func (r *staticAgenticRetriever) RerankHitItems(_ string, hits []knowledge.RetrievalHit, _ int) ([]knowledge.RetrievalHit, string, int64) {
	if r.rerankDelay > 0 {
		time.Sleep(r.rerankDelay)
	}
	if r.rerankReason != "" {
		return nil, r.rerankReason, r.rerankLatency
	}
	if len(r.reranked) > 0 {
		return r.reranked, "", r.rerankLatency
	}
	return hits, "", r.rerankLatency
}

func TestAgenticKnowledgeRetrieval_GlobalRerankHonorsTimeoutBudget(t *testing.T) {
	chunk := agenticTestChunk("chunk-a", "linux_ops", "A", "A")
	retriever := &staticAgenticRetriever{
		results:     map[string]knowledge.RetrievalResult{"排序测试": agenticRetrievalResult(chunk, 90)},
		rerankDelay: 200 * time.Millisecond,
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, hits, ledger := eng.runAgenticKnowledgeRetrieval(ctx, "排序测试", "排序测试", "")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 150*time.Millisecond)
	require.Len(t, hits, 1)
	require.Len(t, ledger.Items, 1)
	assert.Equal(t, "reranker_timeout", result.FusionRerankerFallbackReason)
}

func agenticTestChunk(id, area, title, content string) knowledge.KBChunk {
	return knowledge.KBChunk{
		ChunkID:     id,
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: area,
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       title,
		Content:     content,
		SourceURL:   "https://example.test/" + id,
	}
}

func agenticRetrievalResult(chunk knowledge.KBChunk, score float64) knowledge.RetrievalResult {
	return knowledge.RetrievalResult{
		Enabled:   true,
		KBVersion: chunk.KBVersion,
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: score, Kept: true}},
	}
}

func TestAgenticKnowledgeRetrieval_SingleQueryBuildsReferenceLedger(t *testing.T) {
	chunk := agenticTestChunk("w0-ops-deploy-entry", "linux_ops", "点击部署入口", "点击部署在控制台实例详情页。")
	retriever := &staticAgenticRetriever{results: map[string]knowledge.RetrievalResult{
		"点击部署在哪": agenticRetrievalResult(chunk, 91),
	}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	result, hits, ledger := eng.runAgenticKnowledgeRetrieval(context.Background(), "点击部署在哪", "点击部署在哪", "")

	require.Len(t, result.QueryPlan.Subqueries, 1)
	require.Len(t, result.Activities, 1)
	assert.Equal(t, "点击部署在哪", result.Activities[0].Query)
	assert.Equal(t, 1, result.Activities[0].Hits)
	assert.Equal(t, 1, result.Activities[0].KeptHits)
	assert.Equal(t, []string{"w0-ops-deploy-entry"}, result.Activities[0].KeptChunkIDs)
	require.Len(t, result.ReferenceLedger.References, 1)
	assert.Equal(t, "turn_1_based", result.ReferenceLedger.RefIDScheme)
	assert.Equal(t, "1", result.ReferenceLedger.References[0].RefID)
	assert.Equal(t, "w0-ops-deploy-entry", result.ReferenceLedger.References[0].ChunkID)
	require.Len(t, hits, 1)
	require.Len(t, ledger.Items, 1)
	assert.Equal(t, "1", ledger.Items[0].RefID)
	assert.Equal(t, "w0-ops-deploy-entry", ledger.Items[0].ChunkID)
}

func TestAgenticKnowledgeRetrieval_PlannedSubqueriesDeduplicateReferences(t *testing.T) {
	chunk := agenticTestChunk("w0-billing-postpay-second", "billing_rule", "后付费按量计费", "后付费按量按实际使用秒级计费。")
	retriever := &staticAgenticRetriever{results: map[string]knowledge.RetrievalResult{
		"postpay 秒级计费": agenticRetrievalResult(chunk, 93),
		"不到五分钟 按一小时":   agenticRetrievalResult(chunk, 88),
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{"subqueries":[{"query":"postpay 秒级计费","purpose":"billing_rule","product_area_hint":"billing","required":true},{"query":"不到五分钟 按一小时","purpose":"billing_dispute","product_area_hint":"billing","required":true}]}`}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	result, _, ledger := eng.runAgenticKnowledgeRetrieval(context.Background(), "按秒计费 为什么不到5分钟按1小时收费", "按秒计费 为什么不到5分钟按1小时收费", "")

	require.Len(t, result.Activities, 2)
	require.Len(t, retriever.calls, 2)
	assert.ElementsMatch(t, []string{"postpay 秒级计费", "不到五分钟 按一小时"}, []string{retriever.calls[0].question, retriever.calls[1].question})
	require.Len(t, result.ReferenceLedger.References, 1)
	ref := result.ReferenceLedger.References[0]
	assert.Equal(t, "1", ref.RefID)
	assert.Equal(t, "w0-billing-postpay-second", ref.ChunkID)
	assert.ElementsMatch(t, []string{"act-1", "act-2"}, ref.ActivityIDs)
	require.Len(t, ledger.Items, 1)
	assert.Equal(t, "1", ledger.Items[0].RefID)
}

func TestAgenticKnowledgeRetrieval_GlobalRerankAndFallback(t *testing.T) {
	chunkA := agenticTestChunk("chunk-a", "linux_ops", "A", "A")
	chunkB := agenticTestChunk("chunk-b", "linux_ops", "B", "B")
	base := knowledge.RetrievalResult{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunkA, chunkB},
		HitItems: []knowledge.RetrievalHit{
			{Chunk: chunkA, Score: 90, Kept: true},
			{Chunk: chunkB, Score: 80, Kept: true},
		},
	}

	t.Run("reranker order wins", func(t *testing.T) {
		retriever := &staticAgenticRetriever{
			results: map[string]knowledge.RetrievalResult{"排序测试": base},
			reranked: []knowledge.RetrievalHit{
				{Chunk: chunkB, Score: 80, Kept: true},
				{Chunk: chunkA, Score: 90, Kept: true},
			},
			rerankLatency: 12,
		}
		eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
		eng.SetKnowledgeRetriever(retriever)

		result, _, _ := eng.runAgenticKnowledgeRetrieval(context.Background(), "排序测试", "排序测试", "")

		require.Len(t, result.ReferenceLedger.References, 2)
		assert.Equal(t, "chunk-b", result.ReferenceLedger.References[0].ChunkID)
		assert.Empty(t, result.FusionRerankerFallbackReason)
		assert.Equal(t, int64(12), result.FusionRerankerLatencyMS)
	})

	t.Run("fallback reason recorded", func(t *testing.T) {
		retriever := &staticAgenticRetriever{
			results:       map[string]knowledge.RetrievalResult{"排序测试": base},
			rerankReason:  "reranker_timeout",
			rerankLatency: 34,
		}
		eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
		eng.SetKnowledgeRetriever(retriever)

		result, _, _ := eng.runAgenticKnowledgeRetrieval(context.Background(), "排序测试", "排序测试", "")

		require.Len(t, result.ReferenceLedger.References, 2)
		assert.Equal(t, "chunk-a", result.ReferenceLedger.References[0].ChunkID)
		assert.Equal(t, "reranker_timeout", result.FusionRerankerFallbackReason)
		assert.Equal(t, int64(34), result.FusionRerankerLatencyMS)
	})
}

func TestSynthesizeKnowledgeQAFromLedger_RetryRecordsCitedRefs(t *testing.T) {
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: agenticTestChunk("w0-linux-upload", "linux_ops", "上传文件", "可以通过控制台上传文件。")}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "可以通过控制台上传文件。"},
		{Content: "可以通过控制台上传文件 [1]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeReferenceLedgerThisTurn = agentic.ReferenceLedger{
		RefIDScheme: agenticRefIDScheme,
		References:  []agentic.Reference{{RefID: "1", ChunkID: "w0-linux-upload", SourceArea: "linux_ops"}},
	}
	var traces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		traces = append(traces, trace)
	})

	result, ok := eng.synthesizeKnowledgeQAFromLedgerDetailed(context.Background(), "怎么上传文件")

	require.True(t, ok)
	assert.Equal(t, "可以通过控制台上传文件。", result.display)
	assert.Equal(t, []string{"1"}, result.citedRefs)
	assert.Equal(t, []string{"w0-linux-upload"}, result.citedChunkIDs)
	require.Len(t, traces, 1)
	assert.Equal(t, []string{"1"}, traces[0].CitedRefs)
	assert.Equal(t, []string{"w0-linux-upload"}, traces[0].CitedChunkIDs)
}

func TestSynthesizeKnowledgeQAFromLedger_UsesReferenceLedgerOrder(t *testing.T) {
	chunkA := agenticTestChunk("chunk-a", "linux_ops", "A title", "A content")
	chunkB := agenticTestChunk("chunk-b", "linux_ops", "B title", "B content")
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "A [1]，B [2]。"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{
		{Kept: true, Score: 90, Chunk: chunkB},
		{Kept: true, Score: 95, Chunk: chunkA},
		{Kept: true, Score: 88, Chunk: chunkB},
	}
	eng.searchKnowledgeReferenceLedgerThisTurn = agentic.ReferenceLedger{
		RefIDScheme: agenticRefIDScheme,
		References: []agentic.Reference{
			{RefID: "1", ChunkID: "chunk-a", SourceArea: "linux_ops"},
			{RefID: "2", ChunkID: "chunk-b", SourceArea: "linux_ops"},
		},
	}

	result, ok := eng.synthesizeKnowledgeQAFromLedgerDetailed(context.Background(), "怎么处理")

	require.True(t, ok)
	assert.Equal(t, []string{"chunk-a", "chunk-b"}, result.citedChunkIDs)
	require.Len(t, mock.calls, 1)
	userPrompt := mock.calls[0].Messages[len(mock.calls[0].Messages)-1].Content
	assert.Contains(t, userPrompt, "[1] A title\nA content")
	assert.Contains(t, userPrompt, "[2] B title\nB content")
	assert.Less(t, strings.Index(userPrompt, "[1] A title"), strings.Index(userPrompt, "[2] B title"))
}

func TestSynthesizeKnowledgeQAFromLedger_BillingRequiresBillingReference(t *testing.T) {
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: agenticTestChunk("w0-linux-general", "linux_ops", "通用说明", "这不是计费规则。")}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "后付费不足 1 小时按 1 小时收费 [1]。"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeReferenceLedgerThisTurn = agentic.ReferenceLedger{
		RefIDScheme: agenticRefIDScheme,
		References:  []agentic.Reference{{RefID: "1", ChunkID: "w0-linux-general", SourceArea: "linux_ops"}},
	}

	result, ok := eng.synthesizeKnowledgeQAFromLedgerDetailed(context.Background(), "后付费按量不到5分钟为什么按1小时收费")

	require.True(t, ok)
	assert.Equal(t, ragNoEvidenceReply, result.display)
	assert.Empty(t, mock.calls, "high-risk billing answers must not be synthesized from non-billing references")
}

func TestGuardSearchKnowledgeSynthesis_BillingRequiresBillingReferenceOnFreePath(t *testing.T) {
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: agenticTestChunk("w0-linux-general", "linux_ops", "通用说明", "这不是规则来源。")}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.searchKnowledgeRanThisTurn = true
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.lastUserMsg = "后付费按量不到5分钟为什么按1小时收费"
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeReferenceLedgerThisTurn = agentic.ReferenceLedger{
		RefIDScheme: agenticRefIDScheme,
		References:  []agentic.Reference{{RefID: "1", ChunkID: "w0-linux-general", SourceArea: "linux_ops"}},
	}

	got := eng.guardSearchKnowledgeSynthesis("后付费不足 1 小时按 1 小时收费 [1]。")

	assert.Equal(t, ragNoEvidenceReply, got)
}

func TestGuardSearchKnowledgeSynthesis_BillingBlocksWhenSearchKnowledgeNotCalled(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.lastUserMsg = "按秒计费 为什么不到5分钟却按1小时收费"

	got := eng.guardSearchKnowledgeSynthesis("后付费不足 1 小时按 1 小时收费。")

	assert.Equal(t, ragNoEvidenceReply, got)
}

func TestGuardSearchKnowledgeSynthesis_PriceQuestionBlocksWhenSearchKnowledgeNotCalled(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.lastUserMsg = "4090 多少钱"

	got := eng.guardSearchKnowledgeSynthesis("4090 每小时 1 元。")

	assert.Equal(t, ragNoEvidenceReply, got)
}

func TestGuardSearchKnowledgeSynthesis_BillingBlocksMixedRefusalAndWrongRule(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.lastUserMsg = "按秒计费 为什么不到5分钟却按1小时收费"

	got := eng.guardSearchKnowledgeSynthesis("知识库未覆盖这个问题，但后付费不足 1 小时仍按 1 小时收费。")

	assert.Equal(t, ragNoEvidenceReply, got)
}

func TestGuardSearchKnowledgeSynthesis_BillingAllowsCanonicalNoEvidenceReply(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.lastUserMsg = "按秒计费 为什么不到5分钟却按1小时收费"

	got := eng.guardSearchKnowledgeSynthesis(ragNoEvidenceReply)

	assert.Equal(t, ragNoEvidenceReply, got)
}
