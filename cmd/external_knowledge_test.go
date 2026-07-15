package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
)

func envFromMap(m map[string]string) getenvFunc {
	return func(k string) string { return m[k] }
}

// DEFAULT ON (2026-06-07): unset/empty/affirmative => on; explicit negative => off;
// unknown => off (logged).
func TestExternalKnowledgeEnabled(t *testing.T) {
	on := []string{"", " ", "1", "true", "TRUE", "yes", "on", " On "}
	for _, v := range on {
		if !externalKnowledgeEnabled(envFromMap(map[string]string{"COMPSHARE_EXTERNAL_KNOWLEDGE": v})) {
			t.Errorf("value %q should enable external knowledge (default-on)", v)
		}
	}
	off := []string{"0", "off", "no", "false", "disabled", "none", "garbage"}
	for _, v := range off {
		if externalKnowledgeEnabled(envFromMap(map[string]string{"COMPSHARE_EXTERNAL_KNOWLEDGE": v})) {
			t.Errorf("value %q should NOT enable external knowledge", v)
		}
	}
}

// DEFAULT ON: with the flag unset the retriever merges the external corpus in qwen3
// modes; COMPSHARE_EXTERNAL_KNOWLEDGE=0 rolls back to platform-only (byte-identical
// to pre-Phase-2). The merge degrades to platform-only on load failure regardless.
func TestExternalKnowledgeSourceDefaultOn(t *testing.T) {
	if _, ok := externalKnowledgeSource(envFromMap(map[string]string{}), knowledge.RetrievalModeQwen3RRF); !ok {
		t.Fatal("external knowledge must be ON by default in qwen3 mode")
	}
	if _, ok := externalKnowledgeSource(envFromMap(map[string]string{"COMPSHARE_EXTERNAL_KNOWLEDGE": "0"}), knowledge.RetrievalModeQwen3RRF); ok {
		t.Fatal("COMPSHARE_EXTERNAL_KNOWLEDGE=0 must roll back to platform-only")
	}
}

func TestExternalKnowledgeSourceEnabledQwen3(t *testing.T) {
	src, ok := externalKnowledgeSource(
		envFromMap(map[string]string{"COMPSHARE_EXTERNAL_KNOWLEDGE": "1"}),
		knowledge.RetrievalModeQwen3RRF,
	)
	if !ok {
		t.Fatal("expected external source when enabled in qwen3_rrf mode")
	}
	if src.CorpusPath != defaultExternalKnowledgeCorpusPath {
		t.Errorf("corpus path = %q, want default %q", src.CorpusPath, defaultExternalKnowledgeCorpusPath)
	}
	if src.ExpectedCorpusDigest != knowledge.ExternalCorpusDigestExpected {
		t.Errorf("corpus digest = %q, want ExternalCorpusDigestExpected", src.ExpectedCorpusDigest)
	}
	if src.ExpectedEmbeddingDigest != knowledge.ExternalEmbeddingDigestExpectedQwen3 {
		t.Errorf("embedding digest = %q, want ExternalEmbeddingDigestExpectedQwen3", src.ExpectedEmbeddingDigest)
	}
	wantSuffix := "embeddings_" + knowledge.ExternalCorpusDigestExpected + "_qwen3-embedding-8b.jsonl"
	if !strings.HasSuffix(src.EmbeddingsPath, wantSuffix) {
		t.Errorf("embeddings path = %q, want suffix %q", src.EmbeddingsPath, wantSuffix)
	}
}

// External ships only a qwen3 sidecar, so a non-qwen3 mode must skip it rather
// than try to merge an incompatible vector space.
func TestExternalKnowledgeSourceNonQwen3Skips(t *testing.T) {
	for _, mode := range []string{
		knowledge.RetrievalModeBM25Only,
		knowledge.RetrievalModeHybridCosine,
		knowledge.RetrievalModeHybridRerank,
	} {
		if _, ok := externalKnowledgeSource(
			envFromMap(map[string]string{"COMPSHARE_EXTERNAL_KNOWLEDGE": "1"}),
			mode,
		); ok {
			t.Errorf("mode %s should skip external (qwen3-sidecar only)", mode)
		}
	}
}

// TestLoadKnowledgeCorporaMergeAndDegrade exercises the cmd-layer load logic
// against the real pinned V2 corpora: OFF = platform-only, ON = merged, and
// ON + broken external path gracefully falls back to platform-only.
func TestLoadKnowledgeCorporaMergeAndDegrade(t *testing.T) {
	platformCorpus := filepath.Join("..", "deploy", "kb", "stage2b_w0.jsonl")
	platformSidecar := filepath.Join("..", "deploy", "kb", "embeddings_"+knowledge.CorpusDigestExpected+"_qwen3-embedding-8b.jsonl")
	extCorpus := filepath.Join("..", "deploy", "kb", "external_w0.jsonl")
	extSidecar := filepath.Join("..", "deploy", "kb", "embeddings_"+knowledge.ExternalCorpusDigestExpected+"_qwen3-embedding-8b.jsonl")
	for _, p := range []string{platformCorpus, platformSidecar, extCorpus, extSidecar} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing %s (%v); skipping integration check", p, err)
		}
	}
	mode := knowledge.RetrievalModeQwen3RRF
	digest := knowledge.EmbeddingDigestExpectedQwen3

	// OFF -> platform-only.
	corpus, _, err := loadKnowledgeCorpora(envFromMap(map[string]string{}), mode, platformCorpus, platformSidecar, digest)
	if err != nil {
		t.Fatalf("off-path load: %v", err)
	}
	if len(corpus.Chunks) != 562 {
		t.Fatalf("external off: got %d chunks, want 562 (platform-only)", len(corpus.Chunks))
	}

	onEnv := envFromMap(map[string]string{
		"COMPSHARE_EXTERNAL_KNOWLEDGE":            "1",
		"COMPSHARE_EXTERNAL_KNOWLEDGE_CORPUS":     extCorpus,
		"COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS": extSidecar,
	})
	merged, _, err := loadKnowledgeCorpora(onEnv, mode, platformCorpus, platformSidecar, digest)
	if err != nil {
		t.Fatalf("on-path merge load: %v", err)
	}
	if len(merged.Chunks) != 562+1118 {
		t.Fatalf("external on: got %d chunks, want 1680 (merged)", len(merged.Chunks))
	}

	// ON but the external file is missing -> graceful fall back to platform-only.
	badEnv := envFromMap(map[string]string{
		"COMPSHARE_EXTERNAL_KNOWLEDGE":            "1",
		"COMPSHARE_EXTERNAL_KNOWLEDGE_CORPUS":     filepath.Join("..", "deploy", "kb", "does_not_exist.jsonl"),
		"COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS": extSidecar,
	})
	degraded, _, err := loadKnowledgeCorpora(badEnv, mode, platformCorpus, platformSidecar, digest)
	if err != nil {
		t.Fatalf("degrade load should not error (external is additive): %v", err)
	}
	if len(degraded.Chunks) != 562 {
		t.Fatalf("graceful degrade: got %d chunks, want 562 (platform-only fallback)", len(degraded.Chunks))
	}
}

func TestExternalKnowledgeSourcePathOverrides(t *testing.T) {
	src, ok := externalKnowledgeSource(
		envFromMap(map[string]string{
			"COMPSHARE_EXTERNAL_KNOWLEDGE":            "1",
			"COMPSHARE_EXTERNAL_KNOWLEDGE_CORPUS":     "/tmp/custom_ext.jsonl",
			"COMPSHARE_EXTERNAL_KNOWLEDGE_EMBEDDINGS": "/tmp/custom_ext_vecs.jsonl",
		}),
		knowledge.RetrievalModeQwen3Full,
	)
	if !ok {
		t.Fatal("expected external source when enabled")
	}
	if src.CorpusPath != "/tmp/custom_ext.jsonl" {
		t.Errorf("corpus path override not honored: %q", src.CorpusPath)
	}
	if src.EmbeddingsPath != "/tmp/custom_ext_vecs.jsonl" {
		t.Errorf("embeddings path override not honored: %q", src.EmbeddingsPath)
	}
}
