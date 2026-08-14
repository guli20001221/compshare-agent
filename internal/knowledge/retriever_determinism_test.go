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
			clock := probeClockFor(t, corpus)
			for run := 0; run < 25; run++ {
				retriever := NewRetriever(corpus, RetrieverOptions{
					TopK: 10,
					Mode: RetrievalModeBM25Only,
					Now:  clock,
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
		Now:  probeClockFor(t, corpus),
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

// probeClockFor returns a fixed clock that is guaranteed to sit inside the
// pinned corpus's validity window.
//
// These probes need a FIXED clock — a moving one would make a determinism test
// non-deterministic — but they do not need a PARTICULAR date, only one at or
// after every chunk's valid_from. A literal cannot know that. This clock was
// frozen at 2026-07-16 against a corpus stamped valid_from 2026-07-15; the
// 2026-08-14 rebuild stamped 2026-08-14, chunkActiveAt then rejected all 526
// chunks as not-yet-valid, and four BM25 probes reported "retrieved nothing".
// That reads exactly like a corpus that lost its billing documents, and it cost
// a real investigation to establish that it had not.
//
// Deriving the date from the corpus keeps both properties: the corpus is pinned
// by digest, so max(valid_from) over it is as constant as a literal was, and it
// cannot fall outside the window on the next rebuild.
func probeClockFor(t *testing.T, corpus Corpus) func() time.Time {
	t.Helper()
	latest := ""
	for _, chunk := range corpus.Chunks {
		// ISO-8601 dates order lexicographically.
		if chunk.ValidFrom > latest {
			latest = chunk.ValidFrom
		}
	}
	require.NotEmpty(t, latest, "corpus carries no valid_from; the probe clock cannot be derived from it")
	beijing := time.FixedZone("Asia/Shanghai", 8*60*60)
	day, err := time.ParseInLocation("2006-01-02", latest, beijing)
	require.NoError(t, err, "corpus valid_from %q is not a date", latest)
	// Deriving the clock from the corpus would also hide the one case where
	// "retrieved nothing" IS the corpus's fault: a chunk stamped valid_from in
	// the future is invisible in production too, where the clock is real.
	require.False(t, day.After(time.Now()),
		"corpus valid_from %s has not arrived yet; production retrieval would return nothing for these chunks", latest)
	return func() time.Time { return day.Add(12 * time.Hour) }
}
