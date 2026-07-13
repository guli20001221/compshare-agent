package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lead hand-labelled a replayed production turn:
//
//	user: 「我关机之后，实例里的数据会保存吗」
//	bot : 「当前知识库未覆盖该问题,我无法回答。」
//	lead: 「chunk 里面的内容能否回答？」
//
// It can. deploy/kb/stage2b_w0.jsonl carries it verbatim: 按量…关机后 CPU、GPU 和内存会被
// 释放，实例不再收费，额外扩容的系统盘和数据盘及镜像资源会保留并继续收费. The agent
// retrieved that chunk, read it, and wrote a correct answer — and we deleted the answer and
// told the user the knowledge base does not cover the question.
//
// That message is not a hedge, it is a FALSE STATEMENT, and the 1454-turn replay in
// docs/plans/2026-07-13-program-a-amnesia-fix-results.md measured how often it is false:
// of every KB refusal shown, the honest no-evidence arm (retrieval genuinely returned zero
// hits) fired ZERO times. Every single one came from a guard destroying an answer that was
// written from evidence we had in hand.
//
// The two guard arms fail for reasons that are, on their face, not reasons to refuse:
//
//   - uncited: the answer carries no resolvable [[chunk_id]]. But the citation is STRIPPED
//     before display (knowledge.StripCiteMarkers) — the user never sees it. It is a
//     grounding proof, and flash omits it nondeterministically. A correct answer is
//     destroyed for a missing bracket.
//   - raw-leak: the answer quoted a chunk too literally. That is the answer being MORE
//     faithful to the evidence, not less — and we respond by claiming the evidence does not
//     exist.
//
// These tests pin the INVARIANT, not the mechanism: when SearchKnowledge put evidence in
// the agent's hands, the turn must never end by telling the user the knowledge base has
// nothing. The check stays; the FAILURE ACTION is what was wrong. Repair the answer from
// the evidence ledger (synthesizeKnowledgeQAFromLedger — which cite-validates and
// leak-strips by construction) instead of destroying it.
//
// Deliberately NOT asserted here: that the repaired answer carries a citation. Citations are
// invisible to the user; asserting one would re-pin the proxy this bug is made of.
func TestKnowledgeGuard_EvidenceInHandNeverEndsAsNoCoverage(t *testing.T) {
	// The real chunk, verbatim from deploy/kb/stage2b_w0.jsonl.
	const shutdownChunk = "按量：按小时计费的后付费模式，关机后CPU、GPU和内存会被释放，实例不再收费，额外扩容的系统盘和数据盘及镜像资源会保留并继续收费；"
	const chunkID = "kb-billing-shutdown-001"

	// Title and KBVersion are load-bearing, not decoration: envelope.NewEvidence REJECTS a
	// chunk missing either, and the rejection makes the repair rung fail silently. Real KB
	// chunks carry both.
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID:   chunkID,
		Title:     "计费模式说明",
		Content:   shutdownChunk,
		KBVersion: "w0",
	}}
	ledger := knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{
		{ChunkID: chunkID, Title: "计费模式说明"},
	}}

	// A correct, grounded answer the model actually produced from that chunk — and which
	// the guard destroys. It is worth reading: it is RIGHT.
	const goodAnswer = "按量计费的实例关机后，CPU、GPU 和内存会释放且不再收费；但系统盘、数据盘和镜像会保留，并继续按存储计费，所以数据不会丢失。"

	// The mock's two replies, in the order the repair ladder elicits them. This sequence is
	// what makes the test RED on the pre-fix code rather than passing vacuously:
	//
	//	rung 1 (retrySearchKnowledgeCitation) — re-asks the SAME model to please remember a
	//	  bracket. It fails again, because that is what the production replay measured it
	//	  doing. If the mock let rung 1 succeed, the test would go green on the old code and
	//	  gate nothing.
	//	rung 2 (synthesizeKnowledgeQAFromLedger, NEW) — re-derives the answer FROM the
	//	  ledger and cites it. This is the rung that actually lands.
	uncitedRetry := goodAnswer // still no citation: rung 1 fails exactly as it does live
	citedResynthesis := goodAnswer + "[1]"

	cases := []struct {
		name string
		// answer is what the agent wrote before the guard saw it.
		answer string
		// why records which guard arm this trips, so a future reader knows the case is not
		// redundant with its sibling.
		why string
	}{
		{
			name:   "uncited answer (flash dropped the bracket)",
			answer: goodAnswer,
			why:    "search_knowledge_uncited — grounded, correct, no [[chunk_id]]",
		},
		{
			name: "answer quotes the chunk verbatim",
			// >=32 runes lifted straight from the chunk: the raw-leak arm.
			answer: "关于关机计费：" + shutdownChunk,
			why:    "search_knowledge_raw_leak — MORE faithful to the evidence, not less",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Production ships this on (CLAUDE.md: default-on since 2026-06-09); the Go
			// default is off, so the test must opt in exactly as the binary does.
			SetDisciplinedKnowledgeQASynthesisEnabled(true)
			defer SetDisciplinedKnowledgeQASynthesisEnabled(false)

			eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
				{Content: uncitedRetry},     // rung 1 fails, as it does in production
				{Content: citedResynthesis}, // rung 2 re-derives from the ledger and cites
			}}, &mockExecutor{}, nil)
			eng.searchKnowledgeRanThisTurn = true
			eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
			eng.searchKnowledgeLedgerThisTurn = ledger
			// This is a knowledge_qa turn routed into the agent loop — production's default
			// path (COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP, on since 2026-06-09). It is what arms
			// the cite-or-refuse arm of the guard; without it the uncited case would pass
			// VACUOUSLY, never reaching the guard at all, and gate nothing.
			eng.knowledgeQAAgentLoopThisTurn = true

			got := eng.repairOrRefuseKnowledgeSynthesis(
				context.Background(),
				"我关机之后，实例里的数据会保存吗",
				tc.answer,
			)

			// THE INVARIANT. Evidence was in hand; "the knowledge base does not cover this"
			// is a lie, and it is the single thing this turn must never say.
			assert.NotEqual(t, ragNoEvidenceReply, strings.TrimSpace(got),
				"%s: the KB DID cover this — refusing is a false statement to the user", tc.why)
			assert.NotContains(t, got, "知识库未覆盖",
				"%s: no phrasing of 'not covered' is truthful when the chunk is in the ledger", tc.why)

			// And the answer must still be substantive — not merely a non-refusal. A guard
			// that "passes" by emitting an empty string would satisfy the assertions above.
			require.NotEmpty(t, strings.TrimSpace(got), "repair must produce an answer, not silence")
			assert.Contains(t, got, "数据盘",
				"the repaired answer must still carry the substance of the retrieved evidence")
			// The citation is a grounding proof, never user-facing text.
			assert.NotContains(t, got, "[[", "cite markers must be stripped before display")
		})
	}
}

// TestKnowledgeGuard_HonestNoEvidenceStillRefuses is the other half of the contract, and it
// is what stops the fix above from becoming "never refuse". When retrieval genuinely
// returned nothing, the agent has no evidence, and 「知识库未覆盖」 is TRUE — the only place
// in the codebase where that sentence is honest. The repair rung must not fire there: there
// is no ledger to repair from, and inventing an answer would be the exact fabrication the
// guard exists to prevent.
func TestKnowledgeGuard_HonestNoEvidenceStillRefuses(t *testing.T) {
	SetDisciplinedKnowledgeQASynthesisEnabled(true)
	defer SetDisciplinedKnowledgeQASynthesisEnabled(false)

	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "这里不该被调用"},
	}}, &mockExecutor{}, nil)
	// SearchKnowledge ran, and the relevance floor dropped every hit: an EMPTY ledger.
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = nil
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{}

	// The canned refusal, arriving with no evidence behind it, must survive untouched.
	got := eng.repairOrRefuseKnowledgeSynthesis(context.Background(), "海王星上能不能开实例", ragNoEvidenceReply)
	assert.Equal(t, ragNoEvidenceReply, got,
		"with a genuinely empty ledger the refusal is honest and must stand — repair needs evidence to repair FROM")
}
