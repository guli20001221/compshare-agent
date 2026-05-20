package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCorpusLoadsCustomerSafeJSONL(t *testing.T) {
	corpus, err := LoadCorpus(filepath.Join("testdata", "curated_faq.jsonl"))
	require.NoError(t, err)

	require.Len(t, corpus.Chunks, 7)
	assert.Equal(t, "kb-curated-test", corpus.KBVersion)
	assert.Equal(t, "faq-image-001", corpus.Chunks[0].ChunkID)
	assert.Equal(t, "customer_safe", corpus.Chunks[0].ACL)
	assert.Equal(t, "official", corpus.Chunks[0].SourceOrigin)
	assert.Equal(t, []string{"平台镜像有哪些", "社区镜像和平台镜像区别"}, corpus.Chunks[0].QuestionPatterns)
}

func TestLoadDeployCuratedFAQIncludesFinanceRules(t *testing.T) {
	corpus, err := LoadCorpus(filepath.Join("..", "..", "deploy", "kb", "curated_faq.jsonl"))
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, chunk := range corpus.Chunks {
		ids[chunk.ChunkID] = true
	}
	for _, id := range []string{
		"faq-billing-invoice-001",
		"faq-billing-arrears-001",
		"faq-billing-mode-001",
		"faq-billing-refund-001",
		"faq-billing-expiry-renewal-001",
	} {
		assert.True(t, ids[id], "deploy FAQ missing %s", id)
	}
}

func TestCorpusDigestExpectedMatchesStage2BW0(t *testing.T) {
	got, err := ComputeCorpusFileDigest(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)

	assert.Equal(t, CorpusDigestExpected, got)
}

func TestCorpusDigestIgnoresLineEndingDifferences(t *testing.T) {
	lf, err := ComputeCorpusDigest(strings.NewReader("a\nb\n"))
	require.NoError(t, err)
	crlf, err := ComputeCorpusDigest(strings.NewReader("a\r\nb\r\n"))
	require.NoError(t, err)

	assert.Equal(t, lf, crlf)
}

func TestLoadPinnedCorpusLoadsStage2BW0(t *testing.T) {
	corpus, err := LoadPinnedCorpus(filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl"))
	require.NoError(t, err)

	assert.Equal(t, "kb.stage2b.w0.2026-05-19.package-policy", corpus.KBVersion)
	assert.Len(t, corpus.Chunks, 228)
	for _, chunk := range corpus.Chunks {
		assert.Equal(t, "official", chunk.SourceOrigin)
		assert.Nil(t, chunk.SurfaceURL)
	}
}

func TestLoadPinnedCorpusRejectsDigestMismatch(t *testing.T) {
	path := writeCorpusFile(t, validChunkWithPatterns(t, []string{"ok"}))

	_, err := LoadPinnedCorpus(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corpus digest mismatch")
}

func TestLoadCorpusRejectsInvalidJSONWithRowNumber(t *testing.T) {
	path := writeCorpusFile(t, `{"chunk_id":"ok","kb_version":"kb","source_type":"faq","source_origin":"official","product_area":"billing","acl":"customer_safe","confidence":"high","title":"ok","content":"ok"}
{bad json}
`)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row 2")
}

func TestLoadCorpusRejectsMissingRequiredField(t *testing.T) {
	path := writeCorpusFile(t, `{"chunk_id":"faq-missing-title","kb_version":"kb","source_type":"faq","source_origin":"official","product_area":"billing","acl":"customer_safe","confidence":"high","content":"ok"}`)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "title")
}

func TestLoadCorpusRejectsNonCustomerSafeACL(t *testing.T) {
	path := writeCorpusFile(t, `{"chunk_id":"faq-internal","kb_version":"kb","source_type":"faq","source_origin":"official","product_area":"billing","acl":"internal_ops","confidence":"high","title":"internal","content":"ok"}`)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "customer_safe")
}

func TestLoadCorpusRejectsOpsChatSourceType(t *testing.T) {
	path := writeCorpusFile(t, `{"chunk_id":"faq-ops-chat","kb_version":"kb","source_type":"ops_chat","source_origin":"official","product_area":"billing","acl":"customer_safe","confidence":"high","title":"ops","content":"ok"}`)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source_type")
}

func TestLoadCorpusRejectsMissingSourceOrigin(t *testing.T) {
	path := writeCorpusFile(t, `{"chunk_id":"faq-missing-origin","kb_version":"kb","source_type":"faq","product_area":"billing","acl":"customer_safe","confidence":"high","title":"ok","content":"ok"}`)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source_origin")
}

func TestLoadCorpusRejectsInvalidSourceOrigin(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"source_origin":"official"`, `"source_origin":"invalid"`, 1))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source_origin")
	assert.Contains(t, err.Error(), "official or support_curated")
}

func TestLoadCorpusAcceptsValidSourceOrigins(t *testing.T) {
	for _, origin := range []string{"official", "support_curated"} {
		t.Run(origin, func(t *testing.T) {
			path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"source_origin":"official"`, `"source_origin":"`+origin+`"`, 1))

			corpus, err := LoadCorpus(path)
			require.NoError(t, err)
			require.Len(t, corpus.Chunks, 1)
			assert.Equal(t, origin, corpus.Chunks[0].SourceOrigin)
		})
	}
}

func TestLoadCorpusAcceptsNilSurfaceURL(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"content":"ok"`, `"surface_url":null,"content":"ok"`, 1))

	corpus, err := LoadCorpus(path)
	require.NoError(t, err)
	require.Len(t, corpus.Chunks, 1)
	assert.Nil(t, corpus.Chunks[0].SurfaceURL)
}

func TestLoadCorpusAcceptsAllowedSurfaceURLConsole(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"content":"ok"`, `"surface_url":"https://console.compshare.cn/instance/list","content":"ok"`, 1))

	corpus, err := LoadCorpus(path)
	require.NoError(t, err)
	require.NotNil(t, corpus.Chunks[0].SurfaceURL)
	assert.Equal(t, "https://console.compshare.cn/instance/list", *corpus.Chunks[0].SurfaceURL)
}

func TestLoadCorpusAcceptsAllowedSurfaceURLDocs(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"content":"ok"`, `"surface_url":"https://www.compshare.cn/docs/intro","content":"ok"`, 1))

	corpus, err := LoadCorpus(path)
	require.NoError(t, err)
	require.NotNil(t, corpus.Chunks[0].SurfaceURL)
	assert.Equal(t, "https://www.compshare.cn/docs/intro", *corpus.Chunks[0].SurfaceURL)
}

func TestLoadCorpusRejectsSurfaceURLByPolicyDelegation(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"content":"ok"`, `"surface_url":"http://compshare.cn/docs","content":"ok"`, 1))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "surface_url rejected by policy")
	assert.Contains(t, err.Error(), "scheme_not_https")
}

func TestLoadCorpusRejectsOversizedQuestionPatterns(t *testing.T) {
	patterns := make([]string, 21)
	for i := range patterns {
		patterns[i] = "pattern"
	}
	path := writeCorpusFile(t, validChunkWithPatterns(t, patterns))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "question_patterns")
}

func TestLoadCorpusRejectsOversizedQuestionPatternEntry(t *testing.T) {
	path := writeCorpusFile(t, validChunkWithPatterns(t, []string{strings.Repeat("字", 201)}))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "question_patterns[0]")
}

func TestLoadCorpusRejectsOversizedContent(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"content":"ok"`, `"content":"`+strings.Repeat("字", 4001)+`"`, 1))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content")
}

func TestLoadCorpusRejectsInvalidConfidence(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"confidence":"high"`, `"confidence":"extreme"`, 1))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "confidence")
}

func TestLoadCorpusRejectsInvalidDates(t *testing.T) {
	path := writeCorpusFile(t, strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"content":"ok"`, `"valid_from":"not-a-date","valid_to":"2026-99-99","content":"ok"`, 1))

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "valid_from")
}

func TestLoadCorpusRejectsDuplicateChunkID(t *testing.T) {
	line := validChunkWithPatterns(t, []string{"ok"})
	path := writeCorpusFile(t, line+"\n"+line)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate chunk_id")
}

func TestLoadCorpusRejectsMixedKBVersions(t *testing.T) {
	first := validChunkWithPatterns(t, []string{"ok"})
	second := strings.Replace(validChunkWithPatterns(t, []string{"ok"}), `"chunk_id":"faq-valid"`, `"chunk_id":"faq-valid-2"`, 1)
	second = strings.Replace(second, `"kb_version":"kb"`, `"kb_version":"kb-other"`, 1)
	path := writeCorpusFile(t, first+"\n"+second)

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row 2")
	assert.Contains(t, err.Error(), "kb_version")
}

func TestLoadCorpusRejectsEmptyCorpus(t *testing.T) {
	path := writeCorpusFile(t, "\n\n")

	_, err := LoadCorpus(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty corpus")
}

func writeCorpusFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func validChunkWithPatterns(t *testing.T, patterns []string) string {
	t.Helper()
	chunk := map[string]any{
		"chunk_id":          "faq-valid",
		"kb_version":        "kb",
		"source_type":       "faq",
		"source_origin":     "official",
		"product_area":      "billing",
		"acl":               "customer_safe",
		"confidence":        "high",
		"title":             "valid",
		"question_patterns": patterns,
		"content":           "ok",
	}
	data, err := json.Marshal(chunk)
	require.NoError(t, err)
	return string(data)
}
