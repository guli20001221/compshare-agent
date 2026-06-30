package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/require"
)

func TestRetrievalRecallRealCorpusDiskBilling(t *testing.T) {
	result := retrieveRealCorpusForTest(t,
		"我没看懂收费，磁盘空间是如何收费的？100GB原始空间是免费的吗",
	)

	requireRecallChunkID(t, result, "w0-billing_rule-gitlab-compshare-docs-operation--73bf395c")
	requireRecallText(t, result, "系统盘")
	requireRecallText(t, result, "数据盘")
	requireRecallText(t, result, "免费额度")
}

func TestRetrievalRecallRealCorpusCodingPlanManagement(t *testing.T) {
	result := retrieveRealCorpusForTest(t, "删除 Coding Plan 包")

	requireRecallChunkID(t, result, "w0-modelverse-gitlab-compshare-docs-package-63801282")
	requireRecallText(t, result, "套餐管理")
	requireRecallText(t, result, "不支持退款")
}

func TestRetrievalRecallRealCorpusStockShortage(t *testing.T) {
	result := retrieveRealCorpusForTest(t, "一直暂无资源 是什么情况")

	requireRecallChunkID(t, result, "w0-resource_purchase-gitlab-compshare-docs-gpus-insta-2e534ae3")
	requireRecallText(t, result, "CheckCompShareResourceCapacity")
	requireRecallText(t, result, "ResourceEnough")
}

func TestRetrievalRecallRealCorpusStockStatusSemantics(t *testing.T) {
	result := retrieveRealCorpusForTest(t, "Normal 状态是不是说明一定有库存")

	requireRecallText(t, result, "CheckCompShareResourceCapacity")
	requireRecallText(t, result, "ResourceEnough")
	requireRecallText(t, result, "DescribeAvailableCompShareInstanceTypes")
	requireRecallText(t, result, "Normal")
	requireRecallText(t, result, "SoldOut")
}

func retrieveRealCorpusForTest(t *testing.T, query string) knowledge.RetrievalResult {
	t.Helper()

	corpus, err := knowledge.LoadPinnedCorpus(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)
	retriever := knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{
		TopK: 10,
		Mode: knowledge.RetrievalModeBM25Only,
		Now:  realCorpusRecallNow,
	})
	result := retriever.Retrieve(query, inferKnowledgeProductArea(query))
	require.False(t, result.Empty, "query %q returned no real-corpus hits", query)
	require.NotEmpty(t, result.HitItems, "query %q returned no real-corpus hit items", query)
	return result
}

func realCorpusRecallNow() time.Time {
	return time.Date(2026, 6, 30, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
}

func requireRecallChunkID(t *testing.T, result knowledge.RetrievalResult, chunkID string) {
	t.Helper()
	for _, hit := range result.HitItems {
		if hit.Chunk.ChunkID == chunkID {
			return
		}
	}
	t.Fatalf("expected retrieved chunk %q in top hits, got %s", chunkID, recallChunkIDs(result))
}

func requireRecallText(t *testing.T, result knowledge.RetrievalResult, needle string) {
	t.Helper()
	for _, hit := range result.HitItems {
		haystack := hit.Chunk.Title + "\n" + hit.Chunk.Content + "\n" + strings.Join(hit.Chunk.QuestionPatterns, "\n")
		if strings.Contains(haystack, needle) {
			return
		}
	}
	t.Fatalf("expected retrieved evidence containing %q, got chunks %s", needle, recallChunkIDs(result))
}

func recallChunkIDs(result knowledge.RetrievalResult) string {
	ids := make([]string, 0, len(result.HitItems))
	for _, hit := range result.HitItems {
		ids = append(ids, hit.Chunk.ChunkID)
	}
	return strings.Join(ids, ", ")
}
