package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// CorpusDigestExpected pins deploy/kb/stage2b_w0.jsonl after LF normalization.
const CorpusDigestExpected = "8007f0e64f32ef34a415be97bba0b28ab2d0a3396634bb9977e7f44db54407f3"

// EmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar produced by
// scripts/rag_w0/build_corpus_embeddings.py over the CorpusDigestExpected corpus
// at the 4000-rune corpus cap. The digest, rather than reproducibility of remote
// embeddings, is the artifact identity.
const EmbeddingDigestExpectedQwen3 = "40d3a1221b7dca747a266cbad57354ba0b5bd4aa4631222159fd10eb1d0a518e"

// ExternalCorpusDigestExpected pins deploy/kb/external_w0.jsonl — the separate
// external tool/ops corpus. It is intentionally platform-neutral and stable:
// durable GPU/runtime troubleshooting, OpenAI-compatible API semantics, RAG and
// Agent application basics, remote development/service exposure, data transfer,
// security, evaluation, and professional GPU workflows. Volatile platform facts
// such as pricing, model availability, console paths, events, and current
// community-image rankings belong in the internal platform corpus instead.
// Loaded alongside the platform corpus via LoadPinnedCorporaWithEmbeddings. Same
// refuse-to-start-on-mismatch semantics as the platform pin.
const ExternalCorpusDigestExpected = "74f7356746d3262243dae80ece54a161cb76be77c2c2aa73964d53a3d55113e1"

// ExternalEmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar
// (4096-dim) for the external corpus:
// deploy/kb/embeddings_<ExternalCorpusDigestExpected>_qwen3-embedding-8b.jsonl.
// It uses the same model and input cap as the platform corpus.
const ExternalEmbeddingDigestExpectedQwen3 = "180ebdfb71835cf4143e71f0ad69f6b6de0e993ac4e6593ec5f70b3bb1e439ce"

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
