package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the loop-exit graceful-degradation contract: when the agent
// loop reaches the round ceiling OR an LLM call errors AFTER a prior round
// already gathered groundable SearchKnowledge evidence, deliver the cited answer
// from that evidence instead of discarding the whole turn for a bare
// "请重新描述" / "LLM 调用失败". This reuses the budget-exit recovery primitive
// (synthesizeOnBudgetExceeded), which is empty-ledger-gated, so a turn that
// gathered nothing is byte-identical to before (those halves are pinned by the
// extended TestChat_MaxRoundsExceeded / TestChat_LLMError).
//
// WHY this matters: in the current-main replay these exits produced the
// "处理轮次超限" cluster (M106/M110/M115/M143) and the 180s-timeout cluster
// (M095/M118/M148) — turns where the user got nothing even though the system had
// already retrieved an answer.

// mockLLMSteps scripts a per-call sequence of either a response or an error.
// Unlike mockLLM (responses only) and mockLLMWithError (always errors), it can
// model "round 0 succeeded and recorded evidence, then a later call errors/times
// out, then the recovery synthesis call succeeds". A step's onErr runs just
// before the error is returned — used to cancel the ctx mid-flight so the
// recovery ctx-gate can be exercised.
type mockLLMSteps struct {
	steps []llmStep
	idx   int
}

type llmStep struct {
	resp  *llm.ChatResponse
	err   error
	onErr func()
}

func (m *mockLLMSteps) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	if m.idx >= len(m.steps) {
		// Exhausted: a final no-tool-call text reply (mirrors mockLLM) so an
		// over-run never hangs the loop.
		return &llm.ChatResponse{Content: "no more mock steps"}, nil
	}
	s := m.steps[m.idx]
	m.idx++
	if s.err != nil {
		if s.onErr != nil {
			s.onErr()
		}
		return nil, s.err
	}
	return s.resp, nil
}

// keptVLLMHit is the kept (above-floor) SearchKnowledge hit the scripted
// retriever returns; KBVersion is required by NewEvidence for synthesis.
func keptVLLMHit() knowledge.RetrievalHit {
	return knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID:    "ext-vllm-oom-001",
		KBVersion:  "merged",
		Title:      "vLLM 降显存",
		SourceType: "external",
		Content:    "把 max-model-len 设小一点即可显著降低显存占用，过长的上下文会占用更多 KV cache。",
	}}
}

// vllmGroundedRepairResponse is the single-model budget/ceiling recovery reply:
// plain text with a positional [1] citation that resolves to the gathered ledger
// item (ext-vllm-oom-001). The runtime strips the marker for display.
func vllmGroundedRepairResponse() llm.ChatResponse {
	return llm.ChatResponse{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用[1]。"}`}
}

func vllmRetriever() *scriptedKnowledgeRetriever {
	return &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:  true,
		HitItems: []knowledge.RetrievalHit{keptVLLMHit()},
	}}}
}

func prepareKnowledgeRecoveryLane(t *testing.T, eng *Engine) {
	t.Helper()
	eng.InitWithContext("test user")
}

// TestChat_RoundCeiling_RecoversFromGatheredEvidence: round 0 calls
// SearchKnowledge (records a kept hit), rounds 1..9 thrash with a plain read and
// never produce a final text reply, so the loop hits the round ceiling with a
// non-empty ledger → recovery synthesizes the cited answer instead of refusing.
func TestChat_RoundCeiling_RecoversFromGatheredEvidence(t *testing.T) {
	// One extra slot: the SearchKnowledge in round 0 now also pays a planner call,
	// which sits between round 0 and round 1 in the response sequence.
	responses := make([]llm.ChatResponse, maxReActRounds+2)
	responses[0] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
		toolCall("sk", "SearchKnowledge", `{"query":"vllm 显存不足"}`),
	}}
	responses[1] = plannerEcho("vllm 显存不足")
	for i := 2; i <= maxReActRounds; i++ {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("tc", "ReadCapability_resource_info", `{}`),
		}}
	}
	// Index maxReActRounds is consumed by the one-call grounded recovery, NOT
	// the loop. It returns the answer together with an evidence proof.
	responses[maxReActRounds+1] = vllmGroundedRepairResponse()

	mock := &mockLLM{responses: responses}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	prepareKnowledgeRecoveryLane(t, eng)
	eng.SetKnowledgeRetriever(vllmRetriever())
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "vllm 显存不足怎么办", noopStep)
	require.NoError(t, err)
	assert.NotContains(t, reply, "轮次超限", "evidence was in hand — must recover, not refuse")
	assert.Contains(t, reply, "max-model-len", "the grounded answer must flow through")
	assert.NotContains(t, reply, "[1]", "the positional cite marker is stripped for display")
	assert.True(t, eng.ReactCeilingHitThisTurn(),
		"trace attribution preserved: the loop DID hit the ceiling even though the user got an answer")
	require.Len(t, eng.searchKnowledgeHitsThisTurn, 1, "the gathered hit is what recovery grounds on")
}

func TestChat_RoundCeiling_DoesNotGuessFromInstanceKeywords(t *testing.T) {
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall(fmt.Sprintf("list-%d", i), "ReadCapability_resource_info", `{}`),
		}}
	}
	target := map[string]any{
		"UHostId": "uhost-zzzz-hidden",
		"Name":    "claude-write-test",
		"State":   "Stopped",
		"GpuType": "4090",
		"GPU":     float64(1),
		"CPU":     float64(16),
		"Memory":  float64(65536),
		"Zone":    "cn-wlcb-01",
	}
	hosts := make([]any, 0, 25)
	for i := 0; i < 24; i++ {
		hosts = append(hosts, map[string]any{
			"UHostId": fmt.Sprintf("uhost-visible-%02d", i),
			"Name":    fmt.Sprintf("visible-%02d", i),
			"State":   "Running",
			"GpuType": "4090",
			"GPU":     float64(1),
			"CPU":     float64(16),
			"Memory":  float64(65536),
			"Zone":    "cn-wlcb-01",
		})
	}
	hosts = append(hosts, target)
	describe := map[string]any{"UHostSet": hosts, "TotalCount": float64(len(hosts))}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": describe,
	}}
	eng := NewWithDeps(&mockLLM{responses: responses}, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	require.NoError(t, eng.registry.SyncFromDescribe(describe, "test"))

	reply, err := eng.Chat(context.Background(), "claude-write-test 这台状态怎么样", noopStep)
	require.NoError(t, err)
	assert.Equal(t, reactCeilingRefusal, reply)
}

func TestChat_RoundCeiling_DoesNotClassifyPunctuationFollowup(t *testing.T) {
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall(fmt.Sprintf("list-%d", i), "ReadCapability_resource_info", `{}`),
		}}
	}
	describe := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-a", "Name": "host-a", "State": "Running", "GpuType": "4090", "Zone": "cn-wlcb-01"},
		map[string]any{"UHostId": "uhost-b", "Name": "host-b", "State": "Stopped", "GpuType": "A100", "Zone": "cn-sh2-02"},
	}, "TotalCount": float64(2)}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": describe,
	}}
	eng := NewWithDeps(&mockLLM{responses: responses}, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
		{Role: openai.ChatMessageRoleUser, Content: "我有哪些实例"},
		{Role: openai.ChatMessageRoleAssistant, Content: "您共有 2 台实例：host-a、host-b。"},
	}
	require.NoError(t, eng.registry.SyncFromDescribe(describe, "test"))

	reply, err := eng.Chat(context.Background(), "？", noopStep)
	require.NoError(t, err)
	assert.Equal(t, reactCeilingRefusal, reply)
}

func TestChat_RoundCeiling_DoesNotParseTargetFromFreeText(t *testing.T) {
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall(fmt.Sprintf("list-%d", i), "ReadCapability_resource_info", `{}`),
		}}
	}
	describe := map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId": "uhost-existing",
			"Name":    "host",
			"State":   "Stopped",
			"GpuType": "4090",
			"GPU":     float64(1),
			"CPU":     float64(16),
			"Memory":  float64(65536),
			"Zone":    "cn-wlcb-01",
		},
	}, "TotalCount": float64(1)}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": describe,
	}}
	eng := NewWithDeps(&mockLLM{responses: responses}, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	require.NoError(t, eng.registry.SyncFromDescribe(describe, "test"))

	reply, err := eng.Chat(context.Background(), "将autotest这台实例用无卡模式开启", noopStep)
	require.NoError(t, err)
	assert.Equal(t, reactCeilingRefusal, reply)
}

// TestChat_LLMError_RecoversWhenEvidenceInHandAndCtxLive: round 0 gathers
// evidence, round 1's LLM call errors (the jittery-timeout shape) with the outer
// ctx still live → recover the grounded answer rather than returning a bare
// "LLM 调用失败".
func TestChat_LLMError_RecoversWhenEvidenceInHandAndCtxLive(t *testing.T) {
	mock := &mockLLMSteps{steps: []llmStep{
		{resp: &llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("sk", "SearchKnowledge", `{"query":"vllm 显存不足"}`),
		}}},
		{err: fmt.Errorf("connection reset by peer")}, // round 1 errors → recovery
		{resp: func() *llm.ChatResponse { r := vllmGroundedRepairResponse(); return &r }()},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	prepareKnowledgeRecoveryLane(t, eng)
	eng.SetKnowledgeRetriever(vllmRetriever())
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "vllm 显存不足怎么办", noopStep)
	require.NoError(t, err, "evidence in hand + live ctx → recover, not error")
	assert.Contains(t, reply, "max-model-len")
	assert.NotContains(t, reply, "[1]")
	assert.Equal(t, 3, mock.idx, "round0 search + round1 error + synthesis = exactly 3 LLM calls")
}

// TestChat_LLMError_CtxCancelledSkipsRecovery pins the ctx gate: evidence is in
// hand, but the ctx is cancelled as the LLM error surfaces → recovery must be
// SKIPPED (a recovery call on a dead ctx would just fail again and mask the
// cancellation) and the error must propagate. Distinguishes a transient timeout
// (recover) from a genuine cancellation (give up honestly).
func TestChat_LLMError_CtxCancelledSkipsRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mock := &mockLLMSteps{steps: []llmStep{
		{resp: &llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("sk", "SearchKnowledge", `{"query":"vllm 显存不足"}`),
		}}},
		{resp: func() *llm.ChatResponse { r := plannerEcho("vllm 显存不足"); return &r }()},
		{err: fmt.Errorf("context canceled"), onErr: cancel}, // cancel as the error surfaces
		{resp: &llm.ChatResponse{Content: "这条不应被消费 [1]。"}},   // synthesis step — must NOT run
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	prepareKnowledgeRecoveryLane(t, eng)
	eng.SetKnowledgeRetriever(vllmRetriever())
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(ctx, "vllm 显存不足怎么办", noopStep)
	require.Error(t, err, "a cancelled ctx must not be masked by recovery")
	assert.Contains(t, err.Error(), "LLM 调用失败")
	assert.Equal(t, 3, mock.idx, "recovery synthesis must be skipped — only round0, its planner call, and the erroring round1 ran")
}
