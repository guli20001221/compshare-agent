package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuardSearchKnowledgeSynthesis_GroundedValidator exercises the route-independent
// cite-grounding arm of the agentic-RAG synthesis guard (#126). It proves the
// default-off byte-identity, the cite-or-refuse enforcement when on, the
// anti-fabrication rejection of unknown chunk_ids, marker stripping, and that the
// pre-existing no-raw-leak guard still fires first regardless of the flag.
func TestGuardSearchKnowledgeSynthesis_GroundedValidator(t *testing.T) {
	// Paraphrase-friendly chunk: a >=32-rune verbatim echo leaks; the answers below
	// paraphrase so only the cite contract is exercised.
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: "ext-vllm-oom-001",
		Content: "把 max-model-len 设小一点即可显著降低显存占用，过长的上下文会占用更多 KV cache。",
	}}
	ledger := knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{
		{ChunkID: "ext-vllm-oom-001", Title: "vLLM OOM"},
	}}

	newEng := func() (*Engine, *[]observability.EngineHardBlockTrace) {
		eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
		var traces []observability.EngineHardBlockTrace
		eng.SetHardBlockObserver(func(tr observability.EngineHardBlockTrace) { traces = append(traces, tr) })
		eng.searchKnowledgeRanThisTurn = true
		eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
		eng.searchKnowledgeLedgerThisTurn = ledger
		return eng, &traces
	}

	t.Run("flag off: uncited answer passes unchanged (byte-identical)", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		ans := "可以把 max-model-len 调小来省显存。"
		assert.Equal(t, ans, eng.guardSearchKnowledgeSynthesis(ans), "flag off must not require a citation")
		assert.Empty(t, *traces)
	})

	t.Run("flag on: uncited answer refused + traced", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(true)
		defer SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		ans := "可以把 max-model-len 调小来省显存。"
		assert.Equal(t, ragNoEvidenceReply, eng.guardSearchKnowledgeSynthesis(ans))
		require.Len(t, *traces, 1)
		assert.Equal(t, "search_knowledge_uncited", (*traces)[0].Category)
	})

	t.Run("flag on: cited answer passes with markers stripped", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(true)
		defer SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		got := eng.guardSearchKnowledgeSynthesis("可以把 max-model-len 调小来省显存 [[ext-vllm-oom-001]]。")
		assert.Equal(t, "可以把 max-model-len 调小来省显存。", got)
		assert.NotContains(t, got, "[[")
		assert.Empty(t, *traces)
	})

	t.Run("flag on: explicit canned refusal passes without a citation", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(true)
		defer SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		assert.Equal(t, ragNoEvidenceReply, eng.guardSearchKnowledgeSynthesis(ragNoEvidenceReply))
		assert.Empty(t, *traces, "the canned refusal is allowed to be uncited")
	})

	t.Run("flag on: substantive uncited answer with a hedge phrase is refused (closes the substring-refusal hole)", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(true)
		defer SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		// Carries actionable vLLM flags but cites nothing; the trailing "知识库未覆盖"
		// is a hedge phrase that isKnowledgeRefusal matches by substring. It must NOT
		// be treated as a refusal: cite-exemption is the exact canned reply only.
		ans := "你可以用 vllm serve --max-model-len 4096 来省显存。部分高级参数知识库未覆盖。"
		assert.Equal(t, ragNoEvidenceReply, eng.guardSearchKnowledgeSynthesis(ans),
			"a substantive uncited answer must be refused even though it contains a hedge phrase")
		require.Len(t, *traces, 1)
		assert.Equal(t, "search_knowledge_uncited", (*traces)[0].Category)
	})

	t.Run("flag on: citation to unknown chunk_id is refused (anti-fabrication)", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(true)
		defer SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		assert.Equal(t, ragNoEvidenceReply, eng.guardSearchKnowledgeSynthesis("随便编的结论 [[ext-does-not-exist-999]]。"))
		require.Len(t, *traces, 1)
		assert.Equal(t, "search_knowledge_uncited", (*traces)[0].Category)
	})

	t.Run("flag on: raw leak still refused first (existing guard preserved)", func(t *testing.T) {
		SetGroundedAnswerValidatorEnabled(true)
		defer SetGroundedAnswerValidatorEnabled(false)
		eng, traces := newEng()
		assert.Equal(t, ragNoEvidenceReply, eng.guardSearchKnowledgeSynthesis(hit.Chunk.Content))
		require.Len(t, *traces, 1)
		assert.Equal(t, "search_knowledge_raw_leak", (*traces)[0].Category, "leak check runs before the cite check")
	})
}

// TestSearchKnowledgeResultJSON_CiteProtocolGatedByFlag proves the tool result is
// byte-identical when the validator is off, and carries the cite protocol only when
// on and the ledger is non-empty.
func TestSearchKnowledgeResultJSON_CiteProtocolGatedByFlag(t *testing.T) {
	ledger := knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "ext-a-1", Title: "t"}}}

	off := searchKnowledgeResultJSON(ledger, false, false)
	assert.NotContains(t, off, "cite_protocol", "flag off: no cite protocol")
	assert.Contains(t, off, "ext-a-1")

	on := searchKnowledgeResultJSON(ledger, false, true)
	assert.Contains(t, on, "cite_protocol", "flag on: cite protocol present")
	assert.Contains(t, on, "[[chunk_id]]")

	// Empty ledger never carries the protocol even when the flag is on (nothing to cite).
	emptyOn := searchKnowledgeResultJSON(knowledge.EvidenceLedger{}, true, true)
	assert.NotContains(t, emptyOn, "cite_protocol")
	assert.Contains(t, emptyOn, "\"empty\":true")
}

// TestChatResetsSearchKnowledgeLedgerEachTurn proves the per-turn ChunkID ledger is
// zeroed at the top of every turn, so one turn's accumulated evidence cannot leak
// into the next (the cross-tenant/cross-turn concern engine_session_test.go guards).
func TestChatResetsSearchKnowledgeLedgerEachTurn(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: diagnosisPlanWithoutTarget()}}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "a", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "b", "State": "Running"},
		},
	}, "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	// Simulate a prior turn that accumulated agentic evidence into the per-turn ledger.
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "stale-1"}}}
	_, err := eng.Chat(context.Background(), "我的机器有问题", noopStep)
	require.NoError(t, err)
	assert.Empty(t, eng.searchKnowledgeLedgerThisTurn.Items, "per-turn agentic ledger must be reset at the top of each turn")
}
