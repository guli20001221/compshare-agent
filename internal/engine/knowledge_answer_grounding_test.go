package engine

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
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

func enableDisciplinedKnowledgeRepair(t *testing.T) {
	t.Helper()
	previous := DisciplinedKnowledgeQASynthesisEnabled()
	SetDisciplinedKnowledgeQASynthesisEnabled(true)
	t.Cleanup(func() { SetDisciplinedKnowledgeQASynthesisEnabled(previous) })
}

func enableKnowledgeAnswerVerifier(t *testing.T) {
	t.Helper()
	previous := KnowledgeAnswerVerifierEnabled()
	SetKnowledgeAnswerVerifierEnabled(true)
	t.Cleanup(func() { SetKnowledgeAnswerVerifierEnabled(previous) })
}

func TestKnowledgeAnswerVerifierDisabledFailsClosed(t *testing.T) {
	previous := KnowledgeAnswerVerifierEnabled()
	SetKnowledgeAnswerVerifierEnabled(false)
	t.Cleanup(func() { SetKnowledgeAnswerVerifierEnabled(previous) })

	record := loadSanitizedContextRAGRecords(t)[0]
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: record.ChunkID, KBVersion: "sanitized.prod.fixture", Content: record.Evidence,
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "must not be called"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger(record.ResolvedQuestion, []knowledge.RetrievalHit{hit}, 3, 0)

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, record.Answer)

	assert.Equal(t, ragUngroundableReply, got)
	assert.Empty(t, mock.calls, "disabled verifier must refuse rather than release an unchecked answer")
}

func groundedEngineForRecord(t *testing.T, record sanitizedContextRAGRecord, responses ...llm.ChatResponse) (*Engine, *mockLLM) {
	t.Helper()
	enableKnowledgeAnswerVerifier(t)
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: record.ChunkID, KBVersion: "sanitized.prod.fixture", Title: record.Name, Content: record.Evidence,
	}}
	mock := &mockLLM{responses: responses}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger(record.ResolvedQuestion, []knowledge.RetrievalHit{hit}, 3, 0)
	return eng, mock
}

func groundingVerdictResponse(record sanitizedContextRAGRecord, supported bool, usage int) llm.ChatResponse {
	verdict := knowledgeGroundingVerdict{Supported: supported}
	if supported {
		verdict.Claims = []knowledgeGroundingClaim{{
			AnswerQuote: record.AnswerQuote, ChunkID: record.ChunkID, EvidenceQuote: record.EvidenceQuote,
		}}
	} else {
		verdict.Unsupported = []string{"证据未支持的主张"}
	}
	b, _ := json.Marshal(verdict)
	return llm.ChatResponse{Content: string(b), Usage: llm.TokenUsage{TotalTokens: usage}}
}

func repairResponse(record sanitizedContextRAGRecord, usage int) llm.ChatResponse {
	repaired := map[string]any{
		"answer":    record.Answer,
		"supported": true,
		"claims": []knowledgeGroundingClaim{{
			AnswerQuote: record.AnswerQuote, ChunkID: record.ChunkID, EvidenceQuote: record.EvidenceQuote,
		}},
		"unsupported": []string{},
	}
	b, _ := json.Marshal(repaired)
	return llm.ChatResponse{Content: string(b), Usage: llm.TokenUsage{TotalTokens: usage}}
}

func assertResolvedQuestionContract(t *testing.T, calls []llm.ChatRequest, resolved string) {
	t.Helper()
	require.NotEmpty(t, calls)
	for i, call := range calls {
		require.Len(t, call.Messages, 2, "grounding call %d must be isolated from ReAct history", i)
		payload := call.Messages[1].Content
		assert.Contains(t, payload, `"resolved_question":"`+resolved+`"`, "call %d used a different resolved question", i)
		assert.Contains(t, payload, `"query":"`+resolved+`"`, "call %d did not carry the matching EvidenceLedger.query", i)
	}
}

// Five sanitized real-record shapes, including the two-character follow-up
// "粘贴呢". The fallback utterance must never replace the history-aware retrieval
// query during validation.
func TestKnowledgeAnswerGrounding_SanitizedContextRecordsUseOneResolvedQuestion(t *testing.T) {
	for _, record := range loadSanitizedContextRAGRecords(t) {
		t.Run(record.Name, func(t *testing.T) {
			eng, mock := groundedEngineForRecord(t, record, groundingVerdictResponse(record, true, 100))

			got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, record.Answer)

			assert.Equal(t, record.Answer, got)
			assertResolvedQuestionContract(t, mock.calls, record.ResolvedQuestion)
			if record.UserMessage == "粘贴呢" {
				assert.NotContains(t, mock.calls[0].Messages[1].Content, `"resolved_question":"粘贴呢"`)
			}
		})
	}
}

// A valid citation is attribution, not proof. This is the hole the earlier
// grounding-not-punctuation branch explicitly left open.
func TestKnowledgeAnswerGrounding_CitedFabricationAlsoReachesSemanticGate(t *testing.T) {
	enableDisciplinedKnowledgeRepair(t)
	record := loadSanitizedContextRAGRecords(t)[3]
	fabrication := "所有订单都能在 24 小时内全额退款 [[" + record.ChunkID + "]]。"
	eng, mock := groundedEngineForRecord(t, record,
		groundingVerdictResponse(record, false, 100),
		repairResponse(record, 100),
	)

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, fabrication)

	assert.Equal(t, record.Answer, got, "the cited fabrication must be replaced by a proved ledger answer")
	assert.NotContains(t, got, "24 小时")
	require.Len(t, mock.calls, 2, "cited text must be verified, then repaired; it cannot bypass on punctuation")
	assert.Contains(t, mock.calls[0].Messages[0].Content, "事实核查员")
	assertResolvedQuestionContract(t, mock.calls, record.ResolvedQuestion)
}

func TestKnowledgeAnswerGrounding_UncitedGroundedAnswerIsNotFalseRefused(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[0]
	eng, mock := groundedEngineForRecord(t, record, groundingVerdictResponse(record, true, 100))

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, record.Answer)

	assert.Equal(t, record.Answer, got)
	assert.NotContains(t, got, "知识库未覆盖")
	require.Len(t, mock.calls, 1, "verify the answer in hand; do not re-roll merely to chase brackets")
}

func TestKnowledgeAnswerGrounding_NoEvidenceCannotPass(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "不应调用"}}}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "没有资料的问题", "凭常识猜一个答案")

	assert.Equal(t, ragNoEvidenceReply, got)
	assert.Empty(t, eng.llmClient.(*mockLLM).calls, "empty evidence must fail before another model call")
}

func TestKnowledgeAnswerGrounding_EmptyCurrentSearchDoesNotErasePriorAnswer(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: `{"supported":true,"claims":[{"answer_quote":"使用 Ctrl+Shift+V 粘贴","chunk_id":"terminal-paste-001","evidence_quote":"使用 Ctrl+Shift+V 粘贴"}],"unsupported":[]}`}}}, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.sessionState.VerifiedKnowledge = []VerifiedKnowledgeTurn{{
		Question: "Windows Terminal 怎么粘贴？",
		Answer:   "使用 Ctrl+Shift+V 粘贴。",
		Evidence: knowledge.EvidenceLedger{Query: "Windows Terminal 怎么粘贴？", Items: []knowledge.EvidenceItem{{
			ChunkID: "terminal-paste-001", Snippet: "使用 Ctrl+Shift+V 粘贴。",
		}}},
	}}
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Windows Terminal 怎么粘贴？"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "使用 Ctrl+Shift+V 粘贴。"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "粘贴呢"},
	)

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "粘贴呢", "使用 Ctrl+Shift+V 粘贴。")

	assert.Equal(t, "使用 Ctrl+Shift+V 粘贴。", got)
	assert.Len(t, eng.llmClient.(*mockLLM).calls, 1, "empty current retrieval must be checked against durable prior provenance")
}

func TestKnowledgeAnswerGrounding_MergesPriorVerifiedAndCurrentEvidence(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{"supported":true,"claims":[{"answer_quote":"粘贴使用 Ctrl+Shift+V","chunk_id":"terminal-paste-001","evidence_quote":"使用 Ctrl+Shift+V 粘贴"},{"answer_quote":"复制使用 Ctrl+Shift+C","chunk_id":"terminal-copy-002","evidence_quote":"使用 Ctrl+Shift+C 复制"}],"unsupported":[]}`}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{{Chunk: knowledge.KBChunk{ChunkID: "terminal-copy-002", Content: "使用 Ctrl+Shift+C 复制。"}, Kept: true}}
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Query: "Windows Terminal 复制和粘贴", Items: []knowledge.EvidenceItem{{
		ChunkID: "terminal-copy-002", Snippet: "使用 Ctrl+Shift+C 复制。",
	}}}
	eng.sessionState.VerifiedKnowledge = []VerifiedKnowledgeTurn{{
		Question: "Windows Terminal 怎么粘贴？",
		Answer:   "使用 Ctrl+Shift+V 粘贴。",
		Evidence: knowledge.EvidenceLedger{Query: "Windows Terminal 怎么粘贴？", Items: []knowledge.EvidenceItem{{
			ChunkID: "terminal-paste-001", Snippet: "使用 Ctrl+Shift+V 粘贴。",
		}}},
	}}

	answer := "粘贴使用 Ctrl+Shift+V；复制使用 Ctrl+Shift+C。"
	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), "Windows Terminal 复制和粘贴", answer)

	assert.Equal(t, answer, got)
	require.Len(t, mock.calls, 1)
	payload := mock.calls[0].Messages[1].Content
	assert.Contains(t, payload, "terminal-paste-001")
	assert.Contains(t, payload, "terminal-copy-002")
}

func TestKnowledgeAnswerGrounding_RawLeakCannotUseVerifierAsBypass(t *testing.T) {
	enableDisciplinedKnowledgeRepair(t)
	record := loadSanitizedContextRAGRecords(t)[2]
	leak := "资料原文：" + record.Evidence
	eng, mock := groundedEngineForRecord(t, record, repairResponse(record, 100))

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, leak)

	assert.Equal(t, record.Answer, got)
	require.Len(t, mock.calls, 1, "raw evidence goes directly to safe repair, never to a verifier that might bless the dump")
	assert.Contains(t, mock.calls[0].Messages[0].Content, "知识答案修复器")
	assert.NotContains(t, got, record.Evidence)
}

func TestKnowledgeAnswerGrounding_CitationMarkersCannotSplitRawLeakNeedle(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[0]
	for _, marker := range []string{"[1]", "[[" + record.ChunkID + "]]"} {
		t.Run(marker, func(t *testing.T) {
			enableDisciplinedKnowledgeRepair(t)
			runes := []rune(record.Evidence)
			leak := string(runes[:len(runes)/2]) + marker + string(runes[len(runes)/2:])
			eng, mock := groundedEngineForRecord(t, record, repairResponse(record, 100))
			require.NoError(t, knowledge.ValidateNoRawEvidenceLeak(leak, eng.searchKnowledgeHitsThisTurn),
				"precondition: the marker splits the old contiguous leak needle")
			require.Error(t, knowledge.ValidateNoRawEvidenceLeak(knowledge.StripCiteMarkers(leak), eng.searchKnowledgeHitsThisTurn),
				"the displayed answer reconstructs the raw evidence")

			got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, leak)

			assert.Equal(t, record.Answer, got)
			require.Len(t, mock.calls, 1, "split leak must skip verifier and go directly to bounded repair")
			assert.Contains(t, mock.calls[0].Messages[0].Content, "知识答案修复器")
		})
	}
}

func TestKnowledgeAnswerGrounding_PaidRepairSurvivesPostCallBudget(t *testing.T) {
	enableDisciplinedKnowledgeRepair(t)
	record := loadSanitizedContextRAGRecords(t)[4]
	eng, mock := groundedEngineForRecord(t, record,
		groundingVerdictResponse(record, false, 100),
		repairResponse(record, 60000),
	)
	eng.maxTokensPerTurn = 50000

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, "Git 慢是因为显卡型号不对。")

	assert.Equal(t, record.Answer, got)
	assert.Greater(t, eng.turnTokensConsumed, eng.maxTokensPerTurn)
	assert.Len(t, mock.calls, 2, "the already-paid repair must be validated from its proof and delivered")
}

func TestKnowledgeAnswerGrounding_RepairSuccessRetractsFailureAttribution(t *testing.T) {
	enableDisciplinedKnowledgeRepair(t)
	record := loadSanitizedContextRAGRecords(t)[1]
	eng, _ := groundedEngineForRecord(t, record,
		groundingVerdictResponse(record, false, 100),
		repairResponse(record, 100),
	)
	var blocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) { blocks = append(blocks, trace) })

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, "Ubuntu 镜像自带 8 张显卡。")

	assert.Equal(t, record.Answer, got)
	require.GreaterOrEqual(t, len(blocks), 2)
	assert.True(t, blocks[0].Hit)
	assert.Equal(t, "search_knowledge_ungrounded", blocks[0].Category)
	assert.False(t, blocks[len(blocks)-1].Hit, "a rescued turn must not remain recorded as blocked")
}

// Wiring gate: drive the real production agent-loop route. Deleting the
// finalizeAgentLoopKnowledgeAnswer call from engine.go makes the cited fabrication
// escape unchanged and this test red.
func TestKnowledgeAnswerGrounding_ProductionAgentLoopWiresUnifiedFinalGate(t *testing.T) {
	enableDisciplinedKnowledgeRepair(t)
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	SetGroundedAnswerValidatorEnabled(false)

	record := loadSanitizedContextRAGRecords(t)[3]
	chunk := knowledge.KBChunk{ChunkID: record.ChunkID, KBVersion: "sanitized.prod.fixture", Title: record.Name, Content: record.Evidence}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: chunk.KBVersion, Hits: []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	fabrication := "所有订单都能在 24 小时内全额退款 [[" + record.ChunkID + "]]。"
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{ID: "search", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"` + record.ResolvedQuestion + `"}`}}}},
		{Content: fabrication},
		groundingVerdictResponse(record, false, 100),
		repairResponse(record, 100),
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	got, err := eng.Chat(context.Background(), record.UserMessage, noopStep)

	require.NoError(t, err)
	assert.Equal(t, record.Answer, got)
	assert.NotContains(t, got, "24 小时")
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, record.ResolvedQuestion, retriever.calls[0].question)
	assertResolvedQuestionContract(t, mock.calls[2:], record.ResolvedQuestion)
}

func TestKnowledgeAnswerGrounding_ProofRejectsVerifierThatOnlySaysSupported(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[0]
	ledgerEngine, _ := groundedEngineForRecord(t, record)

	_, _, ok := validateKnowledgeGroundingProof(record.Answer, knowledgeGroundingVerdict{Supported: true}, ledgerEngine.searchKnowledgeLedgerThisTurn)

	assert.False(t, ok, "supported=true without answer/evidence quotes is not a proof")
}

func TestKnowledgeAnswerGrounding_ProofRejectsUncoveredSubstantiveClause(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[0]
	eng, _ := groundedEngineForRecord(t, record)
	answer := record.Answer + " 所有终端都保证支持这个快捷键。"
	verdict := knowledgeGroundingVerdict{Supported: true, Claims: []knowledgeGroundingClaim{{
		AnswerQuote: record.AnswerQuote, ChunkID: record.ChunkID, EvidenceQuote: record.EvidenceQuote,
	}}}

	_, _, ok := validateKnowledgeGroundingProof(answer, verdict, eng.searchKnowledgeLedgerThisTurn)

	assert.False(t, ok, "a verifier cannot omit a second unsupported sentence from its claim list")
}

func TestKnowledgeAnswerGrounding_ProofRejectsShortQuoteForLongClaim(t *testing.T) {
	record := loadSanitizedContextRAGRecords(t)[3]
	eng, _ := groundedEngineForRecord(t, record)
	verdict := knowledgeGroundingVerdict{Supported: true, Claims: []knowledgeGroundingClaim{{
		AnswerQuote: "支持退款", ChunkID: record.ChunkID, EvidenceQuote: "退款是否支持",
	}}}

	_, _, ok := validateKnowledgeGroundingProof("平台支持退款。", verdict, eng.searchKnowledgeLedgerThisTurn)

	assert.False(t, ok, "a short matching phrase cannot stand in for a full unsupported clause")
}

func TestKnowledgeAnswerGrounding_RejectsVerifierApprovedNegationContradiction(t *testing.T) {
	record := sanitizedContextRAGRecord{
		Name: "obvious_refund_contradiction", UserMessage: "这个订单能退款吗",
		ResolvedQuestion: "该订单是否支持退款", ChunkID: "sanitized-refund-contradiction-001",
		Evidence: "该订单不支持退款。", Answer: "该订单支持退款。",
		AnswerQuote: "该订单支持退款", EvidenceQuote: "该订单不支持退款",
	}
	// The model verifier is deliberately wrong: it says the positive answer is
	// supported by a negative evidence sentence. The local contradiction gate
	// must remain authoritative.
	eng, mock := groundedEngineForRecord(t, record, groundingVerdictResponse(record, true, 100))

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, record.Answer)

	assert.Equal(t, ragUngroundableReply, got)
	require.Len(t, mock.calls, 1)
}

func TestKnowledgeAnswerGrounding_RejectsVerifierApprovedQuantityReversal(t *testing.T) {
	// Sanitized from the real Coding Plan retrieval record in
	// eval/trace_gate/billing_jitter_ext1on.jsonl. The verifier is deliberately
	// wrong: it claims a 30-hour answer is supported by 5-hour evidence.
	record := sanitizedContextRAGRecord{
		Name: "coding_plan_window_reversal", UserMessage: "额度多久刷新",
		ResolvedQuestion: "Coding Plan 额度刷新窗口是多久", ChunkID: "sanitized-coding-plan-window-001",
		Evidence: "Coding Plan 采用固定 5 小时窗口刷新额度。", Answer: "额度采用固定 30 小时窗口刷新。",
		AnswerQuote: "额度采用固定 30 小时窗口刷新", EvidenceQuote: "采用固定 5 小时窗口刷新额度",
	}
	eng, mock := groundedEngineForRecord(t, record, groundingVerdictResponse(record, true, 100))

	got := eng.finalizeAgentLoopKnowledgeAnswer(context.Background(), record.UserMessage, record.Answer)

	assert.Equal(t, ragUngroundableReply, got)
	require.Len(t, mock.calls, 1)
}

func TestParseKnowledgeGroundingJSONFailsClosed(t *testing.T) {
	var verdict knowledgeGroundingVerdict
	assert.True(t, parseKnowledgeGroundingJSON("```json\n{\"supported\":false}\n```", &verdict))
	assert.False(t, parseKnowledgeGroundingJSON("大概是支持的", &verdict))
	assert.False(t, parseKnowledgeGroundingJSON(strings.Repeat("{", 2), &verdict))
}
