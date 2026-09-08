package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestSearchKnowledgeCacheRetriesSameQueryAfterFailureThenReusesSuccess(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		{Enabled: true, Unavailable: true, FailureReason: "mcp_timeout"},
		{Enabled: true, HybridMode: knowledge.RetrievalModeUnknownRemote, HitItems: []knowledge.RetrievalHit{
			{Kept: true, Score: 0.9, Chunk: knowledge.KBChunk{ChunkID: "clipboard", Content: "客户端剪贴板说明"}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	call := toolCall("search", "SearchKnowledge", `{"query":"Windows 远程桌面客户端剪贴板"}`)

	first := eng.executeTool(context.Background(), call, noopStep)
	observation, ok := tools.ParseAgentToolResult(agentToolObservation("SearchKnowledge", first))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolStatusFailed, observation.Status)

	second := eng.executeTool(context.Background(), call, noopStep)
	require.NotContains(t, second, "reused_observation")
	require.Contains(t, second, "客户端剪贴板说明")
	require.Len(t, retriever.calls, 2, "the same query must execute after a transient failure")

	third := eng.executeTool(context.Background(), call, noopStep)
	require.Contains(t, third, "reused_observation")
	require.Contains(t, third, "客户端剪贴板说明")
	require.Len(t, retriever.calls, 2, "successful evidence still prevents identical search thrash")
	require.Equal(t, 2, eng.searchKnowledgeCallsThisTurn)
}

func TestSearchKnowledgeCacheRefreshesExpiredCapabilityWithoutEvictingOtherSearches(t *testing.T) {
	for _, weak := range []bool{false, true} {
		name := "citable"
		mode, score := knowledge.RetrievalModeUnknownRemote, 0.9
		if weak {
			name = "below_floor_candidate"
			mode, score = knowledge.RetrievalModeQwen3Full, 0.2
		}
		t.Run(name, func(t *testing.T) {
			targetHits := []knowledge.RetrievalHit{
				{Kept: true, Score: score, Chunk: knowledge.KBChunk{ChunkID: "target", Title: "目标章节", Content: "目标节选"}},
				{Kept: true, Score: score, Chunk: knowledge.KBChunk{ChunkID: "sibling", Title: "关联章节", Content: "关联节选"}},
			}
			retriever := &remoteChunkStoreRetriever{
				scriptedKnowledgeRetriever: scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
					{Enabled: true, SearchID: "expired", HybridMode: mode, HitItems: targetHits},
					{Enabled: true, SearchID: "unrelated", HybridMode: knowledge.RetrievalModeUnknownRemote, HitItems: []knowledge.RetrievalHit{
						{Kept: true, Score: 0.9, Chunk: knowledge.KBChunk{ChunkID: "other", Content: "另一份证据"}},
					}},
					{Enabled: true, SearchID: "refreshed", HybridMode: mode, HitItems: targetHits},
				}},
				chunks: map[string]knowledge.KBChunk{"target": {ChunkID: "target", Content: "目标章节完整正文"}},
			}
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetKnowledgeRetriever(retriever)
			search := toolCall("search", "SearchKnowledge", `{"query":"目标章节"}`)
			other := toolCall("other", "SearchKnowledge", `{"query":"其他章节"}`)
			read := toolCall("read", "ReadChunk", `{"chunk_ids":["target"]}`)
			eng.executeTool(context.Background(), search, noopStep)
			eng.executeTool(context.Background(), other, noopStep)

			retriever.err = knowledge.ErrSearchCapabilityInvalid
			failedRead := eng.executeTool(context.Background(), read, noopStep)
			require.Contains(t, failedRead, `"search_refresh_required":true`)
			require.NotContains(t, eng.searchKnowledgeCapabilitiesThisTurn, "target")
			require.NotContains(t, eng.searchKnowledgeCapabilitiesThisTurn, "sibling", "expiry invalidates the entire search capability")

			retriever.err = nil
			fresh := eng.executeTool(context.Background(), search, noopStep)
			require.NotContains(t, fresh, "reused_observation")
			require.Len(t, retriever.calls, 3, "a same-query refresh must reach the retriever")
			require.Equal(t, "refreshed", eng.searchKnowledgeCapabilitiesThisTurn["target"])
			body := eng.executeTool(context.Background(), read, noopStep)
			require.Contains(t, body, "目标章节完整正文")
			require.Equal(t, "refreshed", retriever.reads[len(retriever.reads)-1].searchID)

			require.Contains(t, eng.executeTool(context.Background(), other, noopStep), "reused_observation")
			require.Contains(t, eng.executeTool(context.Background(), search, noopStep), "reused_observation")
			require.Len(t, retriever.calls, 3, "unrelated and refreshed successful searches remain cached")
		})
	}
}

func TestSearchKnowledgeCacheFailureRetriesStillRespectFourSearchBudget(t *testing.T) {
	results := make([]knowledge.RetrievalResult, maxSearchKnowledgeCallsPerTurn)
	for i := range results {
		results[i] = knowledge.RetrievalResult{Enabled: true, Unavailable: true, FailureReason: "mcp_timeout"}
	}
	retriever := &scriptedKnowledgeRetriever{results: results}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	call := toolCall("search", "SearchKnowledge", `{"query":"暂时不可用的知识查询"}`)
	for range maxSearchKnowledgeCallsPerTurn {
		require.Contains(t, eng.executeTool(context.Background(), call, noopStep), `"knowledge_unavailable":true`)
	}
	require.Len(t, retriever.calls, maxSearchKnowledgeCallsPerTurn)
	require.Equal(t, maxSearchKnowledgeCallsPerTurn, eng.searchKnowledgeCallsThisTurn)
	require.Contains(t, eng.executeTool(context.Background(), call, noopStep), `"search_limit_reached":true`)
	require.Len(t, retriever.calls, maxSearchKnowledgeCallsPerTurn, "cache bypass never bypasses the retrieval budget")
}

func TestSearchKnowledgeCacheRefreshesAfterAutomaticReadCapabilityExpires(t *testing.T) {
	target := knowledge.KBChunk{ChunkID: "privacy", Title: "用户信息", Content: "完整的用户信息收集与使用说明"}
	hits := []knowledge.RetrievalHit{{Kept: true, Score: 0.92, Chunk: target}}
	retriever := &remoteChunkStoreRetriever{
		scriptedKnowledgeRetriever: scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
			{Enabled: true, SearchID: "expired", HybridMode: knowledge.RetrievalModeQwen3RRF, RerankerMode: "qwen3-reranker-8b", HitItems: hits},
			{Enabled: true, SearchID: "fresh", HybridMode: knowledge.RetrievalModeQwen3RRF, RerankerMode: "qwen3-reranker-8b", HitItems: hits},
		}},
		chunks: map[string]knowledge.KBChunk{target.ChunkID: target},
		err:    knowledge.ErrSearchCapabilityInvalid,
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	search := toolCall("search", "SearchKnowledge", `{"query":"我使用过程会被监控吗"}`)

	first := eng.executeTool(context.Background(), search, noopStep)
	require.Contains(t, first, `"auto_expansion_unavailable":true`)
	require.Len(t, retriever.reads, 1, "the failure comes from automatic expansion, not explicit ReadChunk")
	require.NotContains(t, eng.searchKnowledgeCapabilitiesThisTurn, target.ChunkID)
	require.NotContains(t, eng.automaticKnowledgeBodyIDsThisTurn, target.ChunkID)

	retriever.err = nil
	second := eng.executeTool(context.Background(), search, noopStep)
	require.NotContains(t, second, "reused_observation")
	require.Contains(t, second, `"auto_expanded_chunk_ids":["privacy"]`)
	require.Contains(t, second, target.Content)
	require.Len(t, retriever.calls, 2)
	require.Len(t, retriever.reads, 2)
	require.Equal(t, "fresh", retriever.reads[1].searchID)
	require.Equal(t, 2, eng.searchKnowledgeCallsThisTurn)
	require.Zero(t, eng.readChunkCallsThisTurn)

	require.Contains(t, eng.executeTool(context.Background(), search, noopStep), "reused_observation")
	require.Len(t, retriever.calls, 2, "successful refreshed searches are still cached")
	require.Len(t, retriever.reads, 2)
}
