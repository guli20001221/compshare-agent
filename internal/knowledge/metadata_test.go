package knowledge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metadataTestChunks() []KBChunk {
	return []KBChunk{
		{
			ChunkID: "generic", KBVersion: "kb-test", SourceType: sourceTypeFAQ,
			SourceOrigin: sourceOriginOfficial, ProductArea: "resource_purchase",
			ACL: customerSafeACL, Confidence: confidenceHigh, Title: "显卡使用说明",
			ValidFrom: "2025-01-01", Content: "可在控制台选择实例规格。",
		},
		{
			ChunkID: "rtx-4090", KBVersion: "kb-test", SourceType: sourceTypeFAQ,
			SourceOrigin: sourceOriginOfficial, ProductArea: "resource_purchase",
			ACL: customerSafeACL, Confidence: confidenceHigh, Title: "RTX 4090 规格",
			DocumentID: "doc-rtx", ParentID: "doc-rtx", ChunkOrdinal: 1,
			ExactTerms: []string{"RTX 4090", "GeForce RTX 4090"},
			ValidFrom:  "2025-01-01", Content: "RTX 4090 可用于图像生成工作流。",
		},
		{
			ChunkID: "qwen-reranker", KBVersion: "kb-test", SourceType: sourceTypeRunbook,
			SourceOrigin: sourceOriginOfficial, ProductArea: "rag",
			ACL: customerSafeACL, Confidence: confidenceHigh, Title: "检索服务配置",
			ValidFrom: "2025-01-01", Content: "默认 reranker 使用 qwen3-reranker-8b。",
		},
	}
}

func TestMetadataExactIndexMatchesCuratedMultiTokenModel(t *testing.T) {
	chunks := metadataTestChunks()
	index := newMetadataExactIndex(chunks)

	candidates := index.candidates("RTX 4090 有多少显存", chunks, fixedRetrieverNow())
	require.NotEmpty(t, candidates)
	assert.Equal(t, "rtx-4090", candidates[0].chunk.ChunkID)
}

func TestMetadataExactIndexUsesLegacyContentWithoutCorpusMigration(t *testing.T) {
	chunks := metadataTestChunks()
	index := newMetadataExactIndex(chunks)

	candidates := index.candidates("qwen3-reranker-8b 的超时如何设置", chunks, fixedRetrieverNow())
	require.NotEmpty(t, candidates)
	assert.Equal(t, "qwen-reranker", candidates[0].chunk.ChunkID)
}

func TestMetadataExactIndexSkipsNaturalLanguageQuery(t *testing.T) {
	chunks := metadataTestChunks()
	index := newMetadataExactIndex(chunks)

	assert.Empty(t, index.candidates("怎么选择适合的显卡", chunks, fixedRetrieverNow()))
}

func TestMetadataExplicitTermDoesNotMatchNumericPrefix(t *testing.T) {
	chunks := metadataTestChunks()
	chunks[1].ExactTerms = []string{"100"}
	index := newMetadataExactIndex(chunks)

	assert.Empty(t, index.candidates("1000GB 的磁盘空间", chunks, fixedRetrieverNow()))
}

func TestMetadataExactIndexRespectsAdmissibility(t *testing.T) {
	chunks := metadataTestChunks()
	chunks[1].ValidTo = ptrString("2025-12-31")
	index := newMetadataExactIndex(chunks)

	now := time.Date(2026, 8, 2, 0, 0, 0, 0, beijingLocation)
	assert.Empty(t, index.candidates("RTX 4090 有多少显存", chunks, now))
}

func TestRRFFusionWithMetadataCarriesMetadataRank(t *testing.T) {
	chunkA := KBChunk{ChunkID: "a"}
	chunkB := KBChunk{ChunkID: "b"}
	fused, info := rrfFusionWithMetadata(
		[]scoredChunk{{chunk: chunkA}},
		[]scoredChunk{{chunk: chunkB}},
		[]scoredChunk{{chunk: chunkB}},
		2,
	)

	require.Len(t, fused, 2)
	assert.Equal(t, "b", fused[0].chunk.ChunkID, "dense + metadata should outrank the BM25-only candidate")
	assert.Equal(t, 1, info["b"].DenseRank)
	assert.Equal(t, 1, info["b"].MetadataRank)
	assert.Zero(t, info["a"].MetadataRank)
}
