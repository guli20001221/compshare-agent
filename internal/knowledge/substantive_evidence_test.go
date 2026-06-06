package knowledge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildSubstantiveEvidenceLedgerCarriesBoundedSnippet pins the P3 resolution
// of the content-free-ledger blocker: the agentic-RAG ledger carries a real,
// bounded content snippet (so a symptom tool-ops turn — where the retrieved
// evidence IS the primary base — can ground an ACTIONABLE answer), while the
// diagnosis-lane ledger (BuildEvidenceLedger) stays deliberately content-free.
func TestBuildSubstantiveEvidenceLedgerCarriesBoundedSnippet(t *testing.T) {
	// Real-shaped external runbook content: the actionable flags live in the head
	// (mirrors deploy/kb/external_w0.jsonl ext-gpu-oom-vllm-001), padded so the
	// body exceeds the snippet cap and the bound is exercised.
	content := "适用场景：模型放不进 GPU 显存时会报 out-of-memory（OOM）。可组合降低显存占用：" +
		"1. 缩短上下文长度：--max-model-len。2. 降低并发：--max-num-seqs。" +
		"3. 多卡张量并行：--tensor-parallel-size。4. 量化：quantization。" +
		strings.Repeat("补充说明，逐步从小到大调整。", 80)
	hits := []RetrievalHit{{
		Kept:  true,
		Score: 0.9,
		Chunk: KBChunk{
			ChunkID:    "ext-gpu-oom-vllm-001",
			Title:      "vLLM 显存不足 (OOM) 排查",
			SourceType: "external",
			Content:    content,
		},
	}}

	sub := BuildSubstantiveEvidenceLedger("vllm 显存不足", hits, DefaultEvidenceLedgerMaxItems, 0)
	if len(sub.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(sub.Items))
	}
	item := sub.Items[0]

	// Substance: the snippet must carry a concrete actionable token from the chunk
	// — without this the synthesizing LLM has nothing to ground a real fix on.
	if !strings.Contains(item.Snippet, "--max-model-len") {
		t.Fatalf("substance gate: snippet missing actionable flag --max-model-len; got %q", item.Snippet)
	}
	// Bounded: the snippet must not exceed the cap (input-bloat guard for the
	// multi-round loop, memory: priortext-avalanche-invalidates-planner).
	if n := utf8.RuneCountInString(item.Snippet); n > DefaultEvidenceSnippetMaxRunes {
		t.Fatalf("snippet %d runes exceeds cap %d", n, DefaultEvidenceSnippetMaxRunes)
	}

	// The diagnosis-lane ledger stays content-free (no regression), while sharing
	// chunk identity with the substantive one (one projection helper, two views).
	plain := BuildEvidenceLedger("vllm 显存不足", hits, DefaultEvidenceLedgerMaxItems)
	if plain.Items[0].Snippet != "" {
		t.Fatalf("BuildEvidenceLedger must stay content-free; got snippet %q", plain.Items[0].Snippet)
	}
	if plain.Items[0].ChunkID != item.ChunkID {
		t.Fatalf("both builders must agree on chunk identity: %q vs %q", plain.Items[0].ChunkID, item.ChunkID)
	}
}
