package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// CorpusDigestExpected pins deploy/kb/stage2b_w0.jsonl (LF-normalized SHA256).
// Bumped 2026-07-15 for the RAG V2 source-locked rebuild from compshare-docs
// @ 8a81268 plus the three public FAQ exports. V2 keeps complete short API
// references and operation guides as one chunk, uses caption-only VL image
// evidence, performs no redaction, and emits 544 chunks.
const CorpusDigestExpected = "3da808650d9110427aaf90096ac5b6557b47ec1a1dc1414b399eda6bc567352d"

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
const EmbeddingDigestExpectedQwen3 = "7a46f3fc53d252868fda9fb5f660c66e86802628593968f4e5f8eeea74acc793"

// ExternalCorpusDigestExpected pins deploy/kb/external_w0.jsonl — the separate
// external tool/ops corpus. It is intentionally platform-neutral and stable:
// durable GPU/runtime troubleshooting, OpenAI-compatible API semantics, RAG and
// Agent application basics, remote development/service exposure, data transfer,
// security, evaluation, and professional GPU workflows. Volatile platform facts
// such as pricing, model availability, console paths, events, and current
// community-image rankings belong in the internal platform corpus instead.
// Loaded alongside the platform corpus via LoadPinnedCorporaWithEmbeddings. Same
// refuse-to-start-on-mismatch semantics as the platform pin.
const ExternalCorpusDigestExpected = "6555e3996b1a85dd54e4e6db46295c113b11a99854a24be28ec4085a69936602"

// ExternalEmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar
// (4096-dim) for the external corpus:
// deploy/kb/embeddings_<ExternalCorpusDigestExpected>_qwen3-embedding-8b.jsonl.
const ExternalEmbeddingDigestExpectedQwen3 = "4c7abc6e8dab4ce250d242a87dd5cd7fe71cd7cdef6f6084547a6f8696a488c6"

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
