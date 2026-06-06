package knowledge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// EmbeddingSidecar holds the precomputed chunk embeddings loaded from
// deploy/kb/embeddings_<corpus_digest>.jsonl. Vectors are keyed by chunk_id
// so the hybrid retriever can look them up regardless of corpus row order.
//
// The sidecar is produced offline by scripts/rag_w0/build_corpus_embeddings.py
// and its content is pinned via EmbeddingDigestExpected; the runtime hybrid
// path refuses to load if the file's LF-normalized sha256 does not match.
type EmbeddingSidecar struct {
	Model   string
	Dim     int
	Rows    int
	Vectors map[string][]float32
}

type embeddingMetaWire struct {
	Meta struct {
		CorpusDigest string `json:"corpus_digest"`
		EmbedModel   string `json:"embed_model"`
		Dim          int    `json:"dim"`
		Rows         int    `json:"rows"`
	} `json:"_meta"`
}

type embeddingRowWire struct {
	ChunkID string    `json:"chunk_id"`
	Vector  []float32 `json:"vector"`
}

// LoadEmbeddingSidecar reads the sidecar file (a JSONL whose first row is a
// `{"_meta": {...}}` header followed by one row per chunk). It returns an
// error if the meta header is missing, dimensions are inconsistent, chunk_id
// is empty, or duplicate chunk_ids appear.
//
// This function does NOT verify the file's digest against
// EmbeddingDigestExpected; that check belongs to LoadPinnedCorpusWithEmbeddings.
func LoadEmbeddingSidecar(path string) (EmbeddingSidecar, error) {
	f, err := os.Open(path)
	if err != nil {
		return EmbeddingSidecar{}, fmt.Errorf("open embedding sidecar: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// vectors up to 3072 floats * ~24 bytes/value -> ~75 KB plus json overhead.
	scanner.Buffer(make([]byte, 1024), 256*1024)

	var sidecar EmbeddingSidecar
	sidecar.Vectors = map[string][]float32{}

	metaSeen := false
	row := 0
	for scanner.Scan() {
		row++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// First non-empty row must be the _meta header.
		if !metaSeen {
			var meta embeddingMetaWire
			if err := json.Unmarshal(line, &meta); err != nil {
				return EmbeddingSidecar{}, fmt.Errorf("row %d: parse meta: %w", row, err)
			}
			if meta.Meta.Dim == 0 && meta.Meta.EmbedModel == "" && meta.Meta.Rows == 0 {
				return EmbeddingSidecar{}, fmt.Errorf("row %d: missing _meta header (got non-meta row first)", row)
			}
			sidecar.Model = meta.Meta.EmbedModel
			sidecar.Dim = meta.Meta.Dim
			sidecar.Rows = meta.Meta.Rows
			metaSeen = true
			continue
		}
		var entry embeddingRowWire
		if err := json.Unmarshal(line, &entry); err != nil {
			return EmbeddingSidecar{}, fmt.Errorf("row %d: parse row: %w", row, err)
		}
		if entry.ChunkID == "" {
			return EmbeddingSidecar{}, fmt.Errorf("row %d: chunk_id is empty", row)
		}
		if sidecar.Dim > 0 && len(entry.Vector) != sidecar.Dim {
			return EmbeddingSidecar{}, fmt.Errorf("row %d: vector dim %d does not match meta dim %d", row, len(entry.Vector), sidecar.Dim)
		}
		if _, exists := sidecar.Vectors[entry.ChunkID]; exists {
			return EmbeddingSidecar{}, fmt.Errorf("row %d: duplicate chunk_id %q", row, entry.ChunkID)
		}
		sidecar.Vectors[entry.ChunkID] = entry.Vector
	}
	if err := scanner.Err(); err != nil {
		return EmbeddingSidecar{}, fmt.Errorf("scan sidecar: %w", err)
	}
	if !metaSeen {
		return EmbeddingSidecar{}, fmt.Errorf("missing _meta header (empty sidecar)")
	}
	if len(sidecar.Vectors) == 0 {
		return EmbeddingSidecar{}, fmt.Errorf("empty sidecar (only meta header)")
	}
	return sidecar, nil
}

// LoadPinnedCorpusWithEmbeddings loads the corpus + text-embedding-3-large
// sidecar (pinned via EmbeddingDigestExpected). Thin wrapper preserved for
// callers that don't need to select between sidecar models.
func LoadPinnedCorpusWithEmbeddings(corpusPath, embeddingsPath string) (Corpus, EmbeddingSidecar, error) {
	return LoadPinnedCorpusWithEmbeddingsDigest(corpusPath, embeddingsPath, EmbeddingDigestExpected)
}

// LoadPinnedCorpusWithEmbeddingsDigest loads the corpus + embedding sidecar
// and verifies both against their pinned digests (corpus against
// CorpusDigestExpected, sidecar against the caller-supplied expectedDigest).
// It also checks that every corpus chunk has a matching embedding vector
// and vice-versa. Any failure returns an error; the cmd-layer caller is
// expected to log.Fatalf on hybrid paths so the runtime never serves with
// a drifted index.
//
// expectedDigest is parameterized so the same loader supports multiple
// sidecar models (text-embedding-3-large via EmbeddingDigestExpected,
// qwen3-embedding-8b via EmbeddingDigestExpectedQwen3, future via new
// constants). The pin is enforced at load time — a sidecar produced by a
// different model with a different digest will fail this check.
func LoadPinnedCorpusWithEmbeddingsDigest(corpusPath, embeddingsPath, expectedDigest string) (Corpus, EmbeddingSidecar, error) {
	return loadPinnedCorpusWithDigests(corpusPath, embeddingsPath, CorpusDigestExpected, expectedDigest)
}

// loadCorpusVerified loads a corpus file after verifying its LF-normalized
// digest against expectedCorpusDigest. It is the per-source primitive behind
// the multi-source loader, which pins each corpus to its own digest (the
// platform and external corpora have different digests).
func loadCorpusVerified(corpusPath, expectedCorpusDigest string) (Corpus, error) {
	digest, err := ComputeCorpusFileDigest(corpusPath)
	if err != nil {
		return Corpus{}, err
	}
	if digest != expectedCorpusDigest {
		return Corpus{}, fmt.Errorf("corpus digest mismatch: got %s want %s", digest, expectedCorpusDigest)
	}
	return LoadCorpus(corpusPath)
}

// loadPinnedCorpusWithDigests loads one corpus + embedding sidecar, verifying
// each against the supplied expected digests and enforcing a strict
// chunk↔vector bijection. It is shared by LoadPinnedCorpusWithEmbeddingsDigest
// (platform, pinned to CorpusDigestExpected) and LoadPinnedCorporaWithEmbeddings
// (each source pinned independently).
func loadPinnedCorpusWithDigests(corpusPath, embeddingsPath, expectedCorpusDigest, expectedEmbeddingDigest string) (Corpus, EmbeddingSidecar, error) {
	corpus, err := loadCorpusVerified(corpusPath, expectedCorpusDigest)
	if err != nil {
		return Corpus{}, EmbeddingSidecar{}, err
	}
	digest, err := ComputeEmbeddingFileDigest(embeddingsPath)
	if err != nil {
		return Corpus{}, EmbeddingSidecar{}, err
	}
	if digest != expectedEmbeddingDigest {
		return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("embedding sidecar digest mismatch: got %s want %s", digest, expectedEmbeddingDigest)
	}
	sidecar, err := LoadEmbeddingSidecar(embeddingsPath)
	if err != nil {
		return Corpus{}, EmbeddingSidecar{}, err
	}
	corpusIDs := make(map[string]struct{}, len(corpus.Chunks))
	for _, c := range corpus.Chunks {
		corpusIDs[c.ChunkID] = struct{}{}
		if _, ok := sidecar.Vectors[c.ChunkID]; !ok {
			return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("embedding sidecar missing vector for chunk %q", c.ChunkID)
		}
	}
	for chunkID := range sidecar.Vectors {
		if _, ok := corpusIDs[chunkID]; !ok {
			return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("embedding sidecar has orphan vector for chunk %q (not in corpus)", chunkID)
		}
	}
	return corpus, sidecar, nil
}

// mergedKBVersion labels the corpus-level KBVersion when
// LoadPinnedCorporaWithEmbeddings concatenates sources whose kb_versions differ
// (platform vs external). Per-chunk KBVersion is preserved on each KBChunk; only
// the corpus-level summary is generalized.
const mergedKBVersion = "merged"

// PinnedCorpusSource identifies one (corpus, sidecar) pair and the digests each
// file is pinned to. It is the input unit for LoadPinnedCorporaWithEmbeddings.
type PinnedCorpusSource struct {
	CorpusPath              string
	EmbeddingsPath          string
	ExpectedCorpusDigest    string
	ExpectedEmbeddingDigest string
}

// LoadPinnedCorporaWithEmbeddings loads several pinned corpus+sidecar sources
// (e.g. the platform FAQ corpus + the external tool/ops corpus) and concatenates
// them into a single in-memory retrieval index. Each source is verified
// independently — its own digest pins plus the strict per-source chunk↔vector
// bijection from loadPinnedCorpusWithDigests. On top of that this enforces two
// cross-source invariants:
//
//   - chunk_id uniqueness across sources. The retriever keys purely by chunk_id
//     (Vectors[chunk.ChunkID]); two sources sharing an id would silently shadow
//     one source's chunk or vector, so a collision is a hard error.
//   - a single embedding model + dim across all sources. The retriever assumes a
//     homogeneous vector space; mixing models/dims is rejected.
//
// The merged corpus KBVersion is mergedKBVersion ("merged") because chunks may
// originate from corpora with different kb_versions. retriever.go is unchanged —
// it consumes the flat merged Corpus/EmbeddingSidecar exactly as the
// single-source path produces them, so a one-element slice is byte-equivalent to
// LoadPinnedCorpusWithEmbeddingsDigest.
func LoadPinnedCorporaWithEmbeddings(sources []PinnedCorpusSource) (Corpus, EmbeddingSidecar, error) {
	if len(sources) == 0 {
		return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("no corpus sources provided")
	}
	merged := Corpus{KBVersion: mergedKBVersion}
	mergedSidecar := EmbeddingSidecar{Vectors: map[string][]float32{}}
	chunkSource := map[string]string{}
	for i, src := range sources {
		corpus, sidecar, err := loadPinnedCorpusWithDigests(src.CorpusPath, src.EmbeddingsPath, src.ExpectedCorpusDigest, src.ExpectedEmbeddingDigest)
		if err != nil {
			return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("load corpus %s: %w", src.CorpusPath, err)
		}
		// The first source seeds the model/dim; every later source must match it.
		// Use the index as the sentinel (not Model=="") so a sidecar with a
		// legitimately empty model is still cross-checked rather than re-seeding.
		if i == 0 {
			mergedSidecar.Model = sidecar.Model
			mergedSidecar.Dim = sidecar.Dim
		} else {
			if sidecar.Model != mergedSidecar.Model {
				return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("embedding model mismatch across sources: %s has %q, want %q", src.EmbeddingsPath, sidecar.Model, mergedSidecar.Model)
			}
			if sidecar.Dim != mergedSidecar.Dim {
				return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("embedding dim mismatch across sources: %s has %d, want %d", src.EmbeddingsPath, sidecar.Dim, mergedSidecar.Dim)
			}
		}
		for _, c := range corpus.Chunks {
			if prev, ok := chunkSource[c.ChunkID]; ok {
				return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("duplicate chunk_id %q across sources (%s and %s)", c.ChunkID, prev, src.CorpusPath)
			}
			chunkSource[c.ChunkID] = src.CorpusPath
			merged.Chunks = append(merged.Chunks, c)
		}
		for id, vec := range sidecar.Vectors {
			mergedSidecar.Vectors[id] = vec
		}
	}
	mergedSidecar.Rows = len(mergedSidecar.Vectors)
	return merged, mergedSidecar, nil
}
