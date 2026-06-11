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
// Bumped 2026-06-10 for the A800-无卡 factual hotfix (A800 removed from the
// 无卡开机 supported-card lists across 5 chunks; price/inventory/Spot mentions of
// A800 left intact). The qwen3 sidecar was renamed to this digest but NOT
// re-embedded: vectors are keyed by stable chunk_id (bijection holds), and the
// 5 edited chunks keep their prior-text vectors. That is acceptable here — the
// edit is a tiny same-topic factual correction, retrieval still surfaces the
// chunk, and the CORRECTED content is what gets synthesized/cited. A full
// re-embed folds into the deferred incremental-update rebuild.
const CorpusDigestExpected = "eacdc94141566e22ab978a2c0728d834379f9559cffaca7a7a7f71508f83a2c8"

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
const EmbeddingDigestExpectedQwen3 = "da488ead7fb53b6d7ab2e7529b9724b1a6f60910aeef253028db624c7dcd99b4"

// ExternalCorpusDigestExpected pins deploy/kb/external_w0.jsonl — the separate
// external tool/ops corpus (vLLM / SGLang / Ollama / ComfyUI + GPU
// troubleshooting + Linux-ops/env-management + PyTorch-basics + model-download),
// loaded alongside the platform corpus via LoadPinnedCorporaWithEmbeddings. RAG
// Phase 1 (ComfyUI vertical Phase 5; Linux-ops + PyTorch-basics; then the
// model-download vertical: ModelScope / HF token / Ollama Modelfile / local-path
// serving). Same refuse-to-start-on-mismatch semantics as the platform pin.
const ExternalCorpusDigestExpected = "6058e11b4bb2923a46715c659b8b49061de2177a0980efc9a7b98227cf28892f"

// ExternalEmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar
// (4096-dim) for the external corpus:
// deploy/kb/embeddings_<ExternalCorpusDigestExpected>_qwen3-embedding-8b.jsonl.
const ExternalEmbeddingDigestExpectedQwen3 = "1865a2110500ca7f4617bd9b447987400f1eb196d72e01e9ab8a9a5b1be8eeeb"

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
