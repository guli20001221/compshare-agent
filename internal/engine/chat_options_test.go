package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deltaMockLLM returns a final text reply and fires OnTextDelta for each
// Chinese character in the response, simulating a streaming LLM.
type deltaMockLLM struct {
	reqs []llm.ChatRequest
}

func (m *deltaMockLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.reqs = append(m.reqs, req)
	if req.OnTextDelta != nil {
		req.OnTextDelta("你")
		req.OnTextDelta("好")
	}
	return &llm.ChatResponse{
		Content: "你好",
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

func TestChatWithOptionsStreamsTextDeltas(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	var deltas []string
	var usage llm.TokenUsage
	reply, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
		OnTextDelta: func(s string) { deltas = append(deltas, s) },
		OnUsage:     func(u llm.TokenUsage) { usage = u },
	})

	require.NoError(t, err)
	assert.Equal(t, "你好", reply)
	assert.Equal(t, []string{"你", "好"}, deltas)
	assert.Equal(t, 3, usage.TotalTokens)
	require.Len(t, client.reqs, 1)
	assert.NotNil(t, client.reqs[0].OnTextDelta)
}

func TestChatWithOptionsUsageCalledOnFinalTextBranch(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	usageCalled := false
	_, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
		OnUsage: func(u llm.TokenUsage) { usageCalled = true },
	})
	require.NoError(t, err)
	assert.True(t, usageCalled)
}

func TestChatDelegatesToChatWithOptions(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	reply, err := eng.Chat(context.Background(), "你好", nil)
	require.NoError(t, err)
	assert.Equal(t, "你好", reply)
}

func TestRehydrateHistoryBuildsSystemUserAssistantMessages(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })

	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "第一问"},
		{Role: openai.ChatMessageRoleAssistant, Content: "第一答"},
	})

	require.Len(t, eng.messages, 3)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, openai.ChatMessageRoleUser, eng.messages[1].Role)
	assert.Equal(t, "第一问", eng.messages[1].Content)
	assert.Equal(t, openai.ChatMessageRoleAssistant, eng.messages[2].Role)
	assert.Equal(t, "第一答", eng.messages[2].Content)
}

func TestRehydrateHistorySkipsEmptyContent(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })

	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: ""},
		{Role: openai.ChatMessageRoleUser, Content: "valid"},
	})

	// system + one valid user message
	require.Len(t, eng.messages, 2)
	assert.Equal(t, openai.ChatMessageRoleUser, eng.messages[1].Role)
	assert.Equal(t, "valid", eng.messages[1].Content)
}

func TestRehydrateHistorySkipsNonUserNonAssistantRoles(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })

	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleTool, Content: "tool result"},
		{Role: openai.ChatMessageRoleSystem, Content: "another system"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})

	// system + one valid user message
	require.Len(t, eng.messages, 2)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, "hi", eng.messages[1].Content)
}

// TestOnTextDeltaReplacedWhenCitedContractOverrides verifies that when the
// cited-contract guard fires (requireKnowledgeCitationThisTurn=true, retriever
// non-nil, content has no numbered citation), OnTextDelta receives the
// ragNoEvidenceReply as a single chunk instead of the raw streamed deltas.
//
// The guard is triggered via the low-confidence planner path: a scripted
// IntentPlanner returns a KnowledgeQA plan with confidence 0.3 (below the 0.60
// threshold), causing commonPlannerCandidateStatus to return !ok and setting
// requireKnowledgeCitationThisTurn=true before the ReAct loop runs. The
// non-nil knowledgeRetriever satisfies the second guard condition. The mock LLM
// returns "你好" (no [n] citation), so the guard replaces it with ragNoEvidenceReply.
func TestOnTextDeltaReplacedWhenCitedContractOverrides(t *testing.T) {
	// deltaMockLLM fires "你" + "好" and returns Content:"你好" — no numbered
	// citation, so the cited-contract guard will replace it with ragNoEvidenceReply.
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	// Low-confidence KnowledgeQA plan triggers requireKnowledgeCitationThisTurn=true
	// via the !ok branch of commonPlannerCandidateStatus (confidence < 0.60).
	lowConfPlan := intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentKnowledgeQA,
		Confidence:    0.3,
	}
	eng.intentPlanner = &scriptedIntentPlanner{
		results: []intent.PlannerResult{{Plan: lowConfPlan}},
	}
	// A non-nil retriever satisfies the guard's knowledgeRetriever != nil check.
	// plannerDispatchEnabled also returns true when knowledgeRetriever != nil.
	eng.knowledgeRetriever = &scriptedKnowledgeRetriever{}

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
		OnTextDelta: func(s string) { deltas = append(deltas, s) },
	})

	require.NoError(t, err)
	// The engine must return the override content, not the raw LLM response.
	assert.Equal(t, ragNoEvidenceReply, reply)
	// OnTextDelta must emit exactly one chunk containing the override, not the
	// raw streamed deltas ("你", "好").
	require.Len(t, deltas, 1, "expected a single override chunk, not raw deltas")
	assert.Equal(t, ragNoEvidenceReply, deltas[0])
}

// TestOnTextDeltaReplaysRawDeltasWhenNoOverride verifies that when no engine
// guard rewrites the content, OnTextDelta still receives the original streamed
// chunks in order (regression guard for the unchanged path).
func TestOnTextDeltaReplaysRawDeltasWhenNoOverride(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")
	// No retriever, no requireKnowledgeCitationThisTurn — guard cannot fire.

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
		OnTextDelta: func(s string) { deltas = append(deltas, s) },
	})

	require.NoError(t, err)
	assert.Equal(t, "你好", reply)
	// Raw chunks replayed in order.
	assert.Equal(t, []string{"你", "好"}, deltas)
}
