package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Loop exits retain the original task, history and completed observations for
// one final Agent response without tools. Cancellation skips that final attempt.

// mockLLMSteps scripts a per-call sequence of either a response or an error.
// Unlike mockLLM (responses only) and mockLLMWithError (always errors), it can
// model "round 0 succeeded and recorded evidence, then a later call errors/times
// out, then the final Agent response succeeds". A step's onErr runs just
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

// keptVLLMHit is the above-floor SearchKnowledge hit returned by the fixture.
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
// item (ext-vllm-oom-001). It also carries a model-authored operational token so
// every caller proves that recovery still crosses the common delivery boundary.
const recoveryModelToken = "recovery-model-token-abcdefghijklmnopqrst"

func vllmGroundedRepairResponse() llm.ChatResponse {
	return llm.ChatResponse{Content: `可以把 max-model-len 调小来降低显存占用[1]。临时地址：https://example.invalid/download?Authorization=` + recoveryModelToken}
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

// The final Agent request can use knowledge gathered before the loop ceiling.
func TestChat_RoundCeiling_RecoversFromGatheredEvidence(t *testing.T) {
	responses := make([]llm.ChatResponse, maxReActRounds+1)
	responses[0] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
		toolCall("sk", "SearchKnowledge", `{"query":"vllm 显存不足"}`),
	}}
	for i := 1; i < maxReActRounds; i++ {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("tc", "ReadCapability_resource_info", `{}`),
		}}
	}
	// The final request has the same conversation but no further tools.
	responses[maxReActRounds] = vllmGroundedRepairResponse()

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
	assert.NotContains(t, reply, recoveryModelToken, "round-ceiling recovery must cross the ordinary response redaction boundary")
	assert.True(t, eng.ReactCeilingHitThisTurn(),
		"trace attribution preserved: the loop DID hit the ceiling even though the user got an answer")
	require.Len(t, eng.searchKnowledgeHitsThisTurn, 1, "the gathered hit is what recovery grounds on")
}

func TestChat_RoundCeiling_DoesNotGuessFromInstanceKeywords(t *testing.T) {
	const sensitiveReply = "Jupyter Token：server-owned-token"
	const finalAnswer = "尚未取得足够的目标详情，暂时不能确认该实例当前状态。"
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall(fmt.Sprintf("list-%d", i), "ReadCapability_resource_info", `{}`),
		}}
	}
	responses = append(responses, llm.ChatResponse{Content: finalAnswer})
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
	model := &mockLLM{responses: responses}
	eng := NewWithDeps(model, exec, nil)
	limiter := &scriptedRateLimiter{}
	limiter.before = func(governance.Request) {
		eng.sensitiveRepliesThisTurn = []string{sensitiveReply}
	}
	eng.rateLimiter = limiter
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	require.NoError(t, eng.registry.SyncFromDescribe(describe, "test"))

	reply, err := eng.Chat(context.Background(), "claude-write-test 这台状态怎么样", noopStep)
	require.NoError(t, err)
	assert.Equal(t, sensitiveReply+"\n\n"+finalAnswer, reply)
	require.Len(t, model.calls, maxReActRounds+1)
	finalRequest := model.calls[maxReActRounds]
	require.Empty(t, finalRequest.Tools)
	require.Contains(t, renderTestMessages(finalRequest.Messages), "claude-write-test 这台状态怎么样")
	require.True(t, eng.ReactCeilingHitThisTurn())
}

func TestChat_RoundCeiling_DoesNotClassifyPunctuationFollowup(t *testing.T) {
	const finalAnswer = "host-a 运行中，host-b 已关机。"
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall(fmt.Sprintf("list-%d", i), "ReadCapability_resource_info", `{}`),
		}}
	}
	responses = append(responses, llm.ChatResponse{Content: finalAnswer})
	describe := map[string]any{"UHostSet": []any{
		map[string]any{"UHostId": "uhost-a", "Name": "host-a", "State": "Running", "GpuType": "4090", "Zone": "cn-wlcb-01"},
		map[string]any{"UHostId": "uhost-b", "Name": "host-b", "State": "Stopped", "GpuType": "A100", "Zone": "cn-sh2-02"},
	}, "TotalCount": float64(2)}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": describe,
	}}
	model := &mockLLM{responses: responses}
	eng := NewWithDeps(model, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
		{Role: openai.ChatMessageRoleUser, Content: "我有哪些实例"},
		{Role: openai.ChatMessageRoleAssistant, Content: "您共有 2 台实例：host-a、host-b。"},
	}
	require.NoError(t, eng.registry.SyncFromDescribe(describe, "test"))

	reply, err := eng.Chat(context.Background(), "？", noopStep)
	require.NoError(t, err)
	assert.Equal(t, finalAnswer, reply)
	require.Len(t, model.calls, maxReActRounds+1)
	finalRequest := model.calls[maxReActRounds]
	require.Empty(t, finalRequest.Tools)
	require.Contains(t, renderTestMessages(finalRequest.Messages), "我有哪些实例")
	require.Contains(t, renderTestMessages(finalRequest.Messages), "您共有 2 台实例：host-a、host-b。")
	require.Contains(t, renderTestMessages(finalRequest.Messages), "？")
}

func TestChat_RoundCeiling_DoesNotParseTargetFromFreeText(t *testing.T) {
	const finalAnswer = "尚未找到 autotest，未执行开机。"
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall(fmt.Sprintf("list-%d", i), "ReadCapability_resource_info", `{}`),
		}}
	}
	responses = append(responses, llm.ChatResponse{Content: finalAnswer})
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
	model := &mockLLM{responses: responses}
	eng := NewWithDeps(model, exec, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	require.NoError(t, eng.registry.SyncFromDescribe(describe, "test"))

	reply, err := eng.Chat(context.Background(), "将autotest这台实例用无卡模式开启", noopStep)
	require.NoError(t, err)
	assert.Equal(t, finalAnswer, reply)
	require.Len(t, model.calls, maxReActRounds+1)
	finalRequest := model.calls[maxReActRounds]
	require.Empty(t, finalRequest.Tools)
	require.Contains(t, renderTestMessages(finalRequest.Messages), "将autotest这台实例用无卡模式开启")
	require.NotContains(t, exec.calls, "StartCompShareInstance")
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
	assert.NotContains(t, reply, recoveryModelToken, "LLM-error recovery must cross the ordinary response redaction boundary")
	assert.Equal(t, 3, mock.idx, "search, failed model attempt and final Agent response")
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
		{err: fmt.Errorf("context canceled"), onErr: cancel}, // cancel as the error surfaces
		{resp: &llm.ChatResponse{Content: "这条不应被消费 [1]。"}},   // final attempt must not run after cancellation
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
	assert.Equal(t, 2, mock.idx, "cancellation skips the final Agent attempt")
}
