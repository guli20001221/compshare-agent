package knowledge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePinnedSource writes a tiny corpus + matching sidecar into a temp dir and
// returns a PinnedCorpusSource whose expected digests are the freshly computed
// file digests, so LoadPinnedCorporaWithEmbeddings's per-source pins pass. Each
// chunk gets a constant non-zero vector of the requested dim.
func writePinnedSource(t *testing.T, kbVersion, sourceOrigin, embedModel string, dim int, ids ...string) PinnedCorpusSource {
	t.Helper()
	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.jsonl")
	sidecarPath := filepath.Join(dir, "sidecar.jsonl")

	var corpusBuf, sidecarBuf strings.Builder
	sidecarBuf.WriteString(fmt.Sprintf(`{"_meta":{"corpus_digest":"x","embed_model":%q,"dim":%d,"rows":%d}}`+"\n", embedModel, dim, len(ids)))
	for _, id := range ids {
		row := map[string]any{
			"chunk_id":      id,
			"kb_version":    kbVersion,
			"source_type":   "faq",
			"source_origin": sourceOrigin,
			"product_area":  "login",
			"acl":           "customer_safe",
			"confidence":    "high",
			"title":         id,
			"content":       "content for " + id,
		}
		data, err := json.Marshal(row)
		require.NoError(t, err)
		corpusBuf.Write(data)
		corpusBuf.WriteByte('\n')

		vec := make([]float32, dim)
		for i := range vec {
			vec[i] = 0.1
		}
		vrow, err := json.Marshal(map[string]any{"chunk_id": id, "vector": vec})
		require.NoError(t, err)
		sidecarBuf.Write(vrow)
		sidecarBuf.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(corpusPath, []byte(corpusBuf.String()), 0o600))
	require.NoError(t, os.WriteFile(sidecarPath, []byte(sidecarBuf.String()), 0o600))

	corpusDigest, err := ComputeCorpusFileDigest(corpusPath)
	require.NoError(t, err)
	embDigest, err := ComputeEmbeddingFileDigest(sidecarPath)
	require.NoError(t, err)
	return PinnedCorpusSource{
		CorpusPath:              corpusPath,
		EmbeddingsPath:          sidecarPath,
		ExpectedCorpusDigest:    corpusDigest,
		ExpectedEmbeddingDigest: embDigest,
	}
}

func chunkIDSet(corpus Corpus) map[string]struct{} {
	out := map[string]struct{}{}
	for _, c := range corpus.Chunks {
		out[c.ChunkID] = struct{}{}
	}
	return out
}

// Merging a platform-origin and an external-origin source produces one flat
// index: all chunks + all vectors, KBVersion generalized to "merged" because the
// two sources carry different kb_versions.
func TestLoadPinnedCorporaMergesSources(t *testing.T) {
	t.Parallel()
	platform := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1", "p2")
	external := writePinnedSource(t, "kb.external", "external_official", "qwen3-embedding-8b", 4, "e1", "e2")

	corpus, sidecar, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{platform, external})
	require.NoError(t, err)

	assert.Equal(t, "merged", corpus.KBVersion)
	require.Len(t, corpus.Chunks, 4)
	assert.Equal(t, map[string]struct{}{"p1": {}, "p2": {}, "e1": {}, "e2": {}}, chunkIDSet(corpus))
	// Per-chunk provenance is preserved even though the corpus-level label is generalized.
	for _, c := range corpus.Chunks {
		switch c.ChunkID {
		case "p1", "p2":
			assert.Equal(t, "official", c.SourceOrigin)
			assert.Equal(t, "kb.platform", c.KBVersion)
		case "e1", "e2":
			assert.Equal(t, "external_official", c.SourceOrigin)
			assert.Equal(t, "kb.external", c.KBVersion)
		}
	}
	assert.Equal(t, "qwen3-embedding-8b", sidecar.Model)
	assert.Equal(t, 4, sidecar.Dim)
	assert.Len(t, sidecar.Vectors, 4)
	assert.Equal(t, 4, sidecar.Rows)
	for _, id := range []string{"p1", "p2", "e1", "e2"} {
		assert.Contains(t, sidecar.Vectors, id)
	}
}

// A one-element slice must yield the same chunks + vectors as the single-source
// loader; the ONLY intentional difference is the corpus-level KBVersion label
// ("merged" vs the source's kb_version). This pins the "byte-equivalent for one
// source" claim so Phase 2 can route platform-only loads through this path.
func TestLoadPinnedCorporaSingleSourceEquivalent(t *testing.T) {
	t.Parallel()
	src := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1", "p2")

	direct, err := loadCorpusVerified(src.CorpusPath, src.ExpectedCorpusDigest)
	require.NoError(t, err)

	merged, sidecar, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{src})
	require.NoError(t, err)

	require.Len(t, merged.Chunks, len(direct.Chunks))
	for i := range direct.Chunks {
		assert.Equal(t, direct.Chunks[i].ChunkID, merged.Chunks[i].ChunkID, "chunk order preserved")
		assert.Equal(t, direct.Chunks[i].KBVersion, merged.Chunks[i].KBVersion, "per-chunk kb_version preserved")
	}
	assert.Equal(t, "merged", merged.KBVersion)
	assert.Equal(t, "kb.platform", direct.KBVersion)
	assert.Len(t, sidecar.Vectors, 2)
}

// chunk_id is the retriever's only key (Vectors[chunk.ChunkID]); two sources
// sharing an id would silently shadow one source's chunk/vector. The loader must
// reject the collision loudly instead of merging it away.
func TestLoadPinnedCorporaRejectsCrossSourceDuplicateChunkID(t *testing.T) {
	t.Parallel()
	a := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "dup", "a2")
	b := writePinnedSource(t, "kb.external", "external_official", "qwen3-embedding-8b", 4, "dup", "b2")

	_, _, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{a, b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate chunk_id")
	assert.Contains(t, err.Error(), "across sources")
}

// The retriever assumes a single homogeneous vector space. Mixing embedding
// models (or dims) across sources would make cosine scores meaningless, so it is
// rejected rather than silently merged.
func TestLoadPinnedCorporaRejectsModelMismatch(t *testing.T) {
	t.Parallel()
	a := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1")
	b := writePinnedSource(t, "kb.external", "external_official", "text-embedding-3-large", 4, "e1")

	_, _, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{a, b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedding model mismatch")
}

func TestLoadPinnedCorporaRejectsDimMismatch(t *testing.T) {
	t.Parallel()
	a := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1")
	b := writePinnedSource(t, "kb.external", "external_official", "qwen3-embedding-8b", 8, "e1")

	_, _, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{a, b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedding dim mismatch")
}

// A wrong per-source pin must fail loudly, wrapped with which corpus tripped it.
func TestLoadPinnedCorporaRejectsDigestMismatch(t *testing.T) {
	t.Parallel()
	src := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1")
	src.ExpectedCorpusDigest = "deadbeef"

	_, _, err := LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{src})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "corpus digest mismatch")
	assert.Contains(t, err.Error(), "load corpus")
}

func TestLoadPinnedCorporaRejectsOrphanVector(t *testing.T) {
	t.Parallel()
	src := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1")
	f, err := os.OpenFile(src.EmbeddingsPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"chunk_id":"orphan","vector":[0.1,0.1,0.1,0.1]}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	src.ExpectedEmbeddingDigest, err = ComputeEmbeddingFileDigest(src.EmbeddingsPath)
	require.NoError(t, err)

	_, _, err = LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{src})
	require.ErrorContains(t, err, "orphan vector")
}

func TestLoadPinnedCorporaRejectsMissingVector(t *testing.T) {
	t.Parallel()
	src := writePinnedSource(t, "kb.platform", "official", "qwen3-embedding-8b", 4, "p1", "p2")
	raw, err := os.ReadFile(src.EmbeddingsPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Len(t, lines, 3)
	require.NoError(t, os.WriteFile(src.EmbeddingsPath, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o600))
	src.ExpectedEmbeddingDigest, err = ComputeEmbeddingFileDigest(src.EmbeddingsPath)
	require.NoError(t, err)

	_, _, err = LoadPinnedCorporaWithEmbeddings([]PinnedCorpusSource{src})
	require.ErrorContains(t, err, "missing vector")
}

func TestLoadPinnedCorporaRejectsEmptySources(t *testing.T) {
	t.Parallel()
	_, _, err := LoadPinnedCorporaWithEmbeddings(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no corpus sources provided")
}
