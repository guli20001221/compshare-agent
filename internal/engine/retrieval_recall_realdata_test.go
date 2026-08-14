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

	requireRecallText(t, result, "系统盘")
	requireRecallText(t, result, "数据盘")
	requireRecallText(t, result, "免费额度")
}

func TestRetrievalRecallRealCorpusCodingPlanManagement(t *testing.T) {
	result := retrieveRealCorpusForTest(t, "删除 Coding Plan 包")

	requireRecallText(t, result, "套餐管理")
	requireRecallText(t, result, "不支持退款")
}

func TestRetrievalRecallRealCorpusStockShortage(t *testing.T) {
	result := retrieveRealCorpusForTest(t, "一直暂无资源 是什么情况")

	requireRecallText(t, result, "CheckCompShareResourceCapacity")
	requireRecallText(t, result, "ResourceEnough")
}

// TestRetrievalRecallRealCorpusStockStatusSemantics asserts that the corpus
// documents stock-status semantics and that BM25 reaches each fact.
//
// It used to ask "Normal 状态是不是说明一定有库存" and require all five needles
// from that one query. It passed for a reason worth writing down: the corpus
// carried a hand-injected question pattern, "Normal 状态一定有库存吗" plus the
// bare token "SoldOut", stamped onto eight chunks. The test query was a
// restatement of that injected string, so BM25 was matching the injection
// against itself and the pass said nothing about the documents. The 2026-08-14
// rebuild deleted the templated question_patterns (2594 -> 1333) and the test
// failed — which read as the corpus having lost its capacity documentation.
// It had not: the `content` field carries "SoldOut" in exactly the same one
// chunk before and after, and "Normal" in the same three.
//
// What the injection also hid is that the two facts live in two DIFFERENT
// documents, so no single query was ever going to recall both from real text:
// DescribeAvailableCompShareInstanceTypes documents the Normal/SoldOut status
// enum, CheckCompShareResourceCapacity is the authoritative stock check and
// documents ResourceEnough. This asks each document for what it actually says.
//
// Known gap, deliberately not papered over: the original user-style phrasing
// recalls neither document under BM25 alone. Do not restore a question pattern
// to make it pass — that is the bug this comment exists to prevent. Production
// runs RRF plus a cross-encoder over planner-expanded queries, not this path;
// whether that closes the gap is unmeasured, because the shipped sidecar was
// replaced by this release and there is no baseline left to compare against.
func TestRetrievalRecallRealCorpusStockStatusSemantics(t *testing.T) {
	statusEnum := retrieveRealCorpusForTest(t, "可用机型列表 状态 库存 是否有货")
	requireRecallText(t, statusEnum, "DescribeAvailableCompShareInstanceTypes")
	requireRecallText(t, statusEnum, "Normal")
	requireRecallText(t, statusEnum, "SoldOut")

	capacity := retrieveRealCorpusForTest(t, "怎么判断卡型规格库存是否充足")
	requireRecallText(t, capacity, "CheckCompShareResourceCapacity")
	requireRecallText(t, capacity, "ResourceEnough")
}

func retrieveRealCorpusForTest(t *testing.T, query string) knowledge.RetrievalResult {
	t.Helper()

	corpus, err := knowledge.LoadPinnedCorpus(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)
	retriever := knowledge.NewRetriever(corpus, knowledge.RetrieverOptions{
		TopK: 10,
		Mode: knowledge.RetrievalModeBM25Only,
		Now:  realCorpusRecallNow(t, corpus),
	})
	result := retriever.Retrieve(query, "")
	require.False(t, result.Empty, "query %q returned no real-corpus hits", query)
	require.NotEmpty(t, result.HitItems, "query %q returned no real-corpus hit items", query)
	return result
}

// realCorpusRecallNow returns a fixed clock inside the pinned corpus's validity
// window, derived from the corpus rather than written down.
//
// It used to be the literal 2026-07-16, chosen for a corpus stamped valid_from
// 2026-07-15. The 2026-08-14 rebuild stamped 2026-08-14, every chunk became
// not-yet-valid, and these recall tests failed as "returned no real-corpus
// hits" — indistinguishable from the corpus having lost the documents. The
// corpus is pinned by digest, so deriving the date from it is exactly as
// deterministic as the literal was, and it survives the next rebuild.
// internal/knowledge has the same helper for the same reason.
func realCorpusRecallNow(t *testing.T, corpus knowledge.Corpus) func() time.Time {
	t.Helper()
	latest := ""
	for _, chunk := range corpus.Chunks {
		// ISO-8601 dates order lexicographically.
		if chunk.ValidFrom > latest {
			latest = chunk.ValidFrom
		}
	}
	require.NotEmpty(t, latest, "corpus carries no valid_from; the recall clock cannot be derived from it")
	beijing := time.FixedZone("Asia/Shanghai", 8*60*60)
	day, err := time.ParseInLocation("2006-01-02", latest, beijing)
	require.NoError(t, err, "corpus valid_from %q is not a date", latest)
	require.False(t, day.After(time.Now()),
		"corpus valid_from %s has not arrived yet; production retrieval would return nothing for these chunks", latest)
	return func() time.Time { return day.Add(12 * time.Hour) }
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
