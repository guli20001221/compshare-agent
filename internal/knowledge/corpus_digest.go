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
//
// Bumped 2026-08-14 for the rebuild from compshare-docs @ 0cd491da, which moved
// the docs site from Nextra pages/ to the App Router content/ tree. 544 -> 526
// chunks, fully attributed: -9 for public/action_md/ (a build artifact tree the
// old source glob picked up and the site no longer serves), +8 for five new
// documents, -17 heading-only chunks now dropped by the pipeline. Content
// volume rose 0.4%, so the smaller chunk count is boundaries moving, not text
// leaving.
//
// Every source_path changed in this rebuild, and chunk_id hashes source_path, so
// all 526 ids are new even where the text is identical. That is what made this
// the right moment to raise the embedding cap to 4000 — the whole corpus had to
// re-embed regardless.
const CorpusDigestExpected = "8007f0e64f32ef34a415be97bba0b28ab2d0a3396634bb9977e7f44db54407f3"

// EmbeddingDigestExpected pinned a text-embedding-3-large (3072-dim) sidecar.
//
// It is dead and deliberately left at its old value. No such file exists in
// deploy/kb — it was already absent before this rebuild — and the only reader,
// LoadPinnedCorpusWithEmbeddings, is called from nothing but two tests that
// t.Skipf when the artifact is missing. Retrieval moved to the qwen3 pair
// below, and the production corpus lives in compshare-kb.
//
// Do not "fix" this by regenerating a 3072-dim sidecar: nothing would load it.
// Deleting the constant, the wrapper and the two skipped tests is the correct
// change, kept out of this commit so a release does not carry an unrelated
// deletion.
const EmbeddingDigestExpected = "9dcb902bb6026836b43cf52be159af6690bb4c93818e1b34915f053818b9189c"

// EmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar produced by
// scripts/rag_w0/build_corpus_embeddings.py over the CorpusDigestExpected corpus
// (--embed-model qwen3-embedding-8b, 4096-dim). Offline evaluations select it
// through RetrieverOptions; mismatch means the sidecar is stale relative to the
// corpus and the loader must refuse it.
//
// Rebuilt 2026-08-14 at MAX_CONTENT_RUNES_FOR_EMB = 4000 (raised from 1800).
// The cap is frozen into these vectors: it cannot be changed without rebuilding
// this file, and compshare-kb's retrieval.embeddingContentMaxRunes must read the
// same number, because the update plane embeds newly upserted chunks at runtime
// and they land in this same space.
//
// The vectors are not bit-reproducible. Re-embedding one text on the same model
// and key returns a slightly different vector on a cold call (measured cosine
// 0.999928, max component delta 1.6e-3 — far below anything that reorders a
// result). Rebuilding and diffing is therefore not a way to verify this
// artifact; the digest is the artifact's identity, not a checksum of a
// deterministic function.
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
//
// Bumped 2026-08-14 with the platform corpus. The external sources are a pinned
// zip and did not change; 1200 -> 1189 is entirely pipeline: 10 heading-only
// chunks dropped by the new rule (7 a single heading line, 3 a stack of
// sub-headings with no body) and 2 sections re-split into 1, with every body
// line of all 12 verified still present in the rebuilt corpus. Content volume
// -890 runes (-0.04%), all of it heading text that survives in chunk metadata.
const ExternalCorpusDigestExpected = "74f7356746d3262243dae80ece54a161cb76be77c2c2aa73964d53a3d55113e1"

// ExternalEmbeddingDigestExpectedQwen3 pins the qwen3-embedding-8b sidecar
// (4096-dim) for the external corpus:
// deploy/kb/embeddings_<ExternalCorpusDigestExpected>_qwen3-embedding-8b.jsonl.
// Rebuilt 2026-08-14 at the same 4000-rune cap; see EmbeddingDigestExpectedQwen3
// for what that cap binds and why these vectors are not bit-reproducible.
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
