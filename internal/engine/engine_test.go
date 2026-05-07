package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"

	openai "github.com/sashabaranov/go-openai"
)

// --- Mock LLM Client ---

type mockLLM struct {
	responses []llm.ChatResponse // returned in sequence
	calls     []llm.ChatRequest  // recorded calls
	callIdx   int
}

func (m *mockLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls = append(m.calls, req)
	if m.callIdx >= len(m.responses) {
		return &llm.ChatResponse{Content: "no more mock responses"}, nil
	}
	resp := m.responses[m.callIdx]
	m.callIdx++
	return &resp, nil
}

// mockLLMWithError always returns an error.
type mockLLMWithError struct {
	err error
}

func (m *mockLLMWithError) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	return nil, m.err
}

// --- Mock Executor ---

type mockExecutor struct {
	results map[string]map[string]any
	calls   []string
}

func (m *mockExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	if r, ok := m.results[action]; ok {
		return r, nil
	}
	return map[string]any{"Action": action, "RetCode": 0}, nil
}

// mockExecutorFn is a function-based mock for tests that need per-call control.
type mockExecutorFn struct {
	fn    func(action string, args map[string]any) (map[string]any, error)
	calls []string
}

func (m *mockExecutorFn) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	return m.fn(action, args)
}

// --- Helpers ---

func noopStep(StepEvent) {}

// hasStaleNote checks if the messages in a ChatRequest contain the stale-state system note.
func hasStaleNote(req llm.ChatRequest) bool {
	for _, m := range req.Messages {
		if m.Role == openai.ChatMessageRoleSystem && strings.Contains(m.Content, "实例状态信息可能已过时") {
			return true
		}
	}
	return false
}

func requestContainsMessageText(req llm.ChatRequest, text string) bool {
	for _, m := range req.Messages {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

func collectSteps() (func(StepEvent), *[]StepEvent) {
	var events []StepEvent
	return func(ev StepEvent) { events = append(events, ev) }, &events
}

func toolCall(id, name, argsJSON string) openai.ToolCall {
	return openai.ToolCall{
		ID:   id,
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      name,
			Arguments: argsJSON,
		},
	}
}

// --- Tests ---

func TestChat_DirectReply(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您好，有什么可以帮您？"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, "您好，有什么可以帮您？", reply)

	// Should have 1 LLM call with system + user messages
	assert.Len(t, mock.calls, 1)
	assert.Len(t, mock.calls[0].Messages, 2) // system + user
}

func TestChat_KnowledgeTool_GetGPUSpecs(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Round 1: LLM decides to call GetGPUSpecs
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetGPUSpecs", `{"GpuType":"4090"}`),
		}},
		// Round 2: LLM generates final reply using tool result
		{Content: "4090 有 24GB 显存"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "4090什么配置", onStep)
	assert.NoError(t, err)
	assert.Equal(t, "4090 有 24GB 显存", reply)

	// Should have tool call + tool result events
	assert.GreaterOrEqual(t, len(*events), 2)
	assert.Equal(t, StepToolCall, (*events)[0].Type)
	assert.Equal(t, "GetGPUSpecs", (*events)[0].Action)
	assert.Equal(t, StepToolResult, (*events)[1].Type)

	// Tool result fed back to LLM should contain GPU spec data
	assert.Len(t, mock.calls, 2)
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	assert.Contains(t, toolMsg.Content, "24")          // VRAM
	assert.Contains(t, toolMsg.Content, "fp16_tflops") // has FP16 field
}

func TestChat_KnowledgeTool_GetGPURecommendation(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetGPURecommendation", `{"scene":"LoRA微调","budget_sensitive":false}`),
		}},
		{Content: "推荐 4090"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "微调用什么卡", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "4090")

	// Verify tool result contains recommendation
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Contains(t, toolMsg.Content, "recommendations")
}

func TestChat_ExternalTool_L0(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{}`),
		}},
		{Content: "您没有实例"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "我有什么实例", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, "您没有实例", reply)

	// Executor should have been called
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
}

func TestChat_ExternalTool_L2Blocked(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "TerminateCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
		{Content: "好的，已取消"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "删除实例", onStep)
	assert.NoError(t, err)

	// Should have a blocked event
	hasBlocked := false
	for _, ev := range *events {
		if ev.Type == StepBlocked {
			hasBlocked = true
			assert.Contains(t, ev.Message, "L2")
		}
	}
	assert.True(t, hasBlocked, "L2 operation should be blocked")
}

func TestChat_ExternalTool_L1Confirmed(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"StopCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
		{Content: "已关机"},
	}}
	eng := NewWithDeps(mock, executor, confirmFn)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "关机", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, "已关机", reply)
	assert.Contains(t, executor.calls, "StopCompShareInstance")
}

func TestChat_ExternalTool_L1Denied(t *testing.T) {
	confirmFn := func(action string, args map[string]any) bool { return false }
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
		{Content: "好的，已取消"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, confirmFn)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "关机", onStep)
	assert.NoError(t, err)

	hasBlocked := false
	for _, ev := range *events {
		if ev.Type == StepBlocked && strings.Contains(ev.Message, "取消") {
			hasBlocked = true
		}
	}
	assert.True(t, hasBlocked)
}

func TestChat_InvalidToolArgs(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetGPUSpecs", `not json`),
		}},
		{Content: "抱歉出错了"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "test", onStep)
	assert.NoError(t, err)

	hasError := false
	for _, ev := range *events {
		if ev.Type == StepError {
			hasError = true
		}
	}
	assert.True(t, hasError, "invalid JSON args should produce error event")
}

func TestChat_MaxRoundsExceeded(t *testing.T) {
	// LLM always returns tool calls, never a text reply
	responses := make([]llm.ChatResponse, maxReActRounds+1)
	for i := range responses {
		responses[i] = llm.ChatResponse{
			ToolCalls: []openai.ToolCall{
				toolCall("tc", "GetGPUSpecs", `{"GpuType":"4090"}`),
			},
		}
	}
	mock := &mockLLM{responses: responses}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "test", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "轮次超限")
}

func TestInit_InjectsContextAndFAQ(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc", "Name": "test",
					"State": "Running", "GpuType": "4090",
					"GPU": float64(1), "ChargeType": "Postpay",
				},
			},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	suggestions, err := eng.Init(context.Background())

	assert.NoError(t, err)
	assert.NotEmpty(t, suggestions)

	// System prompt should contain user context + FAQ
	systemMsg := eng.messages[0]
	assert.Equal(t, openai.ChatMessageRoleSystem, systemMsg.Role)
	assert.Contains(t, systemMsg.Content, "uhost-abc")
	assert.Contains(t, systemMsg.Content, "平台常见问题") // FAQ injected
	assert.Contains(t, systemMsg.Content, "计费/回收规则")
}

func TestInit_FailedContextInjection(t *testing.T) {
	// Executor fails — should still work with default context
	executor := &mockExecutor{} // returns generic result
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	suggestions, err := eng.Init(context.Background())

	assert.NoError(t, err)
	assert.NotEmpty(t, suggestions)
	// Should still have system prompt with FAQ
	assert.Contains(t, eng.messages[0].Content, "平台常见问题")
}

func TestKnowledgeTool_DoesNotCallExecutor(t *testing.T) {
	executor := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetGPUSpecs", `{"GpuType":"A100"}`),
		}},
		{Content: "A100 规格"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "A100", noopStep)
	assert.NoError(t, err)

	// Executor should NOT have been called — knowledge tools are local
	assert.Empty(t, executor.calls)
}

func TestMultipleToolCalls(t *testing.T) {
	// LLM calls two tools in one round (e.g., GetGPUSpecs for two GPUs)
	idx0 := 0
	idx1 := 1
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			{ID: "tc1", Type: openai.ToolTypeFunction, Index: &idx0,
				Function: openai.FunctionCall{Name: "GetGPUSpecs", Arguments: `{"GpuType":"4090"}`}},
			{ID: "tc2", Type: openai.ToolTypeFunction, Index: &idx1,
				Function: openai.FunctionCall{Name: "GetGPUSpecs", Arguments: `{"GpuType":"A100"}`}},
		}},
		{Content: "对比结果"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "对比", onStep)
	assert.NoError(t, err)
	assert.Equal(t, "对比结果", reply)

	// Should have 4 events: 2x (tool_call + tool_result)
	toolCalls := 0
	for _, ev := range *events {
		if ev.Type == StepToolCall {
			toolCalls++
		}
	}
	assert.Equal(t, 2, toolCalls)

	// LLM round 2 should have both tool results
	round2Msgs := mock.calls[1].Messages
	toolResults := 0
	for _, m := range round2Msgs {
		if m.Role == openai.ChatMessageRoleTool {
			toolResults++
		}
	}
	assert.Equal(t, 2, toolResults)
}

func TestConversationHistory_Accumulates(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "回复1"},
		{Content: "回复2"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "问题1", noopStep)
	eng.Chat(context.Background(), "问题2", noopStep)

	// Second call should include: system + user1 + assistant1 + user2
	assert.Len(t, mock.calls, 2)
	assert.Len(t, mock.calls[1].Messages, 4) // system + u1 + a1 + u2

	// Verify message history
	msgs := mock.calls[1].Messages
	assert.Equal(t, openai.ChatMessageRoleSystem, msgs[0].Role)
	assert.Equal(t, "问题1", msgs[1].Content)
	assert.Equal(t, "回复1", msgs[2].Content)
	assert.Equal(t, "问题2", msgs[3].Content)
}

func TestUnknownAction_Rejected(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "HackTheSystem", `{}`),
		}},
		{Content: "好的"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "hack", onStep)
	assert.NoError(t, err)

	// Unknown action should be rejected by security check
	hasError := false
	for _, ev := range *events {
		if ev.Type == StepError {
			hasError = true
		}
	}
	assert.True(t, hasError, "unknown action should produce error")
}

func TestTrimHistory(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	// Build a long message history: system + 50 user/assistant pairs
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
	}
	for i := 0; i < 50; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("q%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	assert.Equal(t, 101, len(eng.messages)) // 1 + 100

	eng.trimHistory()

	// Should keep system + last maxHistoryMessages
	assert.Equal(t, 1+maxHistoryMessages, len(eng.messages))
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, "system prompt", eng.messages[0].Content)

	// Last message should be the most recent
	lastMsg := eng.messages[len(eng.messages)-1]
	assert.Equal(t, "a49", lastMsg.Content)
}

func TestTrimHistory_ShortHistory_NoOp(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: "hello"},
	}

	eng.trimHistory()
	assert.Len(t, eng.messages, 3) // unchanged
}

func TestTrimHistory_SkipsToolCallGroup(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	// Build history where the naive cut point (len - maxHistoryMessages)
	// lands inside an assistant(tool_calls) + tool group.
	// Structure: system + 18 user/assistant pairs + 1 tool_call group + 2 user/assistant pairs
	// = 1 + 36 + 4 + 4 = 45 messages
	// With maxHistoryMessages=40, candidate cut at index 5 (message[5]).
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
	}
	// 18 safe pairs = 36 messages (indices 1-36)
	for i := 0; i < 18; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("u%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	// Tool call group: assistant(tool_calls) + 2 tool responses + assistant reply = 4 messages (indices 37-40)
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			ToolCalls: []openai.ToolCall{{ID: "tc1"}, {ID: "tc2"}},
		},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, Content: "result1", ToolCallID: "tc1"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, Content: "result2", ToolCallID: "tc2"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "summary"},
	)
	// 2 more safe pairs = 4 messages (indices 41-44)
	for i := 18; i < 20; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("u%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	total := len(eng.messages)
	assert.Equal(t, 45, total) // 1 + 36 + 4 + 4

	eng.trimHistory()

	// The cut must NOT land inside the tool_call group.
	// Verify: first non-system message must be user or plain assistant, never tool.
	assert.Greater(t, len(eng.messages), 1)
	first := eng.messages[1]
	assert.NotEqual(t, openai.ChatMessageRoleTool, first.Role,
		"first kept message should not be an orphaned tool response")

	// Verify no assistant(tool_calls) without matching tool responses
	for i, msg := range eng.messages {
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0 {
			// Every tool_call ID must have a matching tool response after it
			for _, tc := range msg.ToolCalls {
				found := false
				for j := i + 1; j < len(eng.messages); j++ {
					if eng.messages[j].Role == openai.ChatMessageRoleTool && eng.messages[j].ToolCallID == tc.ID {
						found = true
						break
					}
				}
				assert.True(t, found, "tool_call %s has no matching tool response", tc.ID)
			}
		}
	}

	// System prompt preserved
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	// Most recent messages preserved
	assert.Equal(t, "a19", eng.messages[len(eng.messages)-1].Content)
}

func TestTrimHistory_CutPointIsOrphanedTool(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	// Create history where candidate cut lands exactly on a tool message.
	// system + 19 pairs(38) + assistant(tc)+tool+tool+assistant(4) + 1 pair(2) = 45
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
	}
	for i := 0; i < 19; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("u%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	// Tool group at indices 39-42
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "x1"}}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, Content: "r1", ToolCallID: "x1"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "done"},
	)
	// 1 final pair
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "last_q"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "last_a"},
	)
	assert.Equal(t, 44, len(eng.messages))

	eng.trimHistory()

	// First non-system must be safe
	first := eng.messages[1]
	assert.True(t,
		first.Role == openai.ChatMessageRoleUser ||
			(first.Role == openai.ChatMessageRoleAssistant && len(first.ToolCalls) == 0),
		"first kept message role=%s toolCalls=%d is not safe", first.Role, len(first.ToolCalls))

	assert.Equal(t, "last_a", eng.messages[len(eng.messages)-1].Content)
}

func TestChat_LLMError(t *testing.T) {
	mock := &mockLLMWithError{err: fmt.Errorf("connection refused")}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "hello", noopStep)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "LLM 调用失败")
}

func TestKnowledgeTool_ArgsFiltered(t *testing.T) {
	// Knowledge tools should also have args filtered
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetGPUSpecs", `{"GpuType":"4090","evil":"injection"}`),
		}},
		{Content: "done"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "test", onStep)

	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "GetGPUSpecs" {
			assert.NotContains(t, ev.Args, "evil")
			assert.Contains(t, ev.Args, "GpuType")
		}
	}
}

func TestFilterAllowedParams_StripsUnknown(t *testing.T) {
	args := map[string]any{
		"Zone":          "cn-wlcb-a",
		"GpuType":       "4090",
		"injected_evil": "drop table",
		"__proto__":     "bad",
	}
	filtered := filterAllowedParams("GetCompShareInstancePrice", args)

	assert.Contains(t, filtered, "Zone")
	assert.Contains(t, filtered, "GpuType")
	assert.NotContains(t, filtered, "injected_evil")
	assert.NotContains(t, filtered, "__proto__")
}

func TestFilterAllowedParams_PassesThroughUnknownTool(t *testing.T) {
	args := map[string]any{"foo": "bar"}
	filtered := filterAllowedParams("NonexistentTool", args)
	assert.Equal(t, args, filtered) // unchanged
}

func TestFilterAllowedParams_ExternalToolCall(t *testing.T) {
	// Verify that injected params are stripped in a full Chat flow
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance",
				`{"UHostIds":["uhost-xxx"],"evil":"injection","Limit":10}`),
		}},
		{Content: "done"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "查实例", onStep)

	// The tool call event args should NOT contain "evil"
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DescribeCompShareInstance" {
			assert.NotContains(t, ev.Args, "evil")
			assert.Contains(t, ev.Args, "UHostIds")
			assert.Contains(t, ev.Args, "Limit")
		}
	}
}

// Verify tool result JSON is valid by parsing it
func TestToolResult_IsValidJSON(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetGPUSpecs", `{"GpuType":"H20"}`),
		}},
		{Content: "done"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "test", noopStep)

	// The tool result message should be valid JSON
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)

	var parsed map[string]any
	err := json.Unmarshal([]byte(toolMsg.Content), &parsed)
	assert.NoError(t, err, "tool result should be valid JSON: %s", toolMsg.Content)
	assert.Contains(t, toolMsg.Content, "96") // H20 VRAM
}

func TestChat_WorkflowTool_CreateInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {
			"ImageSet": []any{
				map[string]any{"ImageId": "img-001", "ImageName": "PyTorch 2.1"},
			},
		},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "MachineSizes": []any{
				map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}},
			}},
		}},
		"CheckCompShareResourceCapacity": {"RetCode": 0, "Specs": []any{map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true}}},
		"GetCompShareInstanceUserPrice":  {"RetCode": 0, "PriceDetails": []any{map[string]any{"Price": 1.5}}},
		"CreateCompShareInstance":        {"RetCode": 0, "UHostIds": []any{"uhost-new-001"}},
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-new-001", "State": "Running", "GpuType": "4090"},
			},
		},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Round 1: LLM calls CreateInstanceWorkflow
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "CreateInstanceWorkflow", `{"GpuType":"4090"}`),
		}},
		// Round 2: LLM narrates the workflow result
		{Content: "已成功创建 4090 实例 uhost-new-001"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, confirmFn)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "帮我创建一个4090实例", onStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "uhost-new-001")

	// Verify workflow steps were executed via the executor
	assert.Contains(t, executor.calls, "DescribeCompShareImages")
	assert.Contains(t, executor.calls, "CheckCompShareResourceCapacity")
	assert.Contains(t, executor.calls, "GetCompShareInstanceUserPrice")
	assert.Contains(t, executor.calls, "CreateCompShareInstance")
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")

	// Verify step events were emitted
	hasWorkflowCall := false
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "CreateInstanceWorkflow" {
			hasWorkflowCall = true
		}
	}
	assert.True(t, hasWorkflowCall, "should have a StepToolCall event for CreateInstanceWorkflow")

	// The tool result fed to LLM round 2 should be valid JSON with success
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var result map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &result)
	assert.NoError(t, err, "workflow result should be valid JSON")
	assert.Equal(t, true, result["success"])
}

func TestChat_WorkflowTool_StopInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-stop-001", "State": "Running", "GpuType": "4090", "Name": "test"},
			},
		},
		"StopCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopInstanceWorkflow", `{"UHostId":"uhost-stop-001"}`),
		}},
		{Content: "已关机，注意磁盘仍会收费"},
	}}
	eng := NewWithDeps(mock, executor, confirmFn)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "关机 uhost-stop-001", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")

	// Verify executor received the stop call
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.Contains(t, executor.calls, "StopCompShareInstance")

	// Workflow result should be valid JSON with success
	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	var result map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &result)
	assert.NoError(t, err)
	assert.Equal(t, true, result["success"])
}

func TestChat_WorkflowTool_ArgsFiltered(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"StartCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	mock := &mockLLM{responses: []llm.ChatResponse{
		// LLM passes an extra "evil" parameter
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StartInstanceWorkflow", `{"UHostId":"uhost-start-001","evil":"injection"}`),
		}},
		{Content: "已开机"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, confirmFn)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "开机 uhost-start-001", onStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "开机")

	// Verify that the "evil" param was stripped before entering the workflow
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "StartInstanceWorkflow" {
			assert.NotContains(t, ev.Args, "evil", "evil param should be filtered out")
			assert.Contains(t, ev.Args, "UHostId", "UHostId should be preserved")
		}
	}
}

func TestChat_DiagnosisTool_SSHStopped(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-diag-001", "State": "Stopped"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseSSH", `{"UHostId":"uhost-diag-001"}`),
		}},
		{Content: "诊断结果：实例已关机，需要先开机"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "SSH连不上 uhost-diag-001", onStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "关机")

	assert.Contains(t, executor.calls, "DescribeCompShareInstance")

	hasDiagCall := false
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DiagnoseSSH" {
			hasDiagCall = true
		}
	}
	assert.True(t, hasDiagCall)

	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	var result map[string]any
	err = json.Unmarshal([]byte(toolMsg.Content), &result)
	assert.NoError(t, err)
	assert.Equal(t, true, result["success"])
	assert.Contains(t, result["conclusion"], "关机")
}

func TestChat_DiagnosisTool_InitFailure(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-fail-001", "State": "Install Fail", "CompShareImageName": "PyTorch 2.1"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-fail-001"}`),
		}},
		{Content: "初始化失败，建议删除重建"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "实例初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "初始化失败")
}

func TestChat_DiagnosisTool_ArgsFiltered(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-diag-002", "State": "Running"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-diag-002","evil":"injection"}`),
		}},
		{Content: "done"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "test", onStep)

	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DiagnoseInitFailure" {
			assert.NotContains(t, ev.Args, "evil")
			assert.Contains(t, ev.Args, "UHostId")
		}
	}
}

// ==========================================================================
// Stale-state freshness tests
// ==========================================================================

func TestStaleState_NotePositionIsBeforeLastUserMessage(t *testing.T) {
	// Structural check: when stale, the note must appear immediately before
	// the latest user message, not at index 1. This maximizes model attention
	// in long conversations where the user's ask is far from the system prompt.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DescribeCompShareInstance", `{}`)}},
		{Content: "没有实例"},
		{Content: "好的"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	// Turn 1 triggers DescribeCompShareInstance → lastInstanceQueryTurn = 1
	_, err := eng.Chat(context.Background(), "查看实例", noopStep)
	assert.NoError(t, err)

	// Turn 2: stale condition holds (userTurn=2 > lastInstanceQueryTurn=1)
	_, err = eng.Chat(context.Background(), "帮我关掉 xxx", noopStep)
	assert.NoError(t, err)

	// Inspect the LLM call for turn 2 (index 2 overall: turn1-round0, turn1-round1, turn2-round0).
	turn2Msgs := mock.calls[2].Messages

	// Find the stale note and the last user message.
	noteIdx := -1
	lastUserIdx := -1
	for i, m := range turn2Msgs {
		if m.Role == openai.ChatMessageRoleSystem && strings.Contains(m.Content, "实例状态信息可能已过时") {
			noteIdx = i
		}
		if m.Role == openai.ChatMessageRoleUser {
			lastUserIdx = i // keep overwriting → ends on the last user message
		}
	}
	assert.GreaterOrEqual(t, noteIdx, 0, "stale note must be present")
	assert.GreaterOrEqual(t, lastUserIdx, 0, "last user message must exist")
	assert.Equal(t, lastUserIdx-1, noteIdx,
		"stale note must be the message immediately before the last user message; note at %d, last user at %d",
		noteIdx, lastUserIdx)

	// Extra: last user message must contain the current-turn ask.
	assert.Equal(t, "帮我关掉 xxx", turn2Msgs[lastUserIdx].Content)
}

func TestStaleState_StaleTriggersNote(t *testing.T) {
	// Turn 1: LLM calls DescribeCompShareInstance → freshness updated.
	// Turn 2: LLM gets the stale note because lastInstanceQueryTurn < userTurn.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Turn 1, round 0: tool call
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DescribeCompShareInstance", `{}`)}},
		// Turn 1, round 1: text reply
		{Content: "没有实例"},
		// Turn 2, round 0: text reply (model sees stale note but responds directly)
		{Content: "可以创建一个"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	// Turn 1
	_, err := eng.Chat(context.Background(), "查看实例", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, 1, eng.userTurn)
	assert.Equal(t, 1, eng.lastInstanceQueryTurn)

	// Turn 2: stale note should appear
	_, err = eng.Chat(context.Background(), "帮我关掉 xxx", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, 2, eng.userTurn)

	// The LLM call for turn 2 (index 2 in mock.calls) should contain stale note
	assert.True(t, hasStaleNote(mock.calls[2]),
		"turn 2 LLM call should contain stale-state note")
}

func TestStaleState_FreshNoNote(t *testing.T) {
	// Single turn: LLM calls DescribeCompShareInstance in round 0, then
	// the round 1 LLM call should NOT have a stale note.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Round 0: tool call
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DescribeCompShareInstance", `{}`)}},
		// Round 1: text reply
		{Content: "没有实例"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "查看实例", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, eng.userTurn, eng.lastInstanceQueryTurn, "freshness should equal current turn")

	// Round 1 LLM call (index 1) should NOT have stale note
	assert.False(t, hasStaleNote(mock.calls[1]),
		"same-turn LLM call after fresh query should NOT have stale note")
}

func TestStaleState_WorkflowRefreshesFreshness(t *testing.T) {
	// StopInstanceWorkflow queries a Stopped instance: API succeeds,
	// CheckResult rejects. The freshnessTracker should still update
	// lastInstanceQueryTurn because the API call returned fresh data.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-001", "State": "Stopped",
					"GpuType": "4090", "Name": "test", "ChargeType": "Postpay",
				},
			},
		},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Round 0: LLM calls StopInstanceWorkflow
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopInstanceWorkflow", `{"UHostId":"uhost-001"}`),
		}},
		// Round 1: LLM narrates
		{Content: "实例已经是关机状态"},
	}}
	eng := NewWithDeps(mock, executor, confirmFn)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "关掉 uhost-001", noopStep)
	assert.NoError(t, err)

	// Even though the workflow failed (CheckResult rejected Stopped),
	// the API call for DescribeCompShareInstance succeeded, so freshness
	// should be updated via the executor wrapper.
	assert.Equal(t, eng.userTurn, eng.lastInstanceQueryTurn,
		"workflow internal DescribeCompShareInstance should update freshness even on CheckResult failure")
}

func TestStaleState_InitSnapshotStaleOnFirstTurn(t *testing.T) {
	// After Init(), the instance snapshot is from turn 0. On the first
	// Chat() call (turn 1), the init snapshot IS stale and the stale
	// note should be injected — the user may have changed state via
	// the console between startup and their first write request.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-init", "State": "Running",
					"GpuType": "4090", "GPU": float64(1), "ChargeType": "Postpay",
					"Name": "init-test",
				},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您好"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	_, err := eng.Init(context.Background())
	assert.NoError(t, err)

	// After Init: tracker fired at userTurn=0
	assert.Equal(t, 0, eng.lastInstanceQueryTurn)

	// First Chat
	_, err = eng.Chat(context.Background(), "帮我关掉 uhost-init", noopStep)
	assert.NoError(t, err)

	// Init snapshot is stale → note should be present
	assert.True(t, hasStaleNote(mock.calls[0]),
		"first turn after Init should have stale note (init snapshot is stale)")
}

func TestStaleState_NeverQueriedNoNote(t *testing.T) {
	// When no instance query has ever been made (InitWithContext or
	// no Init at all), there is no stale state to warn about.
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "您好"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("暂无用户信息")

	_, err := eng.Chat(context.Background(), "你好", noopStep)
	assert.NoError(t, err)

	// lastInstanceQueryTurn is -1 (never queried) → no note
	assert.Equal(t, -1, eng.lastInstanceQueryTurn)
	assert.False(t, hasStaleNote(mock.calls[0]),
		"no prior instance query → no stale note")
}

func TestStaleState_FAQNotDerailed(t *testing.T) {
	// Turn 1: DescribeCompShareInstance queried → freshness set.
	// Turn 2: User asks FAQ. Stale note IS injected, but the model
	// should still be free to return a text-only FAQ answer.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Turn 1: tool call + text
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DescribeCompShareInstance", `{}`)}},
		{Content: "没有实例"},
		// Turn 2: FAQ → text only, no tool call
		{Content: "关机后按量模式下，GPU/CPU/内存停止计费，但额外磁盘继续收费。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	// Turn 1
	eng.Chat(context.Background(), "查看实例", noopStep)

	// Turn 2: FAQ
	reply, err := eng.Chat(context.Background(), "关机后还收费吗", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "磁盘")

	// Stale note was injected (mechanical check)
	assert.True(t, hasStaleNote(mock.calls[2]),
		"stale note should be present in turn 2 messages")

	// But model returned text directly — no forced tool call.
	// Executor should have been called only once (turn 1), not in turn 2.
	descCalls := 0
	for _, c := range executor.calls {
		if c == "DescribeCompShareInstance" {
			descCalls++
		}
	}
	assert.Equal(t, 1, descCalls,
		"FAQ turn should not force an extra DescribeCompShareInstance call")
}

func TestStaleState_ExternalStateChangeRegression(t *testing.T) {
	// This is the exact reproduction of the real-account shadow QA bug:
	// Turn 1: Instance is Stopped → agent says "已关机"
	// External change: instance becomes Running
	// Turn 2: Same question → agent MUST re-query, not reuse stale state.

	describeCallCount := 0
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			if action == "DescribeCompShareInstance" {
				describeCallCount++
				if describeCallCount == 1 {
					// Turn 1: Stopped
					return map[string]any{
						"UHostSet": []any{
							map[string]any{"UHostId": "uhost-shadow", "State": "Stopped", "Name": "qa-test", "GpuType": "4090"},
						},
					}, nil
				}
				// Turn 2+: Running (external state change happened)
				return map[string]any{
					"UHostSet": []any{
						map[string]any{"UHostId": "uhost-shadow", "State": "Running", "Name": "qa-test", "GpuType": "4090"},
					},
				}, nil
			}
			return map[string]any{"RetCode": 0}, nil
		},
	}

	mock := &mockLLM{responses: []llm.ChatResponse{
		// Turn 1, round 0: LLM queries instance state
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DescribeCompShareInstance", `{"UHostIds":["uhost-shadow"]}`)}},
		// Turn 1, round 1: LLM replies based on Stopped state
		{Content: "实例 uhost-shadow 已经是关机状态，无需操作。"},
		// Turn 2, round 0: LLM sees stale note → re-queries (correct behavior)
		{ToolCalls: []openai.ToolCall{toolCall("tc2", "DescribeCompShareInstance", `{"UHostIds":["uhost-shadow"]}`)}},
		// Turn 2, round 1: LLM sees Running state → proceeds with stop workflow
		{Content: "实例当前是运行状态，我来帮您关机。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	// Turn 1: "帮我关掉 xxx" → Stopped → "已关机"
	reply1, err := eng.Chat(context.Background(), "帮我关掉 uhost-shadow", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply1, "关机")
	assert.Equal(t, 1, describeCallCount, "turn 1 should query instance once")

	// Turn 2: same question, but external state changed to Running
	reply2, err := eng.Chat(context.Background(), "帮我关掉 uhost-shadow", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply2, "运行")

	// Key assertion: turn 2 MUST have called DescribeCompShareInstance again
	assert.Equal(t, 2, describeCallCount,
		"turn 2 must re-query instance state, not reuse stale 'Stopped' from turn 1")

	// Verify stale note was injected in turn 2's first LLM call
	assert.True(t, hasStaleNote(mock.calls[2]),
		"turn 2 first LLM call should have stale-state note")
}

// ==========================================================================
// ProjectId auto-discovery tests
// ==========================================================================

// projectListHandler mimics the CompShare API endpoint for GetProjectList
// and DescribeCompShareInstance. It records every Action received and lets
// callers override the GetProjectList response body.
type projectListHandler struct {
	mu              sync.Mutex
	actionsReceived []string
	projectListBody string
}

func (h *projectListHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	action := form.Get("Action")
	h.mu.Lock()
	h.actionsReceived = append(h.actionsReceived, action)
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch action {
	case "GetProjectList":
		_, _ = w.Write([]byte(h.projectListBody))
	case "DescribeCompShareInstance":
		_, _ = w.Write([]byte(`{"RetCode": 0, "UHostSet": []}`))
	default:
		_, _ = w.Write([]byte(`{"RetCode": 0}`))
	}
}

func (h *projectListHandler) actions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.actionsReceived))
	copy(out, h.actionsReceived)
	return out
}

func newEngineWithServer(t *testing.T, mock *mockLLM, projectIdInCfg string, body string) (*Engine, *projectListHandler, func()) {
	t.Helper()
	h := &projectListHandler{projectListBody: body}
	srv := httptest.NewServer(h)
	ext := tools.NewExternalExecutor(config.AgentConfig{
		CompShareAPIURL: srv.URL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       projectIdInCfg,
	})
	eng := NewWithDeps(mock, ext, nil)
	return eng, h, srv.Close
}

func TestEnsureProjectId_UsesConfigWhenSet(t *testing.T) {
	// Pre-configured ProjectId → GetProjectList must NOT be called.
	eng, h, cleanup := newEngineWithServer(t, &mockLLM{}, "org-cfg-value", "")
	defer cleanup()

	_, err := eng.Init(context.Background())
	assert.NoError(t, err)

	// Verify the underlying executor still carries the config value.
	ext := unwrapExternalExecutor(eng.executor)
	assert.NotNil(t, ext)
	assert.Equal(t, "org-cfg-value", ext.ProjectId())

	// GetProjectList should not have been called.
	for _, a := range h.actions() {
		assert.NotEqual(t, "GetProjectList", a,
			"GetProjectList should not be called when config provides ProjectId")
	}
}

func TestEnsureProjectId_FetchesWhenUnset_PicksDefault(t *testing.T) {
	// No config value → GetProjectList called → IsDefault=true wins over first.
	body := `{
		"RetCode": 0,
		"ProjectSet": [
			{"ProjectId": "org-first", "IsDefault": false},
			{"ProjectId": "org-default", "IsDefault": true},
			{"ProjectId": "org-third", "IsDefault": false}
		]
	}`
	eng, h, cleanup := newEngineWithServer(t, &mockLLM{}, "", body)
	defer cleanup()

	_, err := eng.Init(context.Background())
	assert.NoError(t, err)

	ext := unwrapExternalExecutor(eng.executor)
	assert.NotNil(t, ext)
	assert.Equal(t, "org-default", ext.ProjectId(),
		"IsDefault=true entry must win over first entry")

	// GetProjectList should appear in recorded actions.
	actions := h.actions()
	found := false
	for _, a := range actions {
		if a == "GetProjectList" {
			found = true
			break
		}
	}
	assert.True(t, found, "GetProjectList must be called when ProjectId unset; got %v", actions)
}

func TestEnsureProjectId_FallsBackToFirstWhenNoDefault(t *testing.T) {
	body := `{
		"RetCode": 0,
		"ProjectSet": [
			{"ProjectId": "org-first"},
			{"ProjectId": "org-second"}
		]
	}`
	eng, _, cleanup := newEngineWithServer(t, &mockLLM{}, "", body)
	defer cleanup()

	_, err := eng.Init(context.Background())
	assert.NoError(t, err)

	ext := unwrapExternalExecutor(eng.executor)
	assert.Equal(t, "org-first", ext.ProjectId())
}

func TestEnsureProjectId_SilentOnMalformed(t *testing.T) {
	// Empty ProjectSet → no panic, ProjectId stays empty.
	body := `{"RetCode": 0, "ProjectSet": []}`
	eng, _, cleanup := newEngineWithServer(t, &mockLLM{}, "", body)
	defer cleanup()

	_, err := eng.Init(context.Background())
	assert.NoError(t, err, "Init must not fail when GetProjectList returns empty set")

	ext := unwrapExternalExecutor(eng.executor)
	assert.Equal(t, "", ext.ProjectId())
}

func TestEnsureProjectId_SkipsForMockExecutor(t *testing.T) {
	// mockExecutor is not *tools.ExternalExecutor → ensureProjectId is a no-op.
	// This guards against tests crashing when they don't use the real executor.
	mockExec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, mockExec, nil)

	_, err := eng.Init(context.Background())
	assert.NoError(t, err)

	// No GetProjectList call should be made through the mock.
	for _, a := range mockExec.calls {
		assert.NotEqual(t, "GetProjectList", a,
			"non-external executor path must not call GetProjectList")
	}
}

func TestPickProjectId(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"nil", nil, ""},
		{"no ProjectSet", map[string]any{"RetCode": float64(0)}, ""},
		{"empty set", map[string]any{"ProjectSet": []any{}}, ""},
		{
			"single entry",
			map[string]any{"ProjectSet": []any{
				map[string]any{"ProjectId": "org-only"},
			}},
			"org-only",
		},
		{
			"default wins",
			map[string]any{"ProjectSet": []any{
				map[string]any{"ProjectId": "org-a"},
				map[string]any{"ProjectId": "org-b", "IsDefault": true},
			}},
			"org-b",
		},
		{
			"first when no default",
			map[string]any{"ProjectSet": []any{
				map[string]any{"ProjectId": "org-a"},
				map[string]any{"ProjectId": "org-b"},
			}},
			"org-a",
		},
		{
			"skips empty ProjectId",
			map[string]any{"ProjectSet": []any{
				map[string]any{"ProjectId": ""},
				map[string]any{"ProjectId": "org-real"},
			}},
			"org-real",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pickProjectId(tc.resp))
		})
	}
}

// ==========================================================================
// Billing stale-state hard guard
// ==========================================================================

func TestExtractDiagnosisTargets(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"uhost_only", map[string]any{"UHostId": "uhost-1paxrg4g1vfw"}, []string{"uhost-1paxrg4g1vfw"}},
		{"name_only", map[string]any{"Name": "qa-shadow-20260417-01"}, []string{"qa-shadow-20260417-01"}},
		{"both", map[string]any{"UHostId": "uhost-1paxrg4g1vfw", "Name": "qa-shadow-20260417-01"}, []string{"uhost-1paxrg4g1vfw", "qa-shadow-20260417-01"}},
		{"empty_map", map[string]any{}, nil},
		{"empty_strings", map[string]any{"UHostId": "", "Name": ""}, nil},
		{"non_string", map[string]any{"UHostId": 123, "Name": true}, nil},
		{"mixed_one_valid", map[string]any{"UHostId": "uhost-x", "Name": ""}, []string{"uhost-x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, extractDiagnosisTargets(tc.args))
		})
	}
}

func TestShouldForceBillingDiagnosis(t *testing.T) {
	cases := []struct {
		name        string
		engine      func() *Engine
		userMsg     string
		wantTrigger bool
	}{
		{
			name: "positive_target_plus_keyword",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"uhost-1paxrg4g1vfw"}
				return e
			},
			userMsg:     "那为什么 uhost-1paxrg4g1vfw 还在扣费",
			wantTrigger: true,
		},
		{
			// P1 fix: target alone must NOT trigger the billing guard.
			// Same instance name can appear in restart/release/SSH intents
			// that are unrelated to billing.
			name: "negative_target_only_restart_intent",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"qa-shadow-20260417-01"}
				return e
			},
			userMsg:     "帮我重启 qa-shadow-20260417-01",
			wantTrigger: false,
		},
		{
			name: "negative_target_only_release_intent",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"qa-shadow-20260417-01"}
				return e
			},
			userMsg:     "qa-shadow-20260417-01 怎么释放？",
			wantTrigger: false,
		},
		{
			name: "negative_target_only_ssh_intent",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"qa-shadow-20260417-01"}
				return e
			},
			userMsg:     "SSH 连不上 qa-shadow-20260417-01",
			wantTrigger: false,
		},
		{
			// Same instance AND billing keyword — legitimate billing follow-up.
			name: "positive_target_plus_billing_keyword_mixed",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"uhost-A"}
				return e
			},
			userMsg:     "uhost-A 的费用为什么这么高",
			wantTrigger: true,
		},
		{
			name: "positive_billing_keyword_no_target",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"uhost-xxx"}
				return e
			},
			userMsg:     "那为什么还在扣费",
			wantTrigger: true,
		},
		{
			name: "positive_费用_keyword",
			engine: func() *Engine {
				return &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
			},
			userMsg:     "这个费用怎么计算的",
			wantTrigger: true,
		},
		{
			name: "negative_no_prior_diagnosis",
			engine: func() *Engine {
				return &Engine{userTurn: 1, lastDiagnosisTool: "", lastDiagnosisTurn: -1}
			},
			userMsg:     "为什么还在扣费",
			wantTrigger: false,
		},
		{
			name: "negative_prior_was_ssh_not_billing",
			engine: func() *Engine {
				return &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseSSH", lastDiagnosisTurn: 1}
			},
			userMsg:     "那为什么还在扣费",
			wantTrigger: false,
		},
		{
			name: "negative_non_adjacent_turn",
			engine: func() *Engine {
				e := &Engine{userTurn: 3, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"uhost-x"}
				return e
			},
			userMsg:     "那为什么 uhost-x 还在扣费",
			wantTrigger: false,
		},
		{
			name: "negative_no_target_no_keyword",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"uhost-x"}
				return e
			},
			userMsg:     "怎么释放实例？",
			wantTrigger: false,
		},
		{
			name: "negative_faq_question",
			engine: func() *Engine {
				e := &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
				e.lastDiagnosisTargets = []string{"uhost-x"}
				return e
			},
			userMsg:     "4090 的显存有多大",
			wantTrigger: false,
		},
		{
			name: "positive_empty_targets_billing_keyword_fires",
			engine: func() *Engine {
				return &Engine{userTurn: 2, lastDiagnosisTool: "DiagnoseBilling", lastDiagnosisTurn: 1}
			},
			userMsg:     "计费是怎么算的",
			wantTrigger: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.engine()
			assert.Equal(t, tc.wantTrigger, e.shouldForceBillingDiagnosis(tc.userMsg))
		})
	}
}

// billingScenarioExecutor returns a mockExecutor configured with the
// DescribeCompShareInstance result needed for DiagnoseBilling to complete
// its two-step chain.
func billingScenarioExecutor(state string) *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":       "uhost-bill-001",
					"Name":          "qa-bill",
					"State":         state,
					"ChargeType":    "Dynamic",
					"InstancePrice": 0.70,
					"DiskPrice":     0.00,
					"GPU":           float64(1),
					"GpuType":       "3080Ti",
				},
			},
		},
	}}
}

// toolChoiceForBilling returns true iff req.ToolChoice names DiagnoseBilling.
func toolChoiceForBilling(req llm.ChatRequest) bool {
	return toolChoiceForAction(req, "DiagnoseBilling")
}

// toolChoiceForMonitor returns true iff req.ToolChoice names GetCompShareInstanceMonitor.
func toolChoiceForMonitor(req llm.ChatRequest) bool {
	return toolChoiceForAction(req, "GetCompShareInstanceMonitor")
}

func toolChoiceForAction(req llm.ChatRequest, action string) bool {
	tc, ok := req.ToolChoice.(openai.ToolChoice)
	if !ok {
		return false
	}
	return tc.Type == openai.ToolTypeFunction && tc.Function.Name == action
}

func monitorScenarioExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"RetCode": 0,
			"Data": map[string]any{
				"DataSet": []any{},
			},
		},
	}}
}

func TestMonitorFreshnessGuard_ForcesRefreshOnMetricFollowUp(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "监控数据已返回"},
		{Content: "没有重新查询监控"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看看 uhost-monitor-001 的监控数据", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, executor.calls, "GetCompShareInstanceMonitor")

	_, err = eng.Chat(context.Background(), "帮我判断一下有没有机器 CPU、内存、GPU 或显存占用异常高", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.True(t, toolChoiceForMonitor(mock.calls[2]),
			"monitor metric follow-up should force ToolChoice=GetCompShareInstanceMonitor")
	}
	for i := 0; i < 2; i++ {
		assert.Nil(t, mock.calls[i].ToolChoice, "call %d should not have ToolChoice", i)
	}
}

func TestMonitorFreshnessGuard_ExplicitReuseDoesNotTrigger(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "监控数据已返回"},
		{Content: "基于刚才的数据总结"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看看 uhost-monitor-001 的监控数据", noopStep)
	assert.NoError(t, err)

	_, err = eng.Chat(context.Background(), "基于刚才的监控数据总结一下有没有异常", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"explicit reuse phrasing should let the model answer from prior monitor data")
	}
}

func TestResourceInfoGuard_ForcesInstanceDiscoveryForExpiryRenewalQuery(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"RetCode": 0,
				"UHostSet": []any{
					map[string]any{
						"UHostId":    "uhost-expire-001",
						"Name":       "包日实例",
						"ChargeType": "Day",
						"ExpireTime": float64(1777442400),
						"AutoRenew":  true,
					},
					map[string]any{"UHostId": "uhost-expire-002", "Name": "postpay-2", "ChargeType": "Dynamic", "ExpireTime": float64(0), "AutoRenew": "Yes"},
					map[string]any{"UHostId": "uhost-expire-003", "Name": "postpay-3", "ChargeType": "Dynamic", "ExpireTime": float64(0), "AutoRenew": "Yes"},
					map[string]any{"UHostId": "uhost-expire-004", "Name": "postpay-4", "ChargeType": "Dynamic", "ExpireTime": float64(0), "AutoRenew": "No"},
					map[string]any{"UHostId": "uhost-expire-005", "Name": "postpay-5", "ChargeType": "Dynamic", "ExpireTime": float64(0), "AutoRenew": "No"},
					map[string]any{"UHostId": "uhost-expire-006", "Name": "prepaid-six", "ChargeType": "Day", "ExpireTime": float64(1777528800), "AutoRenew": "No"},
				},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":100}`),
		}},
		{Content: "包日实例已开启自动续费，到期时间为 2026-04-29。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "我的机器什么时候到期，哪些开了自动续费？", noopStep)
	assert.NoError(t, err)

	if assert.Len(t, mock.calls, 2) {
		assert.True(t, toolChoiceForAction(mock.calls[0], "DescribeCompShareInstance"))
		assert.True(t, requestContainsMessageText(mock.calls[1], "ExpireTime"),
			"LLM must receive ExpireTime from DescribeCompShareInstance")
		assert.True(t, requestContainsMessageText(mock.calls[1], "AutoRenew"),
			"LLM must receive AutoRenew from DescribeCompShareInstance")
		assert.True(t, requestContainsMessageText(mock.calls[1], "ResourceInfoSummary"),
			"resource info queries must add a non-array summary that survives tool-result truncation")
		assert.True(t, requestContainsMessageText(mock.calls[1], "prepaid-six"),
			"resource summary must include instances beyond the first five truncated UHostSet entries")
	}
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.Contains(t, reply, "自动续费")
}

func TestExecuteTool_TreatsEmptyArgumentsAsEmptyObject(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"RetCode": 0, "UHostSet": []any{}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			{
				ID:   "tc-empty",
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      "DescribeCompShareInstance",
					Arguments: "",
				},
			},
		}},
		{Content: "ok"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	onStep, events := collectSteps()

	_, err := eng.Chat(context.Background(), "列一下我的机器", onStep)
	assert.NoError(t, err)

	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	for _, ev := range *events {
		assert.NotEqual(t, StepError, ev.Type, "empty tool arguments should be treated as {}")
	}
}

func TestMonitorIntentGuard_ForcesInstanceDiscoveryForModelMonitorQuery(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "wrong route"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "帮我看下昨天下午两点的4090监控", noopStep)
	assert.NoError(t, err)

	if assert.Len(t, mock.calls, 1) {
		assert.True(t, toolChoiceForAction(mock.calls[0], "DescribeCompShareInstance"),
			"monitor query scoped by GPU model should discover user instances before specs or monitor calls")
	}
}

func TestMonitorHistoryBatchGuard_BlocksMultiInstanceHistoryFollowUp(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "请选择一台实例，或者全部逐台查询"},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001","uhost-monitor-002"],"StartTime":1776492000,"EndTime":1776492060}`),
		}},
		{Content: "需要逐台单实例查询"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "帮我看下昨天下午两点的4090监控", onStep)
	assert.NoError(t, err)
	_, err = eng.Chat(context.Background(), "全部 4090 实例都查一下", onStep)
	assert.NoError(t, err)

	assert.NotContains(t, executor.calls, "GetCompShareInstanceMonitor",
		"historical monitor with multiple UHostIds must be blocked before the external API")
	hasBlocked := false
	for _, ev := range *events {
		if ev.Type == StepBlocked && ev.Action == "GetCompShareInstanceMonitor" {
			hasBlocked = true
			assert.Contains(t, ev.Message, "历史时间")
			assert.Contains(t, ev.Message, "逐台")
		}
	}
	assert.True(t, hasBlocked, "expected StepBlocked for multi-instance historical monitor query")
}

func TestMonitorHistoryBatchGuard_AllowsCurrentMultiInstanceSnapshot(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001","uhost-monitor-002"]}`),
		}},
		{Content: "最近 60 秒监控快照"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看看所有运行中机器的监控", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, executor.calls, "GetCompShareInstanceMonitor",
		"current multi-instance monitor snapshot should still be allowed")
}

func TestMonitorTemporalContextNote_InjectedBeforeInstanceClarification(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"RetCode": 0, "UHostSet": []any{}},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":100}`),
		}},
		{Content: "请选择一台 4090 实例"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "帮我看下昨天下午两点的4090监控", noopStep)
	assert.NoError(t, err)

	if assert.Len(t, mock.calls, 2) {
		for _, req := range mock.calls {
			assert.True(t, requestContainsMessageText(req, "2026-04-29 13:45"),
				"LLM must see the resolved monitor window before any clarification text")
			assert.True(t, requestContainsMessageText(req, "2026-04-29 14:15"),
				"LLM must see the resolved monitor window before any clarification text")
			assert.True(t, requestContainsMessageText(req, "StartTime=1777441500"),
				"LLM must see the deterministic StartTime")
			assert.True(t, requestContainsMessageText(req, "EndTime=1777443300"),
				"LLM must see the deterministic EndTime")
		}
	}
}

func TestMonitorHistoricalQuery_ContinuesAfterNeedlessConfirmation(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {"RetCode": 0, "UHostSet": []any{}},
			"GetCompShareInstanceMonitor": {
				"RetCode": 0,
				"Data": map[string]any{
					"DataSet": []any{},
				},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":100}`),
		}},
		{Content: "需要我继续逐台查询昨天下午两点的监控吗？"},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc2", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"],"StartTime":1,"EndTime":2}`),
		}},
		{Content: "已查询 2026-04-29 的历史监控。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我看下昨天下午两点的4090监控", noopStep)
	assert.NoError(t, err)

	assert.NotContains(t, reply, "需要我继续")
	assert.Contains(t, executor.calls, "GetCompShareInstanceMonitor")
	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.True(t, toolChoiceForAction(mock.calls[2], "GetCompShareInstanceMonitor"),
			"engine should force monitor after model stops at a needless confirmation")
	}
}

func TestMonitorTemporalFinalReplyGuard_CorrectsWrongModelDate(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"GetCompShareInstanceMonitor": {
				"RetCode": 0,
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"UHostId": "uhost-monitor-001",
							"Metrics": []any{
								map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(1)}}}}},
							},
						},
					},
				},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"],"StartTime":1,"EndTime":2}`),
		}},
		{Content: "昨天下午两点（2025-06-30 14:00）的历史监控不在范围内，14:00 ~ 14:30 当前实时监控如下。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我看下昨天下午两点的4090监控", noopStep)
	assert.NoError(t, err)

	assert.NotContains(t, reply, "2025-06-30")
	assert.Contains(t, reply, "2026-04-29")
	assert.NotContains(t, reply, "当前实时监控")
	assert.Contains(t, reply, "该历史时间窗监控")
	assert.NotContains(t, reply, "14:00 ~ 14:30")
	assert.Contains(t, reply, "13:45 ~ 14:15")
}

func TestMonitorHistoricalNoData_FinalReplyDoesNotInventMetrics(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"GetCompShareInstanceMonitor": {
				"RetCode": 0,
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"UHostId": "uhost-monitor-001",
							"Metrics": []any{
								map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{}},
								map[string]any{"MetricKey": "cloudwatch_gpu_util", "Results": []any{}},
							},
						},
					},
				},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"],"StartTime":1,"EndTime":2}`),
		}},
		{Content: "当前实时监控如下：CPU 99%，GPU 88%。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我看下昨天 14:00-15:00 的 uhost-monitor-001 监控", noopStep)
	assert.NoError(t, err)

	assert.Contains(t, reply, "没有返回有效监控数据")
	assert.NotContains(t, reply, "99%")
	assert.NotContains(t, reply, "88%")
	assert.NotContains(t, reply, "当前实时监控")
}

func TestMonitorTimeArgNormalizer_CorrectsYesterdayExplicitHourRange(t *testing.T) {
	var gotArgs map[string]any
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			gotArgs = args
			return map[string]any{"RetCode": 0, "Data": map[string]any{"List": []any{}}}, nil
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"],"StartTime":1,"EndTime":2}`),
		}},
		{Content: "昨天 14:00-15:00 监控"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "帮我看下昨天 14:00-15:00 的 uhost-monitor-001 监控", noopStep)
	assert.NoError(t, err)

	assert.Equal(t, int64(1777442400), gotInt64Arg(gotArgs, "StartTime"))
	assert.Equal(t, int64(1777446000), gotInt64Arg(gotArgs, "EndTime"))
}

func TestMonitorTimeArgNormalizer_CorrectsYesterdayAfternoonTwo(t *testing.T) {
	var gotArgs map[string]any
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			gotArgs = args
			return map[string]any{"RetCode": 0, "Data": map[string]any{"List": []any{}}}, nil
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"],"StartTime":1776492000,"EndTime":1776495600}`),
		}},
		{Content: "昨天 14:00 监控"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "帮我看下昨天下午两点的 qa-shadow 监控", noopStep)
	assert.NoError(t, err)

	assert.Equal(t, int64(1777441500), gotInt64Arg(gotArgs, "StartTime"),
		"yesterday 14:00 Beijing should normalize to 2026-04-29 13:45")
	assert.Equal(t, int64(1777443300), gotInt64Arg(gotArgs, "EndTime"),
		"point-time monitor should use a +/-15 minute window")
}

func TestMonitorTimeArgNormalizer_CorrectsPastThirtyMinutes(t *testing.T) {
	var gotArgs map[string]any
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			gotArgs = args
			return map[string]any{"RetCode": 0, "Data": map[string]any{"List": []any{}}}, nil
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"],"StartTime":1,"EndTime":2}`),
		}},
		{Content: "过去30分钟监控"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.nowFn = func() time.Time {
		return time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone)
	}
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看一下这台机器过去 30 分钟的监控", noopStep)
	assert.NoError(t, err)

	wantEnd := time.Date(2026, 4, 30, 17, 40, 0, 0, beijingZone).Unix()
	assert.Equal(t, wantEnd-1800, gotInt64Arg(gotArgs, "StartTime"))
	assert.Equal(t, wantEnd, gotInt64Arg(gotArgs, "EndTime"))
}

func gotInt64Arg(args map[string]any, key string) int64 {
	switch v := args[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func TestShouldForceMonitorRefresh(t *testing.T) {
	cases := []struct {
		name        string
		engine      *Engine
		userMsg     string
		wantTrigger bool
	}{
		{
			name:        "positive_metric_follow_up",
			engine:      &Engine{userTurn: 2, lastMonitorQueryTurn: 1},
			userMsg:     "帮我判断一下有没有机器 CPU、内存、GPU 或显存占用异常高",
			wantTrigger: true,
		},
		{
			name:        "positive_gpu_idle_follow_up",
			engine:      &Engine{userTurn: 2, lastMonitorQueryTurn: 1},
			userMsg:     "只看刚才那台机器的 GPU 和显存监控，告诉我有没有 GPU 空闲或显存占满",
			wantTrigger: true,
		},
		{
			name:        "negative_no_prior_monitor",
			engine:      &Engine{userTurn: 1, lastMonitorQueryTurn: -1},
			userMsg:     "有没有 CPU 占用异常高",
			wantTrigger: false,
		},
		{
			name:        "negative_non_adjacent_turn",
			engine:      &Engine{userTurn: 3, lastMonitorQueryTurn: 1},
			userMsg:     "有没有 GPU 空闲或显存占满",
			wantTrigger: false,
		},
		{
			name:        "negative_explicit_reuse",
			engine:      &Engine{userTurn: 2, lastMonitorQueryTurn: 1},
			userMsg:     "基于刚才的监控数据总结一下有没有异常",
			wantTrigger: false,
		},
		{
			name:        "negative_account_billing_boundary",
			engine:      &Engine{userTurn: 2, lastMonitorQueryTurn: 1},
			userMsg:     "查一下我这个账号本月总账单、余额和消费明细",
			wantTrigger: false,
		},
		{
			name:        "negative_gpu_billing_boundary",
			engine:      &Engine{userTurn: 2, lastMonitorQueryTurn: 1},
			userMsg:     "这台 GPU 费用为什么这么高",
			wantTrigger: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantTrigger, tc.engine.shouldForceMonitorRefresh(tc.userMsg))
		})
	}
}

func TestSystemMessageTimePrefix_InjectedOnInitAndRefreshedEachChat(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "ok turn 1"},
		{Content: "ok turn 2"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	bj := time.FixedZone("CST", 8*3600)
	clock := []time.Time{
		time.Date(2026, 4, 30, 14, 35, 0, 0, bj),
		time.Date(2026, 5, 1, 9, 12, 0, 0, bj),
	}
	idx := 0
	eng.nowFn = func() time.Time {
		t := clock[idx]
		if idx < len(clock)-1 {
			idx++
		}
		return t
	}
	eng.InitWithContext("test user")

	assert.True(t, strings.HasPrefix(eng.messages[0].Content, "当前北京时间：2026-04-30 14:35"),
		"Init must seed messages[0] with the literal '当前北京时间：' prefix in YYYY-MM-DD HH:MM format")

	_, err := eng.Chat(context.Background(), "随便问一句", noopStep)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(eng.messages[0].Content, "当前北京时间：2026-05-01 09:12"),
		"each Chat must refresh the time prefix in place, preserving the literal prefix")

	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role,
		"messages[0] must remain a system message after refresh")
	assert.Contains(t, eng.messages[0].Content, "你是优云算力共享平台的 AI 助手",
		"refresh must preserve the static system prompt body")
}

func TestAccountBillingUnsupported_MonthlySummaryHardBlocks(t *testing.T) {
	// Account-level monthly summary phrasings (本月/当月/月度 + cost word,
	// no instance scope) must hard-block. Empirically deepseek-v4-flash
	// violates a prompt-only soft guidance and calls DiagnoseBilling on
	// these, so the hard-block is required regardless of system prompt.
	cases := []string{
		"我账号下本月花了多少钱",
		"我这个账户本月费用",
		"我本月在平台总共消费了多少",
		"当月账单是多少",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			executor := &mockExecutor{}
			mock := &mockLLM{responses: []llm.ChatResponse{
				{Content: "should never be called"},
			}}
			eng := NewWithDeps(mock, executor, nil)
			eng.InitWithContext("test user")

			reply, err := eng.Chat(context.Background(), msg, noopStep)
			assert.NoError(t, err)
			assert.Empty(t, mock.calls, "monthly account summary must short-circuit, never reach the LLM")
			assert.Empty(t, executor.calls, "monthly account summary must not call any external tool")
			assert.Contains(t, reply, "财务中心")
		})
	}
}

func TestAccountBillingUnsupported_AccountOnlyDataIgnoresInstanceWords(t *testing.T) {
	// 余额 / 总账单 / 消费流水 / 流水 / balance live in the financial
	// center, never in any per-instance API. Instance words in the same
	// sentence MUST NOT veto the hard-block, otherwise the LLM may try
	// to fabricate or guess via DescribeCompShareInstance.
	cases := []string{
		"这些机器导致账号余额还剩多少",
		"每台机器的消费流水",
		"哪台实例占用了我账号余额",
		"我那台 GPU 实例的 balance 还有多少",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			executor := &mockExecutor{}
			mock := &mockLLM{responses: []llm.ChatResponse{
				{Content: "should never be called"},
			}}
			eng := NewWithDeps(mock, executor, nil)
			eng.InitWithContext("test user")

			reply, err := eng.Chat(context.Background(), msg, noopStep)
			assert.NoError(t, err)
			assert.Empty(t, mock.calls,
				"account-only data words (余额/流水/balance) must hard-block even with instance words present")
			assert.Empty(t, executor.calls)
			assert.Contains(t, reply, "财务中心")
		})
	}
}

func TestAccountBillingUnsupported_HijackByInstanceScopeFallsThrough(t *testing.T) {
	cases := []string{
		"查我账号下哪台实例消费最高",
		"我账户里这些机器的费用占比",
		"账号下哪些主机本月扣费最多",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			mock := &mockLLM{responses: []llm.ChatResponse{
				{Content: "fall through to LLM"},
			}}
			eng := NewWithDeps(mock, &mockExecutor{}, nil)
			eng.InitWithContext("test user")

			_, err := eng.Chat(context.Background(), msg, noopStep)
			assert.NoError(t, err)
			assert.NotEmpty(t, mock.calls,
				"messages mentioning both account and instance scope must NOT short-circuit")
		})
	}
}

func TestAccountBillingUnsupported_ReturnsWithoutLLMOrTools(t *testing.T) {
	executor := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "查一下我这个账号本月总账单、余额和消费流水明细", noopStep)
	assert.NoError(t, err)

	assert.Empty(t, mock.calls, "account-level billing must not call the LLM/tool loop")
	assert.Empty(t, executor.calls, "account-level billing must not call external tools")
	assert.Contains(t, reply, "不支持")
	assert.Contains(t, reply, "财务中心")
}

func TestBillingIntentGuard_ForcesDiagnosisOnInstanceFeeFirstTurn(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "没有调用诊断"},
	}}
	eng := NewWithDeps(mock, billingScenarioExecutor("Running"), nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "查一下我当前这些实例的费用明细，按按量/后付费、包日/包月分开列", noopStep)
	assert.NoError(t, err)

	if assert.Len(t, mock.calls, 1) {
		assert.True(t, toolChoiceForBilling(mock.calls[0]),
			"instance fee detail question should force ToolChoice=DiagnoseBilling")
	}
}

func TestBillingIntentGuard_ForcesDiagnosisOnShutdownBillingFirstTurn(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "没有调用诊断"},
	}}
	eng := NewWithDeps(mock, billingScenarioExecutor("Stopped"), nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么我的机器关机后还在扣费？帮我找出哪些关机实例还可能产生费用", noopStep)
	assert.NoError(t, err)

	if assert.Len(t, mock.calls, 1) {
		assert.True(t, toolChoiceForBilling(mock.calls[0]),
			"shutdown billing question should force ToolChoice=DiagnoseBilling")
	}
}

func TestMixedMonitorBillingIntent_RunsMonitorThenBilling(t *testing.T) {
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": {
				"RetCode": 0,
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-mixed-001", "Name": "mixed", "State": "Running", "GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic"},
				},
			},
			"GetCompShareInstanceMonitor": {
				"RetCode": 0,
				"Data": map[string]any{
					"List": []any{
						map[string]any{
							"UHostId": "uhost-mixed-001",
							"Metrics": []any{
								map[string]any{"MetricKey": "uhost_cpu_used", "Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(90)}}}}},
							},
						},
					},
				},
			},
			"GetCompShareInstancePrice": {
				"RetCode": 0,
				"Infos":   []any{map[string]any{"GPUType": "4090", "Price": float64(1.58)}},
			},
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":100}`),
		}},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc2", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-mixed-001"]}`),
		}},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc3", "DiagnoseBilling", `{}`),
		}},
		{Content: "监控异常和扣费情况已分别查询。"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "账号里这些机器哪台监控异常，哪台扣费多？", onStep)
	assert.NoError(t, err)

	assert.Contains(t, reply, "监控")
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.Contains(t, executor.calls, "GetCompShareInstanceMonitor")
	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.True(t, toolChoiceForAction(mock.calls[0], "DescribeCompShareInstance"))
		assert.True(t, toolChoiceForAction(mock.calls[1], "GetCompShareInstanceMonitor"))
		assert.True(t, toolChoiceForAction(mock.calls[2], "DiagnoseBilling"))
	}
	sawBilling := false
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "DiagnoseBilling" {
			sawBilling = true
		}
	}
	assert.True(t, sawBilling, "mixed monitor+billing query must also run DiagnoseBilling")
}

func TestBillingStaleGuard_ForcesRediagnosisOnFollowUp(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`),
		}},
		{Content: "实例 Running，按量 0.70/h"},
		{Content: "重新诊断后状态已变化"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么 uhost-bill-001 在扣费", noopStep)
	assert.NoError(t, err)

	assert.Equal(t, "DiagnoseBilling", eng.lastDiagnosisTool)
	assert.Equal(t, 1, eng.lastDiagnosisTurn)
	assert.Contains(t, eng.lastDiagnosisTargets, "uhost-bill-001")

	_, err = eng.Chat(context.Background(), "那为什么 uhost-bill-001 还在扣费", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.True(t, toolChoiceForBilling(mock.calls[2]),
			"turn 2 round 1 should force ToolChoice=DiagnoseBilling")
	}
	for i := 0; i < 2; i++ {
		assert.Nil(t, mock.calls[i].ToolChoice, "call %d should not have ToolChoice", i)
	}
}

func TestBillingStaleGuard_BillingKeywordWithoutTargetAlsoFires(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`),
		}},
		{Content: "实例 Running，按量 0.70/h"},
		{Content: "重新诊断后..."},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么 uhost-bill-001 在扣费", noopStep)
	assert.NoError(t, err)

	_, err = eng.Chat(context.Background(), "那为什么还在扣费", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.True(t, toolChoiceForBilling(mock.calls[2]),
			"billing keyword follow-up without instance name should still force DiagnoseBilling")
	}
}

func TestBillingStaleGuard_UnrelatedFollowUpDoesNotTrigger(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`),
		}},
		{Content: "实例 Running，按量 0.70/h"},
		{Content: "释放实例请到控制台..."},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么 uhost-bill-001 在扣费", noopStep)
	assert.NoError(t, err)

	_, err = eng.Chat(context.Background(), "怎么释放实例？", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"cross-domain follow-up must not trigger DiagnoseBilling hard guard")
	}
}

func TestBillingStaleGuard_FAQFollowUpDoesNotTrigger(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`),
		}},
		{Content: "实例 Running，按量 0.70/h"},
		{Content: "4090 的显存是 24GB"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么 uhost-bill-001 在扣费", noopStep)
	assert.NoError(t, err)

	_, err = eng.Chat(context.Background(), "4090 的显存有多大", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"FAQ follow-up must not trigger DiagnoseBilling hard guard")
	}
}

func TestBillingStaleGuard_NonAdjacentTurnDoesNotTrigger(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`),
		}},
		{Content: "实例 Running，按量 0.70/h"},
		{Content: "4090 显存 24GB"},
		{Content: "billing has not changed"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么 uhost-bill-001 在扣费", noopStep)
	assert.NoError(t, err)
	_, err = eng.Chat(context.Background(), "4090 显存多大", noopStep)
	assert.NoError(t, err)
	_, err = eng.Chat(context.Background(), "那为什么 uhost-bill-001 还在扣费", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 4) {
		assert.Nil(t, mock.calls[3].ToolChoice,
			"turn 3 is non-adjacent to turn 1's DiagnoseBilling — must not trigger")
	}
}

// P1 regression: same-instance follow-up with a non-billing intent
// (restart/release/SSH) must not be hijacked into DiagnoseBilling.
func TestBillingStaleGuard_SameInstanceRestartDoesNotTrigger(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`),
		}},
		{Content: "实例 Running，按量 0.70/h"},
		// Turn 2: restart the same instance. Model chooses freely.
		{Content: "已准备重启 uhost-bill-001..."},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "为什么 uhost-bill-001 在扣费", noopStep)
	assert.NoError(t, err)

	// Follow-up mentions the same instance but the intent is restart, not billing.
	_, err = eng.Chat(context.Background(), "帮我重启 uhost-bill-001", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"restart intent on the same instance must not trigger DiagnoseBilling hard guard")
	}
}

func TestBillingStaleGuard_OtherDiagnosisDoesNotTrigger(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-ssh-001", "State": "Stopped"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseSSH", `{"UHostId":"uhost-ssh-001"}`),
		}},
		{Content: "SSH 连不上，实例已关机"},
		{Content: "not my job"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "SSH 连不上 uhost-ssh-001", noopStep)
	assert.NoError(t, err)

	assert.Equal(t, "", eng.lastDiagnosisTool)

	_, err = eng.Chat(context.Background(), "那为什么还在扣费", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"prior DiagnoseSSH must not trigger DiagnoseBilling hard guard")
	}
}

func TestNormalizeMsg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"trim leading trailing spaces", "  hello  ", "hello"},
		{"collapse internal spaces", "foo   bar", "foo bar"},
		{"collapse tabs and newlines", "foo\t\nbar", "foo bar"},
		{"lowercase ascii", "Install Fail", "install fail"},
		{"preserve chinese", "初始化失败", "初始化失败"},
		{"mixed ascii chinese", " Install  Fail 初始化", "install fail 初始化"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeMsg(tc.in))
		})
	}
}

func TestContainsInitFailureSignal(t *testing.T) {
	positives := []string{
		"初始化失败了",
		"Install Fail",
		"install fail",
		"卡在初始化",
		"卡在启动",
		"starting很久",
		"一直 starting",
		"uhost-xxx 初始化失败",
	}
	negatives := []string{
		"跑崩了",
		"挂了",
		"有问题",
		"帮我扫一下所有有问题的实例",
		"uhost-xxx 崩了",
		"昨晚那台不行了",
		"",
	}
	for _, msg := range positives {
		t.Run("positive/"+msg, func(t *testing.T) {
			assert.True(t, containsInitFailureSignal(msg), "want true for %q", msg)
		})
	}
	for _, msg := range negatives {
		t.Run("negative/"+msg, func(t *testing.T) {
			assert.False(t, containsInitFailureSignal(msg), "want false for %q", msg)
		})
	}
}

func TestContainsScanAllSignal(t *testing.T) {
	positives := []string{
		"帮我看看哪些实例初始化失败了",
		"帮我扫全部",
		"全部失败的实例都查一下",
		"都有哪些失败的",
		"所有实例的状态",
		"有哪些实例挂了",
		"扫一下失败的",
	}
	negatives := []string{
		"跑崩了",
		"昨晚那台挂了",
		"uhost-xxx 有问题",
		"wyptest 那台",
		"",
	}
	for _, msg := range positives {
		t.Run("positive/"+msg, func(t *testing.T) {
			assert.True(t, containsScanAllSignal(msg), "want true for %q", msg)
		})
	}
	for _, msg := range negatives {
		t.Run("negative/"+msg, func(t *testing.T) {
			assert.False(t, containsScanAllSignal(msg), "want false for %q", msg)
		})
	}
}

// initFailureScenarioExecutor returns a mockExecutor with a minimal
// UHostSet so that DiagnoseInitFailure's chain can execute when allowed
// past the guard. The host state is Install Fail so chain completion
// has something meaningful to report in the passing tests.
func initFailureScenarioExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":            "uhost-init-001",
					"Name":               "wyptest",
					"State":              "Install Fail",
					"CompShareImageName": "cuda130_torch291_py312",
				},
			},
		},
	}}
}

const vagueClarifyPrefix = "请问是哪台实例出了问题？"

func TestVagueCrashGuard_VagueNoTargetBlocked(t *testing.T) {
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "昨晚那台跑崩了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, vagueClarifyPrefix,
		"vague failure with no target must trigger Gate 1 clarification")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance",
		"guard must stop the chain before any API call")
}

func TestVagueCrashGuard_VagueWithTargetBlocked(t *testing.T) {
	// P1 regression: guard must fire even when the LLM provides a target,
	// because the user's symptom description is still vague.
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-init-001"}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "uhost-init-001 跑崩了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, vagueClarifyPrefix,
		"vague failure wording must trigger Gate 1 even when target is known")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance")
}

func TestVagueCrashGuard_VagueScanAllBlocked(t *testing.T) {
	// P2 regression: scan-all phrasing alone must NOT bypass the guard when
	// the user has not named an init-failure symptom. "所有有问题的实例"
	// is vague — could be SSH, GPU, billing, etc.
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我扫一下所有有问题的实例", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, vagueClarifyPrefix,
		"scan-all phrasing without init-failure signal must still be blocked")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance")
}

const specificClarifyPrefix = "请问是哪台实例的初始化失败了？"

func TestVagueCrashGuard_SpecificNoTargetBlocked(t *testing.T) {
	// Gate 1 passes (has init-failure signal), Gate 2 fires (no target, no scan-all).
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "昨晚那台卡在初始化了", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, specificClarifyPrefix,
		"init-failure signal but no target must trigger Gate 2 clarification")
	assert.NotContains(t, executor.calls, "DescribeCompShareInstance")
}

func TestVagueCrashGuard_UHostIdTargetPasses(t *testing.T) {
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{"UHostId":"uhost-init-001"}`),
		}},
		{Content: "实例初始化失败，建议重建"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "uhost-init-001 初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.NotContains(t, reply, specificClarifyPrefix)
	assert.NotContains(t, reply, vagueClarifyPrefix)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
}

func TestVagueCrashGuard_ExplicitInitFailureScanAllPasses(t *testing.T) {
	// Gate 1 passes (init-failure signal), Gate 2 passes (scan-all intent).
	executor := initFailureScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DiagnoseInitFailure", `{}`),
		}},
		{Content: "共发现 1 台初始化失败的实例"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我看看哪些实例初始化失败了", noopStep)
	assert.NoError(t, err)
	assert.NotContains(t, reply, specificClarifyPrefix)
	assert.NotContains(t, reply, vagueClarifyPrefix)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance",
		"scan-all must be allowed when both init-failure signal and scan-all phrasing are present")
}
