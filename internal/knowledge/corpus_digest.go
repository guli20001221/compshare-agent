package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const CorpusDigestExpected = "d97a074a18eb4ec8ac30d26c8901514b29c6bf30e9bd13d0a64484ea3691806a"

// EmbeddingDigestExpected pins the hybrid retrieval embedding sidecar produced by
// scripts/rag_w0/build_corpus_embeddings.py over the CorpusDigestExpected corpus
// with text-embedding-3-large (3072-dim). Mismatch indicates the sidecar is
// stale relative to the deployed corpus and RAG hybrid path must refuse to load.
const EmbeddingDigestExpected = "9dcb902bb6026836b43cf52be159af6690bb4c93818e1b34915f053818b9189c"

// EmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar produced by
// the same script over the CorpusDigestExpected corpus (--embed-model
// qwen3-embedding-8b, 4096-dim default). Selected only when
// RAG_RETRIEVAL_MODE=qwen3_full; the text-emb-3 sidecar above remains the
// default for hybrid_cosine / hybrid_rerank modes. Same mismatch semantics
// as EmbeddingDigestExpected: stale sidecar = hybrid path refuses to load.
const EmbeddingDigestExpectedQwen3 = "f91aef22b4082bd9b7a58c59348c8e16028f93aef57c015a8479016a61f0abac"

// ComputeCorpusDigest normalizes line endings so the pinned corpus digest is
// stable across Windows and Unix checkouts.
func ComputeCorpusDigest(reader io.Reader) (string, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("compute corpus digest: %w", err)
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	hash := sha256.New()
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ComputeCorpusFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open corpus for digest: %w", err)
	}
	defer file.Close()
	return ComputeCorpusDigest(file)
}

// ComputeEmbeddingFileDigest mirrors ComputeCorpusFileDigest semantics so the
// embedding sidecar pin is byte-stable across CRLF/LF checkouts.
func ComputeEmbeddingFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open embedding sidecar for digest: %w", err)
	}
	defer file.Close()
	return ComputeCorpusDigest(file)
}

func LoadPinnedCorpus(path string) (Corpus, error) {
	digest, err := ComputeCorpusFileDigest(path)
	if err != nil {
		return Corpus{}, err
	}
	if digest != CorpusDigestExpected {
		return Corpus{}, fmt.Errorf("corpus digest mismatch: got %s want %s", digest, CorpusDigestExpected)
	}
	return LoadCorpus(path)
}
