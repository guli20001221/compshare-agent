package engine

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sanitizedContextRAGRecord struct {
	Name             string `json:"name"`
	UserMessage      string `json:"user_message"`
	ResolvedQuestion string `json:"resolved_question"`
	ChunkID          string `json:"chunk_id"`
	Evidence         string `json:"evidence"`
	Answer           string `json:"answer"`
	AnswerQuote      string `json:"answer_quote"`
	EvidenceQuote    string `json:"evidence_quote"`
}

func loadSanitizedContextRAGRecords(t *testing.T) []sanitizedContextRAGRecord {
	t.Helper()
	b, err := os.ReadFile("testdata/sanitized_context_rag_records.json")
	require.NoError(t, err)
	var records []sanitizedContextRAGRecord
	require.NoError(t, json.Unmarshal(b, &records))
	require.Len(t, records, 5)
	return records
}

func TestSanitizedContextRAGRecordsContainNoCustomerIdentifiers(t *testing.T) {
	b, err := os.ReadFile("testdata/sanitized_context_rag_records.json")
	require.NoError(t, err)
	sensitive := regexp.MustCompile(`(?i)(uhost-[a-z0-9]|session[_-]?id|conversation[_-]?id|user[_-]?id|tenant[_-]?id|account[_-]?id|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}|\b(?:\d{1,3}\.){3}\d{1,3}\b|https?://|access[_-]?key|secret|password|token)`)
	assert.Empty(t, sensitive.Find(b), "sanitized real-record fixtures must not carry customer or operational identifiers")
}

// syntheticGroundedEngine wires an agent-loop turn that already ran SearchKnowledge
// and kept one hit (chunk id "kb-port-001").
func syntheticGroundedEngine(t *testing.T) (*Engine, *mockLLM) {
	t.Helper()
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: "kb-port-001", KBVersion: "test.fixture", Title: "端口说明",
		Content: "防火墙端口在默认情况下处于关闭状态，需要在控制台手动放通。",
	}}
	mock := &mockLLM{}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("端口默认状态", []knowledge.RetrievalHit{hit}, 3, 0)
	require.NotEmpty(t, eng.searchKnowledgeLedgerThisTurn.Items, "precondition: the kept hit produced a ledger item")
	return eng, mock
}

// A validly-cited answer is accepted deterministically: markers stripped, no second
// model, and the pure-RAG answer is remembered.
func TestKnowledgeGrounding_ValidCitationAccepted(t *testing.T) {
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "端口默认状态", "防火墙端口默认是关闭的[[kb-port-001]]。")
	assert.Equal(t, "防火墙端口默认是关闭的。", got, "the marker is stripped; the prose ships")
	require.Empty(t, mock.calls, "a validly-cited answer is accepted without any retry or verifier call")
	require.NotEmpty(t, eng.sessionState.VerifiedKnowledge, "a cited pure-RAG answer is remembered")
	require.Equal(t, groundingSupported, eng.groundingOutcomeThisTurn)
}

// Positional [n] citations resolve the same way.
func TestKnowledgeGrounding_PositionalCitationAccepted(t *testing.T) {
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "端口默认状态", "防火墙端口默认是关闭的[1]。")
	assert.Equal(t, "防火墙端口默认是关闭的。", got)
	require.Empty(t, mock.calls)
}

// Citation typography cannot trigger a second model call that rewrites a correct
// answer. The original prose ships and is not promoted to verified memory.
func TestKnowledgeGrounding_UncitedAnswerShipsWithoutRewrite(t *testing.T) {
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "端口默认状态", "防火墙端口默认是关闭的。")
	assert.Equal(t, "防火墙端口默认是关闭的。", got)
	require.Empty(t, mock.calls, "citation formatting must never rewrite user-facing prose")
	require.Empty(t, eng.sessionState.VerifiedKnowledge)
	require.Equal(t, groundingUnavailable, eng.groundingOutcomeThisTurn)
}

// THE core fail-open contract: an answer that is correct and evidence-backed but
// cites the WRONG chunk id (an LLM mis-fill) must NOT be destroyed. The bad marker
// is stripped and the original correct answer ships. Historically this exact case blocked 0/50 correct
// production answers with a canned "知识库未覆盖" refusal.
func TestKnowledgeGrounding_WrongChunkIdShipsNotDestroyed(t *testing.T) {
	answer := "防火墙端口默认是关闭的[[not-the-real-chunk-id]]。"
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "端口默认状态", answer)
	assert.Equal(t, "防火墙端口默认是关闭的。", got, "the correct answer survives; the fabricated marker is stripped")
	assert.NotContains(t, got, "[[", "no fabricated marker reaches the user")
	assert.NotContains(t, got, "知识库未覆盖", "a correct answer is never replaced by a canned refusal")
	require.Empty(t, mock.calls, "invalid citation syntax is stripped without regenerating the answer")
	require.Equal(t, groundingUnavailable, eng.groundingOutcomeThisTurn)
}

func TestKnowledgeGrounding_MixedKnownAndUnknownCitationsAreNotRecordedAsGrounded(t *testing.T) {
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(
		context.Background(),
		"端口默认状态",
		"防火墙端口默认是关闭的[[kb-port-001]]，更多说明见[[made-up-id]]。",
	)
	assert.Equal(t, "防火墙端口默认是关闭的，更多说明见。", got)
	require.Empty(t, mock.calls)
	require.Empty(t, eng.sessionState.VerifiedKnowledge,
		"a partly fabricated citation set may ship fail-open but must never become verified memory")
	require.Equal(t, groundingUnavailable, eng.groundingOutcomeThisTurn)
}

// A fully uncited answer ships fail-open without a second model call.
func TestKnowledgeGrounding_UncitedFailOpenShips(t *testing.T) {
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "端口默认状态", "防火墙端口默认是关闭的。")
	assert.Equal(t, "防火墙端口默认是关闭的。", got)
	assert.NotContains(t, got, "知识库未覆盖")
	require.Empty(t, mock.calls)
	require.Empty(t, eng.sessionState.VerifiedKnowledge, "an uncited answer is shipped but not remembered as verified")
}

// Under typography-only grounding the semantic verifier is deliberately gone: a
// fabricated claim carrying a VALID citation ships. This is the accepted residual
// risk documented in the convergence plan (P1c: pure marker-validity cannot catch
// 断章取义). Encoded so the trade-off is explicit and any future re-introduction of a
// semantic gate is a conscious decision, not an accident.
func TestKnowledgeGrounding_CitedFabricationShips_AcceptedResidualRisk(t *testing.T) {
	eng, mock := syntheticGroundedEngine(t)
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "端口默认状态", "所有端口默认都是开放的[[kb-port-001]]。")
	assert.Contains(t, got, "所有端口默认都是开放的", "typography-only ships a validly-cited claim; content is not re-judged")
	assert.NotContains(t, got, "[[")
	require.Empty(t, mock.calls, "no second model reviews the answer")
}

// No evidence in hand (the Agent answered stable general knowledge directly): the
// answer ships untouched and the runtime never forces a retrieval or retry.
func TestKnowledgeGrounding_NoEvidenceShipsStableGeneral(t *testing.T) {
	mock := &mockLLM{}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true // ran, but retrieved nothing
	answer := "在常见 Linux 终端中可使用 Ctrl+Shift+V 粘贴。"
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "Linux 终端怎么粘贴", answer)
	assert.Equal(t, answer, got)
	require.Empty(t, mock.calls, "an empty ledger has nothing to cite; no retry is forced")
}

// An irrelevant retrieval hit must not erase a correct stable-general answer, and
// the answer must not be promoted into durable verified memory against unrelated
// evidence.
func TestKnowledgeGrounding_IrrelevantSearchDoesNotEraseStableAnswer(t *testing.T) {
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: "kb-billing-001", KBVersion: "test.fixture", Title: "计费说明", Content: "实例按量计费。",
	}}
	answer := "在常见 Linux 终端中可使用 Ctrl+Shift+V 粘贴。"
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("Linux 终端怎么粘贴", []knowledge.RetrievalHit{hit}, 3, 0)

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "粘贴呢", answer)
	assert.Equal(t, answer, got)
	require.Empty(t, eng.sessionState.VerifiedKnowledge, "general knowledge must not be persisted against unrelated retrieval evidence")
}

// A raw-evidence leak cannot ship verbatim evidence: it is stopped immediately,
// never sent through a rewrite call. Uses a real sanitized record so the
// leak needle is real evidence text.
func TestKnowledgeGrounding_RawLeakBlockedWithoutRewrite(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[2]
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: record.ChunkID, KBVersion: "sanitized.prod.fixture", Title: record.Name, Content: record.Evidence,
	}}
	leak := "资料原文：" + record.Evidence
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: leak}}}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger(record.ResolvedQuestion, []knowledge.RetrievalHit{hit}, 3, 0)

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, leak)
	assert.Equal(t, ragUngroundableReply, got, "a persistent verbatim dump is a security stop, not shipped")
	assert.NotContains(t, got, record.Evidence)
	require.Empty(t, eng.llmClient.(*mockLLM).calls, "raw evidence is blocked, not handed to a rewrite prompt")
}

// Outside the knowledge_qa agent loop the finalizer is a pass-through: it must not
// touch a normal answer.
func TestKnowledgeGrounding_NotAgentLoopIsPassThrough(t *testing.T) {
	mock := &mockLLM{}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = false
	answer := "任意回答，可能带 [[fake]] 标记。"
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "q", answer)
	assert.Equal(t, answer, got)
	require.Empty(t, mock.calls)
}

// Wiring gate: drive the real production agent loop end-to-end. Removing the
// finalizeAgentLoopKnowledgeAnswer call from engine.go would let a fabricated
// [[chunk_id]] marker reach the user un-stripped and make this red.
func TestKnowledgeGrounding_ProductionAgentLoopWiresGate(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[0]
	chunk := knowledge.KBChunk{ChunkID: record.ChunkID, KBVersion: "sanitized.prod.fixture", Title: record.Name, Content: record.Evidence}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: chunk.KBVersion, Hits: []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	answer := "该问题的答复见资料[[" + record.ChunkID + "]]。"
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{ID: "search", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"` + record.ResolvedQuestion + `"}`}}}},
		{Content: answer},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test")
	eng.SetKnowledgeRetriever(retriever)

	got, err := eng.Chat(context.Background(), record.UserMessage, noopStep)
	require.NoError(t, err)
	assert.NotContains(t, got, "[[", "the citation marker is stripped by the production final gate")
	assert.Contains(t, got, "该问题的答复见资料")
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, record.ResolvedQuestion, retriever.calls[0].question)
}
