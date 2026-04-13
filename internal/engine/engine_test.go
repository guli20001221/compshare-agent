package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
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

// --- Helpers ---

func noopStep(StepEvent) {}

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
	assert.Contains(t, toolMsg.Content, "24")  // VRAM
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
	assert.Contains(t, systemMsg.Content, "关机后还扣费吗")
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
		"CheckCompShareResourceCapacity": {"RetCode": 0, "Available": true},
		"GetCompShareInstancePrice":      {"RetCode": 0, "Price": 1.5},
		"CreateCompShareInstance":         {"RetCode": 0, "UHostIds": []any{"uhost-new-001"}},
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
	assert.Contains(t, executor.calls, "GetCompShareInstancePrice")
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
				map[string]any{"UHostId": "uhost-fail-001", "State": "InstallFail", "CompShareImageName": "PyTorch 2.1"},
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
