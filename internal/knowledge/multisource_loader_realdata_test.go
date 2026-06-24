package knowledge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadExternalCorpusPinnedRealData proves the shipped external corpus +
// its qwen3 sidecar load clean against their pinned digests, with a strict
// chunk↔vector bijection, and that every chunk carries an external_* origin and
// customer_safe acl. This is the Go side of the Phase 1 "Go-loads-clean" gate
// that a retrieval byte-match alone is blind to.
func TestLoadExternalCorpusPinnedRealData(t *testing.T) {
	ext := filepath.Join("..", "..", "deploy", "kb", "external_w0.jsonl")
	extSidecar := filepath.Join("..", "..", "deploy", "kb", "embeddings_"+ExternalCorpusDigestExpected+"_qwen3-embedding-8b.jsonl")
	for _, p := range []string{ext, extSidecar} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing %s (%v); skipping integration check", p, err)
		}
	}
	corpus, sidecar, err := loadPinnedCorpusWithDigests(ext, extSidecar, ExternalCorpusDigestExpected, ExternalEmbeddingDigestExpectedQwen3)
	require.NoError(t, err)
	assert.Equal(t, 224, len(corpus.Chunks)) // 180 prior external chunks + 44 production/research platform topics
	assert.Equal(t, 4096, sidecar.Dim)
	assert.Equal(t, 224, len(sidecar.Vectors))
	for _, c := range corpus.Chunks {
		assert.Contains(t, []string{"external_official", "external_community"}, c.SourceOrigin, "chunk %s origin", c.ChunkID)
		assert.Equal(t, "customer_safe", c.ACL, "chunk %s acl", c.ChunkID)
	}
}

// TestMergePlatformAndExternalRealData proves the platform and external corpora
// merge into one in-memory index through LoadPinnedCorporaWithEmbeddings: no
// chunk_id collision across the two sources, a single homogeneous qwen3 vector
// space, and a complete chunk↔vector bijection over the union. This is the
// runtime shape cmd/trace.go will produce in Phase 2.
func TestMergePlatformAndExternalRealData(t *testing.T) {
	platformCorpus := filepath.Join("..", "..", "deploy", "kb", "stage2b_w0.jsonl")
	platformSidecar := filepath.Join("..", "..", "deploy", "kb", "embeddings_"+CorpusDigestExpected+"_qwen3-embedding-8b.jsonl")
	ext := filepath.Join("..", "..", "deploy", "kb", "external_w0.jsonl")
	extSidecar := filepath.Join("..", "..", "deploy", "kb", "embeddings_"+ExternalCorpusDigestExpected+"_qwen3-embedding-8b.jsonl")
	for _, p := range []string{platformCorpus, platformSidecar, ext, extSidecar} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing %s (%v); skipping integration check", p, err)
		}
	}
	merged, sidecar, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{
		{CorpusPath: platformCorpus, EmbeddingsPath: platformSidecar, ExpectedCorpusDigest: CorpusDigestExpected, ExpectedEmbeddingDigest: EmbeddingDigestExpectedQwen3},
		{CorpusPath: ext, EmbeddingsPath: extSidecar, ExpectedCorpusDigest: ExternalCorpusDigestExpected, ExpectedEmbeddingDigest: ExternalEmbeddingDigestExpectedQwen3},
	})
	require.NoError(t, err)
	assert.Equal(t, "merged", merged.KBVersion)
	assert.Equal(t, 687+224, len(merged.Chunks))
	assert.Equal(t, 4096, sidecar.Dim)
	assert.Equal(t, 687+224, len(sidecar.Vectors))
	for _, c := range merged.Chunks {
		_, ok := sidecar.Vectors[c.ChunkID]
		assert.True(t, ok, "missing vector for merged chunk %s", c.ChunkID)
	}
}
