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

// LoadPinnedCorpusWithEmbeddings loads the corpus + embedding sidecar and
// verifies both against their pinned digests. It also checks that every
// corpus chunk has a matching embedding vector and vice-versa. Any failure
// returns an error; the cmd-layer caller is expected to log.Fatalf on hybrid
// paths so the runtime never serves with a drifted index.
func LoadPinnedCorpusWithEmbeddings(corpusPath, embeddingsPath string) (Corpus, EmbeddingSidecar, error) {
	corpus, err := LoadPinnedCorpus(corpusPath)
	if err != nil {
		return Corpus{}, EmbeddingSidecar{}, err
	}
	digest, err := ComputeEmbeddingFileDigest(embeddingsPath)
	if err != nil {
		return Corpus{}, EmbeddingSidecar{}, err
	}
	if digest != EmbeddingDigestExpected {
		return Corpus{}, EmbeddingSidecar{}, fmt.Errorf("embedding sidecar digest mismatch: got %s want %s", digest, EmbeddingDigestExpected)
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
