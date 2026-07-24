package knowledge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRetrieverIsDeterministicForAnIdenticalQuery decides where a measured
// retrieval-instability floor actually comes from.
//
// An A/A probe (same arm, 50 real questions, run twice) found the retrieved
// chunk set differing on 50% of questions. That was attributed to the LLM query
// planner, but the attribution only holds if retrieval itself is a pure function
// of the query text. Go makes that assumption easy to get wrong: sort.SliceStable
// preserves INPUT order, so if candidates are accumulated by ranging a map, ties
// resolve differently run to run and the top-K silently reshuffles.
//
// If this test fails, the instability is in production retrieval, not in the
// eval harness, and it affects real answers rather than just experiments.
func TestRetrieverIsDeterministicForAnIdenticalQuery(t *testing.T) {
	corpus, err := LoadPinnedCorpus(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)

	queries := []string{
		"磁盘空间是如何收费的",
		"一直暂无资源 是什么情况",
		"重装系统会不会清空数据盘",
		"关机之后还会继续扣费吗",
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			var first []string
			for run := 0; run < 25; run++ {
				retriever := NewRetriever(corpus, RetrieverOptions{
					TopK: 10,
					Mode: RetrievalModeBM25Only,
					Now:  determinismProbeNow,
				})
				got := chunkIDsOf(retriever.Retrieve(query, ""))
				if run == 0 {
					first = got
					require.NotEmpty(t, first, "query %q retrieved nothing; probe proves nothing", query)
					continue
				}
				require.Equal(t, first, got,
					"run %d returned a different ranking for an identical query.\n first: %s\n got  : %s",
					run, strings.Join(first, ","), strings.Join(got, ","))
			}
		})
	}
}

// TestRetrieverIsDeterministicAcrossOneInstance covers the other shape: reusing
// a single retriever (as production does) rather than rebuilding it per call.
func TestRetrieverIsDeterministicAcrossOneInstance(t *testing.T) {
	corpus, err := LoadPinnedCorpus(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)
	retriever := NewRetriever(corpus, RetrieverOptions{
		TopK: 10,
		Mode: RetrievalModeBM25Only,
		Now:  determinismProbeNow,
	})

	const query = "磁盘空间是如何收费的"
	first := chunkIDsOf(retriever.Retrieve(query, ""))
	require.NotEmpty(t, first)
	for run := 1; run < 25; run++ {
		require.Equal(t, first, chunkIDsOf(retriever.Retrieve(query, "")),
			"reused retriever returned a different ranking on run %d", run)
	}
}

func chunkIDsOf(result RetrievalResult) []string {
	ids := make([]string, 0, len(result.HitItems))
	for _, hit := range result.HitItems {
		ids = append(ids, hit.Chunk.ChunkID)
	}
	return ids
}

func determinismProbeNow() time.Time {
	return time.Date(2026, 7, 16, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
}
