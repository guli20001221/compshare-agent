package knowledge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetrieverFindsBillingChunk(t *testing.T) {
	corpus, err := LoadCorpus("testdata/curated_faq.jsonl")
	require.NoError(t, err)

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "billing")

	require.False(t, result.Empty)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, "kb-curated-test", result.KBVersion)
	assert.Equal(t, "faq-billing-001", result.Hits[0].ChunkID)
}

func TestRetrieverFindsFinanceRuleChunks(t *testing.T) {
	corpus, err := LoadCorpus("testdata/curated_faq.jsonl")
	require.NoError(t, err)

	cases := []struct {
		name        string
		question    string
		wantChunkID string
	}{
		{name: "invoice howto", question: "\u600e\u4e48\u5f00\u53d1\u7968", wantChunkID: "faq-billing-invoice-001"},
		{name: "arrears handling", question: "\u6b20\u8d39\u600e\u4e48\u529e", wantChunkID: "faq-billing-arrears-001"},
		{name: "billing mode difference", question: "\u6309\u91cf\u548c\u5305\u65e5\u6709\u4ec0\u4e48\u533a\u522b", wantChunkID: "faq-billing-mode-001"},
		{name: "refund rules", question: "\u9000\u6b3e\u89c4\u5219\u662f\u4ec0\u4e48", wantChunkID: "faq-billing-refund-001"},
		{name: "expiry renewal", question: "\u5957\u9910\u5230\u671f\u600e\u4e48\u529e", wantChunkID: "faq-billing-expiry-renewal-001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve(tc.question, "billing")

			require.False(t, result.Empty)
			require.NotEmpty(t, result.Hits)
			assert.Equal(t, tc.wantChunkID, result.Hits[0].ChunkID)
		})
	}
}

func TestRetrieverFindsImageChunk(t *testing.T) {
	corpus, err := LoadCorpus("testdata/curated_faq.jsonl")
	require.NoError(t, err)

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("平台镜像有哪些", "image")

	require.False(t, result.Empty)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, "faq-image-001", result.Hits[0].ChunkID)
}

func TestRetrieverReturnsEmptyForUnrelatedQuestion(t *testing.T) {
	corpus, err := LoadCorpus("testdata/curated_faq.jsonl")
	require.NoError(t, err)

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("今天天气怎么样", "")

	assert.True(t, result.Empty)
	assert.Empty(t, result.Hits)
	assert.Equal(t, "kb-curated-test", result.KBVersion)
}

func TestRetrieverIgnoresExpiredAndFutureChunks(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("expired", "billing", "high", "计费", "关机后还收费吗", "关机后磁盘收费", "2026-01-01", ptrString("2026-04-01")),
			testChunk("future", "billing", "high", "计费", "关机后还收费吗", "关机后磁盘收费", "2026-06-01", nil),
			testChunk("current", "billing", "high", "计费", "关机后还收费吗", "关机后磁盘收费", "2026-05-01", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "billing")

	require.False(t, result.Empty)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, "current", result.Hits[0].ChunkID)
}

func TestRetrieverDoesNotReturnLowConfidenceChunks(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("low", "billing", "low", "计费", "关机后还收费吗", "关机后磁盘收费", "", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "billing")

	assert.True(t, result.Empty)
	assert.Empty(t, result.Hits)
}

func TestRetrieverRankingIsStable(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("faq-z", "billing", "medium", "计费", "关机后还收费吗", "关机后磁盘收费", "", nil),
			testChunk("faq-a", "billing", "medium", "计费", "关机后还收费吗", "关机后磁盘收费", "", nil),
			testChunk("faq-high", "billing", "high", "计费", "关机后还收费吗", "关机后磁盘收费", "", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{TopK: 3, Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "billing")

	require.Len(t, result.Hits, 3)
	assert.Equal(t, []string{"faq-high", "faq-a", "faq-z"}, chunkIDs(result.Hits))
}

func TestRetrieverHonorsTopKAndThresholdOptions(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("match-1", "billing", "high", "计费", "关机后还收费吗", "关机后磁盘收费", "", nil),
			testChunk("match-2", "billing", "high", "磁盘", "额外磁盘收费吗", "额外磁盘收费", "", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{TopK: 1, Threshold: 2, Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "billing")

	require.Len(t, result.Hits, 1)
	assert.Equal(t, "match-1", result.Hits[0].ChunkID)
}

func TestRetrieverQuestionPatternScoreIsBinary(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			{
				ChunkID:     "pattern-bloat",
				KBVersion:   "kb-test",
				SourceType:  sourceTypeFAQ,
				ProductArea: "billing",
				ACL:         customerSafeACL,
				Confidence:  confidenceMedium,
				Title:       "其他",
				QuestionPatterns: []string{
					"关机后还收费吗",
					"关机后",
					"收费吗",
				},
				Content: "其他",
			},
			testChunk("better-match", "billing", "medium", "关机后还收费吗", "关机后还收费吗", "关机后还收费吗", "", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{TopK: 2, Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "billing")

	require.Len(t, result.Hits, 2)
	assert.Equal(t, "better-match", result.Hits[0].ChunkID)
}

func TestRetrieverUsesBeijingDateForValidity(t *testing.T) {
	beijing := time.FixedZone("Asia/Shanghai", 8*60*60)
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("expired-local-day", "billing", "high", "计费", "关机后还收费吗", "关机后磁盘收费", "", ptrString("2026-05-09")),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{
		Now: func() time.Time {
			return time.Date(2026, 5, 10, 1, 0, 0, 0, beijing)
		},
	}).Retrieve("关机后还收费吗", "billing")

	assert.True(t, result.Empty)
}

func TestRetrieverFiltersSubThresholdMatches(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("content-only", "billing", "high", "其他", "其他", "关机后还收费吗", "", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("关机后还收费吗", "")

	assert.True(t, result.Empty)
}

func TestRetrieverDoesNotMatchOnProductAreaAlone(t *testing.T) {
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("billing-only", "billing", "high", "退款规则", "退款规则是什么", "退款规则说明", "", nil),
		},
	}

	result := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow}).Retrieve("怎么开发票", "billing")

	assert.True(t, result.Empty)
	assert.Empty(t, result.Hits)
}

func fixedRetrieverNow() time.Time {
	return time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
}

func testChunk(id, area, confidence, title, pattern, content, validFrom string, validTo *string) KBChunk {
	return KBChunk{
		ChunkID:          id,
		KBVersion:        "kb-test",
		SourceType:       sourceTypeFAQ,
		ProductArea:      area,
		ACL:              customerSafeACL,
		ValidFrom:        validFrom,
		ValidTo:          validTo,
		Confidence:       confidence,
		Title:            title,
		QuestionPatterns: []string{pattern},
		Content:          content,
	}
}

func ptrString(s string) *string {
	return &s
}

func chunkIDs(chunks []KBChunk) []string {
	ids := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		ids = append(ids, chunk.ChunkID)
	}
	return ids
}
