package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	grounded "github.com/compshare-agent/internal/renderer"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

type scriptedRateLimiter struct {
	decisions []governance.Decision
	requests  []governance.Request
}

func (l *scriptedRateLimiter) Allow(req governance.Request) governance.Decision {
	l.requests = append(l.requests, req)
	if len(l.decisions) == 0 {
		return governance.Decision{
			Allowed:     true,
			Class:       req.Class,
			Action:      req.Action,
			SubjectHash: req.SubjectKey,
		}
	}
	decision := l.decisions[0]
	l.decisions = l.decisions[1:]
	if decision.Class == "" {
		decision.Class = req.Class
	}
	if decision.Action == "" {
		decision.Action = req.Action
	}
	if decision.SubjectHash == "" {
		decision.SubjectHash = req.SubjectKey
	}
	return decision
}

type scriptedIntentPlanner struct {
	results []intent.PlannerResult
	calls   []intent.PlannerInput
	err     error
}

func (p *scriptedIntentPlanner) Plan(_ context.Context, input intent.PlannerInput) (intent.PlannerResult, error) {
	p.calls = append(p.calls, input)
	if p.err != nil {
		return intent.PlannerResult{}, p.err
	}
	if len(p.results) == 0 {
		return intent.PlannerResult{Fallback: true, Plan: unknownEngineTestPlan()}, nil
	}
	result := p.results[0]
	p.results = p.results[1:]
	return result, nil
}

type scriptedKnowledgeRetriever struct {
	results []knowledge.RetrievalResult
	calls   []knowledgeRetrievalCall
}

type knowledgeRetrievalCall struct {
	question    string
	productArea string
}

func (r *scriptedKnowledgeRetriever) Retrieve(question, productArea string) knowledge.RetrievalResult {
	r.calls = append(r.calls, knowledgeRetrievalCall{
		question:    question,
		productArea: productArea,
	})
	if len(r.results) == 0 {
		return knowledge.RetrievalResult{Enabled: true, Empty: true}
	}
	result := r.results[0]
	r.results = r.results[1:]
	if len(result.HitItems) == 0 && len(result.Hits) > 0 {
		result.HitItems = make([]knowledge.RetrievalHit, 0, len(result.Hits))
		for _, chunk := range result.Hits {
			result.HitItems = append(result.HitItems, knowledge.RetrievalHit{Chunk: chunk, Score: 80, Kept: true})
		}
	}
	return result
}

type mockGroundedRenderer struct {
	result   grounded.RenderResult
	requests []grounded.RenderRequest
}

func (r *mockGroundedRenderer) Render(_ context.Context, req grounded.RenderRequest) grounded.RenderResult {
	r.requests = append(r.requests, req)
	return r.result
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

func collectSteps() (func(StepEvent), *[]StepEvent) {
	var events []StepEvent
	return func(ev StepEvent) { events = append(events, ev) }, &events
}

func assertStepWithType(t *testing.T, events []StepEvent, typ StepType, action, contains string) {
	t.Helper()
	for _, ev := range events {
		if ev.Type == typ && ev.Action == action && strings.Contains(ev.Message, contains) {
			return
		}
	}
	t.Fatalf("missing step type=%v action=%s containing %q in %#v", typ, action, contains, events)
}

func assertNoStepTypeForAction(t *testing.T, events []StepEvent, typ StepType, action string) {
	t.Helper()
	for _, ev := range events {
		if ev.Type == typ && ev.Action == action {
			t.Fatalf("unexpected step type=%v action=%s: %#v", typ, action, ev)
		}
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() {
		os.Stderr = old
	}()

	fn()

	require.NoError(t, w.Close())
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
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

func toolNames(registry []openai.Tool) []string {
	names := make([]string, 0, len(registry))
	for _, tool := range registry {
		if tool.Function != nil {
			names = append(names, tool.Function.Name)
		}
	}
	return names
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

func TestChat_ExternalToolEventsCarryTraceMetadata(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-1"}}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":1}`),
		}},
		{Content: "ok"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "查实例", onStep)
	assert.NoError(t, err)
	assert.Equal(t, "ok", reply)

	var callEvent, resultEvent *StepEvent
	for i := range *events {
		ev := &(*events)[i]
		if ev.Action != "DescribeCompShareInstance" {
			continue
		}
		switch ev.Type {
		case StepToolCall:
			callEvent = ev
		case StepToolResult:
			resultEvent = ev
		}
	}
	if assert.NotNil(t, callEvent) {
		assert.Equal(t, observability.ToolSourceMainReAct, callEvent.Source)
		assert.Equal(t, map[string]any{"Limit": float64(1)}, callEvent.Args)
	}
	if assert.NotNil(t, resultEvent) {
		assert.Equal(t, observability.ToolSourceMainReAct, resultEvent.Source)
		assert.Equal(t, 1, resultEvent.Attempts)
		assert.NotNil(t, resultEvent.TraceResult)
		assert.Contains(t, resultEvent.TraceResult, "UHostSet")
	}
}

func TestChat_ExternalToolReadRetriesTransientError(t *testing.T) {
	attempts := 0
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			if action != "DescribeCompShareInstance" {
				return map[string]any{"RetCode": 0}, nil
			}
			attempts++
			if attempts == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return map[string]any{"RetCode": 0, "UHostSet": []any{}}, nil
		},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{}`),
		}},
		{Content: "retry succeeded"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "list instances", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, "retry succeeded", reply)
	assert.Equal(t, 2, attempts, "direct external read tools should be retried through SafeToolExecutor")

	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	assert.Contains(t, toolMsg.Content, "UHostSet")
	assert.NotContains(t, toolMsg.Content, "API")
}

// Reference-only legacy scenario kept for future historical-monitor re-enable.
// It is intentionally not a Test* while this stage rejects history windows.
func legacyChat_HistoricalMonitorNoDataFinalReplyAndTurnReset(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"RetCode": 0,
			"Data": []any{
				map[string]any{"UHostId": "uhost-1", "MonitorSet": []any{}},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-1"],"StartTime":1777442400,"EndTime":1777444200}`),
		}},
		{Content: "当前实时监控显示 CPU 99%，GPU 88%。"},
		{Content: "第二轮普通回复 CPU 99%。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "看 2026-04-29 14:00 的监控", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "北京时间 2026-04-29 14:00 ~ 2026-04-29 14:30")
	assert.Contains(t, reply, "uhost-1")
	assert.Contains(t, reply, "没有返回有效监控数据")
	assert.NotContains(t, reply, "CPU 99")
	assert.NotContains(t, reply, "GPU 88")

	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Contains(t, toolMsg.Content, "MonitorDataStatus")

	reply, err = eng.Chat(context.Background(), "这轮不查工具", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, "第二轮普通回复 CPU 99%。", reply)
}

// Reference-only legacy scenario kept for future historical-monitor re-enable.
// It is intentionally not a Test* while this stage rejects history windows.
func legacyChat_HistoricalMonitorFinalReplyCorrectsWindowWording(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"RetCode": 0,
			"Data": []any{
				map[string]any{
					"UHostId": "uhost-1",
					"Metrics": []any{
						map[string]any{"Results": []any{map[string]any{"Values": []any{map[string]any{"Value": float64(42)}}}}},
					},
				},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-1"],"StartTime":1777442400,"EndTime":1777444200}`),
		}},
		{Content: "当前实时监控显示 2025-06-30 13:00 ~ 13:30 CPU 42%。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "看 2026-04-29 14:00 的监控", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "该历史时间窗监控")
	assert.Contains(t, reply, "2026-04-29")
	assert.Contains(t, reply, "14:00 ~ 14:30")
	assert.Contains(t, reply, "CPU 42")
	assert.NotContains(t, reply, "2025-06-30")
	assert.NotContains(t, reply, "当前实时监控")
}

func TestChat_HistoricalMonitorToolCallBlockedBeforeExecution(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{
		ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-1"],"StartTime":1777442400,"EndTime":1777444200}`),
		},
	}}}
	executor := &mockExecutor{}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "show monitor data", noopStep)

	require.NoError(t, err)
	assert.Equal(t, monitorHistoryUnsupportedReply, reply)
	assert.Empty(t, executor.calls, "historical monitor tool call must be blocked before API execution")
}

func TestGuardMonitorTemporalFinalReplyCorrectsWindowWording(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)
	eng.currentMonitorWindow = true
	eng.currentMonitorStart = 1777442400
	eng.currentMonitorEnd = 1777444200

	reply := eng.guardMonitorTemporalFinalReply("historical monitor shows 2025-06-30 13:00 ~ 13:30 CPU 42%")

	assert.Contains(t, reply, "2026-04-29")
	assert.Contains(t, reply, "14:00 ~ 14:30")
	assert.Contains(t, reply, "CPU 42")
	assert.NotContains(t, reply, "2025-06-30")
}

func TestGuardMonitorTemporalFinalReplyCorrectsChineseClockRangeWording(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)
	eng.currentMonitorWindow = true
	eng.currentMonitorStart = 1777442400
	eng.currentMonitorEnd = 1777444200

	reply := eng.guardMonitorTemporalFinalReply("\u5386\u53f2\u76d1\u63a7\u663e\u793a 2025-06-30 8\u70b9\u523010\u70b9 CPU 42\u3002")

	assert.Contains(t, reply, "14:00 ~ 14:30")
	assert.Contains(t, reply, "CPU 42")
	assert.NotContains(t, reply, "8\u70b9\u523010\u70b9")
	assert.NotContains(t, reply, "2025-06-30")
}

func TestChat_ClearHistoricalMonitorQuestionBlockedBeforeReAct(t *testing.T) {
	cases := []string{
		"\u770b\u6628\u5929 8\u70b9\u523010\u70b9 CPU \u76d1\u63a7",
		"\u8fc7\u53bb\u4e00\u5c0f\u65f6 CPU \u76d1\u63a7",
		"\u8fd1 24 \u5c0f\u65f6\u663e\u5b58",
		"\u4e0a\u5468 GPU \u5229\u7528\u7387",
		"2026-05-08 \u7684\u76d1\u63a7",
		"show yesterday cpu monitor",
		"last night GPU monitor",
		"last 24 hours CPU",
		"\u6628\u665a\u8fd9\u53f0\u673a\u5668\u5fd9\u4e0d\u5fd9",
		"was it idle last night",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			mock := &mockLLM{responses: []llm.ChatResponse{{
				ToolCalls: []openai.ToolCall{
					toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-1"],"StartTime":1777442400,"EndTime":1777444200}`),
				},
			}}}
			executor := &mockExecutor{}
			eng := NewWithDeps(mock, executor, nil)
			eng.InitWithContext("test user")

			reply, err := eng.Chat(context.Background(), msg, noopStep)

			require.NoError(t, err)
			assert.Equal(t, monitorHistoryUnsupportedReply, reply)
			assert.Empty(t, mock.calls, "clear historical monitor question must not enter ReAct")
			assert.Empty(t, executor.calls)
		})
	}
}

func TestChat_CurrentMonitorQuestionNotBlockedByHistoricalGuard(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react current monitor"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "\u73b0\u5728 CPU \u76d1\u63a7\u600e\u4e48\u6837", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "react current monitor", reply)
	assert.Len(t, mock.calls, 1)
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

func TestChat_LLMRateLimitDenialSkipsLLM(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{{
		Allowed:     false,
		Reason:      governance.ReasonQPSExceeded,
		SubjectHash: "sha256:subject",
		Err:         governance.ErrRateLimited,
	}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "hello", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "请求过于频繁，请稍后再试。", reply)
	assert.Empty(t, mock.calls, "denied LLM request must not call LLM")
	require.Len(t, limiter.requests, 1)
	assert.Equal(t, governance.ClassLLM, limiter.requests[0].Class)
	assert.Equal(t, "main_react_chat", limiter.requests[0].Action)
}

func TestChat_LLMRateLimitDailyDenialUsesDailyMessage(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{{
		Allowed:     false,
		Reason:      governance.ReasonDailyExceeded,
		SubjectHash: "sha256:subject",
		Err:         governance.ErrRateLimited,
	}}}
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "hello", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "今日额度已用完，请明天再试。", reply)
	assert.Empty(t, mock.calls)
}

func TestChat_LLMRateLimitAllowPreservesBehavior(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{{
		Allowed:     true,
		SubjectHash: "sha256:subject",
	}}}
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "hello", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "ok", reply)
	assert.Len(t, mock.calls, 1)
}

func TestChat_LLMRateLimitDecisionObserverReceivesHashedSubject(t *testing.T) {
	rawPublicKey := "public-key-that-must-not-appear"
	subjectHash, ok := governance.SubjectKeyFromPublicKey(rawPublicKey)
	require.True(t, ok)
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{{
		Allowed:     false,
		Reason:      governance.ReasonQPSExceeded,
		SubjectHash: subjectHash,
		Err:         governance.ErrRateLimited,
	}}}
	eng.rateLimitSubject = subjectHash
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	var observed []governance.Decision
	eng.SetRateLimitObserver(func(decision governance.Decision) {
		observed = append(observed, decision)
	})

	_, err := eng.Chat(context.Background(), "hello", noopStep)

	require.NoError(t, err)
	require.Len(t, observed, 1)
	assert.Equal(t, subjectHash, observed[0].SubjectHash)
	assert.NotContains(t, fmt.Sprintf("%+v", observed[0]), rawPublicKey)
}

func TestChat_MutatingRateLimitDenialSkipsConfirmAndExecutor(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
		{Content: "should not be used"},
	}}
	executor := &mockExecutor{}
	confirmCalls := 0
	eng := NewWithDeps(mock, executor, func(action string, args map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonQPSExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
	}}
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "stop it", onStep)

	require.NoError(t, err)
	assert.Equal(t, rateLimitQPSMessage, reply)
	assert.Equal(t, 0, confirmCalls, "quota denial must happen before L1 confirmation")
	assert.Empty(t, executor.calls, "quota denial must happen before API execution")
	assert.Len(t, mock.calls, 1, "quota denial should stop the turn without another LLM round")
	require.Len(t, eng.rateLimiter.(*scriptedRateLimiter).requests, 2)
	assert.Equal(t, governance.ClassLLM, eng.rateLimiter.(*scriptedRateLimiter).requests[0].Class)
	assert.Equal(t, governance.ClassMutatingTool, eng.rateLimiter.(*scriptedRateLimiter).requests[1].Class)
	assert.Equal(t, "StopCompShareInstance", eng.rateLimiter.(*scriptedRateLimiter).requests[1].Action)
	assertStepWithType(t, *events, StepBlocked, "StopCompShareInstance", rateLimitQPSMessage)
	for _, ev := range *events {
		assert.NotEqual(t, StepConfirmNeeded, ev.Type, "quota denial must not ask for confirmation")
		if ev.Type == StepBlocked && ev.Action == "StopCompShareInstance" {
			assert.Equal(t, observability.ToolCappedRateLimit, ev.Capped)
			assert.Equal(t, rateLimitQPSMessage, ev.CapReason)
		}
	}
}

func TestChat_WorkflowMutatingRateLimitDenialMarksRateLimitCap(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopInstanceWorkflow", `{"UHostId":"uhost-xxx"}`),
		}},
	}}
	executor := &mockExecutor{}
	eng := NewWithDeps(mock, executor, func(action string, args map[string]any) bool { return true })
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonQPSExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
	}}
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "stop workflow", onStep)

	require.NoError(t, err)
	assert.Equal(t, rateLimitQPSMessage, reply)
	assert.Empty(t, executor.calls)
	found := false
	for _, ev := range *events {
		if ev.Type == StepBlocked && ev.Action == "StopInstanceWorkflow" {
			found = true
			assert.Equal(t, observability.ToolCappedRateLimit, ev.Capped)
			assert.Equal(t, rateLimitQPSMessage, ev.CapReason)
		}
	}
	assert.True(t, found, "missing workflow quota StepBlocked event")
}

func TestChat_MutatingRateLimitDailyDenialUsesDailyMessage(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StartCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
	}}
	executor := &mockExecutor{}
	confirmCalls := 0
	eng := NewWithDeps(mock, executor, func(action string, args map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonDailyExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
	}}
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "start it", noopStep)

	require.NoError(t, err)
	assert.Equal(t, rateLimitDailyMessage, reply)
	assert.Equal(t, 0, confirmCalls)
	assert.Empty(t, executor.calls)
}

func TestChat_MutatingRateLimitAllowsWorkflowWithoutCountingInternalSteps(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-stop-001", "State": "Running", "GpuType": "4090", "Name": "test"},
			},
		},
		"StopCompShareInstance": {"RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopInstanceWorkflow", `{"UHostId":"uhost-stop-001"}`),
		}},
		{Content: "stopped"},
	}}
	eng := NewWithDeps(mock, executor, func(action string, args map[string]any) bool {
		return true
	})
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: true, SubjectHash: "sha256:subject"},
	}}
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "stop workflow", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "stopped", reply)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.Contains(t, executor.calls, "StopCompShareInstance")
	var mutating []governance.Request
	for _, req := range limiter.requests {
		if req.Class == governance.ClassMutatingTool {
			mutating = append(mutating, req)
		}
	}
	require.Len(t, mutating, 1, "workflow should consume one mutating quota for the top-level workflow only")
	assert.Equal(t, "StopInstanceWorkflow", mutating[0].Action)
}

func TestChat_ReadExpensiveRateLimitDenialBecomesToolResult(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":10}`),
		}},
		{Content: "please narrow"},
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonQPSExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
		{Allowed: true, SubjectHash: "sha256:subject"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "list", onStep)

	require.NoError(t, err)
	assert.Equal(t, "please narrow", reply)
	assert.Empty(t, executor.calls, "read-expensive quota denial must happen before API execution")
	require.Len(t, limiter.requests, 3)
	assert.Equal(t, governance.ClassLLM, limiter.requests[0].Class)
	assert.Equal(t, governance.ClassReadExpensiveTool, limiter.requests[1].Class)
	assert.Equal(t, "DescribeCompShareInstance", limiter.requests[1].Action)
	assert.Equal(t, governance.ClassLLM, limiter.requests[2].Class)
	assert.Len(t, mock.calls, 2, "quota denial should be returned as a tool result for LLM narration")
	assertStepWithType(t, *events, StepBlocked, "DescribeCompShareInstance", rateLimitQPSMessage)
	assertNoStepTypeForAction(t, *events, StepError, "DescribeCompShareInstance")
}

func TestChat_ReadExpensiveTargetCapBecomesToolResult(t *testing.T) {
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = fmt.Sprintf("uhost-%02d", i)
	}
	rawArgs, err := json.Marshal(map[string]any{"UHostIds": ids})
	require.NoError(t, err)
	executor := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", string(rawArgs)),
		}},
		{Content: "scope narrowed"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "monitor all", onStep)

	require.NoError(t, err)
	assert.Equal(t, "scope narrowed", reply)
	assert.Empty(t, executor.calls)
	assert.Len(t, mock.calls, 2)
	assertStepWithType(t, *events, StepBlocked, "GetCompShareInstanceMonitor", toolCapExceededMessage)
	assertNoStepTypeForAction(t, *events, StepError, "GetCompShareInstanceMonitor")
}

func TestWorkflowInternalReadExpensiveConsumesSubjectQuotaButSkipsTurnBudget(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{"UHostId": "uhost-stop-001", "State": "Running", "Zone": "cn-wlcb-01"}},
		},
		"StopCompShareInstance": {"RetCode": 0},
	}}
	limiter := &scriptedRateLimiter{}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.readExpensiveCallsThisTurn = maxReadExpensiveCallsPerTurn

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-stop-001"}, noopStep)

	assert.Contains(t, reply, `"success":true`)
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.Contains(t, executor.calls, "StopCompShareInstance")
	var readExpensive []governance.Request
	for _, req := range limiter.requests {
		if req.Class == governance.ClassReadExpensiveTool {
			readExpensive = append(readExpensive, req)
		}
	}
	require.Len(t, readExpensive, 1)
	assert.Equal(t, "DescribeCompShareInstance", readExpensive[0].Action)
}

func TestWorkflowInternalReadExpensiveQuotaDenialReturnsFriendlyMessage(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{"UHostId": "uhost-stop-001", "State": "Running", "Zone": "cn-wlcb-01"}},
		},
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonQPSExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	onStep, events := collectSteps()

	reply := eng.executeWorkflow(context.Background(), "StopInstanceWorkflow", map[string]any{"UHostId": "uhost-stop-001"}, onStep)

	assert.Equal(t, finalReplyPrefix+rateLimitQPSMessage, reply)
	assert.Empty(t, executor.calls, "workflow internal quota denial must stop before API execution")
	require.Len(t, limiter.requests, 2)
	assert.Equal(t, governance.ClassMutatingTool, limiter.requests[0].Class)
	assert.Equal(t, governance.ClassReadExpensiveTool, limiter.requests[1].Class)
	assertStepWithType(t, *events, StepBlocked, "DescribeCompShareInstance", rateLimitQPSMessage)
	assertStepWithType(t, *events, StepBlocked, "StopInstanceWorkflow", rateLimitQPSMessage)
	assertNoStepTypeForAction(t, *events, StepError, "DescribeCompShareInstance")
}

func TestDiagnosisInternalReadExpensiveCountsTurnBudget(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-diag-001", "State": "Running"}}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.rateLimiter = &scriptedRateLimiter{}
	eng.rateLimitSubject = "sha256:subject"
	eng.userTurn = 1
	eng.readExpensiveCallsThisTurn = maxReadExpensiveCallsPerTurn
	onStep, events := collectSteps()

	reply := eng.executeDiagnosis(context.Background(), "DiagnoseSSH", map[string]any{"UHostId": "uhost-diag-001"}, onStep)

	assert.Equal(t, finalReplyPrefix+readExpensiveTurnBudgetMessage, reply)
	assert.Empty(t, executor.calls, "diagnosis internal read-expensive calls must stop when turn budget is exhausted")
	assertStepWithType(t, *events, StepBlocked, "DescribeCompShareInstance", readExpensiveTurnBudgetMessage)
	assertStepWithType(t, *events, StepBlocked, "DiagnoseSSH", readExpensiveTurnBudgetMessage)
	assertNoStepTypeForAction(t, *events, StepError, "DescribeCompShareInstance")
}

func TestDiagnoseBillingConsumesMultipleReadExpensiveQuotaUnits(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-bill-001", "State": "Running", "ChargeType": "Dynamic",
				"InstancePrice": float64(1), "DiskPrice": float64(0.1),
			}},
		},
	}}
	limiter := &scriptedRateLimiter{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"

	reply := eng.executeDiagnosis(context.Background(), "DiagnoseBilling", map[string]any{}, noopStep)

	assert.Contains(t, reply, "uhost-bill-001")
	var readExpensive []governance.Request
	for _, req := range limiter.requests {
		if req.Class == governance.ClassReadExpensiveTool {
			readExpensive = append(readExpensive, req)
		}
	}
	require.Len(t, readExpensive, 2, "DiagnoseBilling intentionally consumes quota for both list and price-detail Describe calls")
	assert.Equal(t, "DescribeCompShareInstance", readExpensive[0].Action)
	assert.Equal(t, "DescribeCompShareInstance", readExpensive[1].Action)
}

func TestInitReadExpensiveQuotaDenialDoesNotFailStartup(t *testing.T) {
	mock := &mockLLM{}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{{
		Allowed: false, Reason: governance.ReasonDailyExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited,
	}}}
	eng.rateLimitSubject = "sha256:subject"

	stderr := captureStderr(t, func() {
		suggestions, err := eng.Init(context.Background())
		require.NoError(t, err)
		assert.NotEmpty(t, suggestions)
	})

	assert.Contains(t, stderr, rateLimitDailyMessage)
	assert.Equal(t, string(entity.SyncEventFailed), eng.registry.TraceState(time.Now()).SyncEvent)
}

func TestChat_ReadOnlyToolDoesNotConsumeMutatingQuota(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{"Limit":10}`),
		}},
		{Content: "listed"},
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{{Allowed: true, SubjectHash: "sha256:subject"}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "list", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "listed", reply)
	require.Len(t, limiter.requests, 3)
	for _, req := range limiter.requests {
		assert.NotEqual(t, governance.ClassMutatingTool, req.Class)
	}
	assert.Equal(t, governance.ClassReadExpensiveTool, limiter.requests[1].Class)
}

func TestChat_L2BlockedToolDoesNotConsumeMutatingQuota(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "TerminateCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{{Allowed: true, SubjectHash: "sha256:subject"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	_, err := eng.Chat(context.Background(), "terminate", noopStep)

	require.NoError(t, err)
	require.Len(t, limiter.requests, 1)
	assert.Equal(t, governance.ClassLLM, limiter.requests[0].Class)
}

func TestNewConstructsRateLimiterFromConfig(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		PublicKey: "public-key-for-subject",
		LLM: config.LLMConfig{
			BaseURL: "https://api.modelverse.cn/v1",
			APIKey:  "llm-key",
			Model:   "deepseek-v4-flash",
		},
		RateLimit: config.RateLimitConfig{
			LLMQPS:        1,
			LLMDaily:      10,
			MutatingQPS:   1,
			MutatingDaily: 5,
		},
	}}

	eng := New(cfg, nil)

	require.NotNil(t, eng.rateLimiter)
	wantSubject, ok := governance.SubjectKeyFromPublicKey("public-key-for-subject")
	require.True(t, ok)
	assert.Equal(t, wantSubject, eng.rateLimitSubject)
	assert.NotContains(t, eng.rateLimitSubject, "public-key-for-subject")
}

func TestNewDefaultsToReadOnlyMutatingToolsDisabled(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		PublicKey: "public-key-for-subject",
		LLM: config.LLMConfig{
			BaseURL: "https://api.modelverse.cn/v1",
			APIKey:  "llm-key",
			Model:   "deepseek-v4-flash",
		},
	}}

	eng := New(cfg, nil)
	require.False(t, eng.mutatingToolsEnabled)

	eng.InitWithContext("test user")
	require.NotEmpty(t, eng.messages)
	system := eng.messages[0].Content
	assert.NotContains(t, system, "StopInstanceWorkflow")
	assert.Contains(t, system, "当前阶段不直接执行")
}

func TestChatReadOnlyHidesWorkflowToolsFromLLM(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetMutatingToolsEnabled(false)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "帮我关机", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "ok", reply)
	require.Len(t, mock.calls, 1)
	names := toolNames(mock.calls[0].Tools)
	assert.NotContains(t, names, "StopInstanceWorkflow")
	assert.NotContains(t, names, "CreateInstanceWorkflow")
	assert.Contains(t, names, "DescribeCompShareInstance")
	assert.Contains(t, names, "DiagnoseSSH")
}

func TestChatReadOnlyBlocksWorkflowToolCall(t *testing.T) {
	executor := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopInstanceWorkflow", `{"UHostId":"uhost-stop-001"}`),
		}},
		{Content: "blocked"},
	}}
	eng := NewWithDeps(mock, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(false)
	eng.InitWithContext("test user")
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "帮我关机", onStep)

	require.NoError(t, err)
	assert.Equal(t, "blocked", reply)
	assert.Empty(t, executor.calls)
	assertStepWithType(t, *events, StepBlocked, "StopInstanceWorkflow", "当前阶段不直接执行")
}

func TestExecuteSafeToolReadOnlyBlocksDirectMutatingAction(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(false)

	_, err := eng.executeSafeTool(context.Background(), tools.SafeToolRequest{
		Action: "StartCompShareInstance",
		Args:   map[string]any{"UHostId": "uhost-start-001"},
		Origin: tools.OriginDirectLLM,
	})

	require.ErrorIs(t, err, tools.ErrMutatingActionDisabled)
	assert.Empty(t, executor.calls)
}

func TestChatStepToolCallRedactsArgsBeforeTrace(t *testing.T) {
	llmMock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("call-1", "ResetCompShareInstancePassword", `{"UHostId":"uhost-1","Password":"Secret123!"}`),
		}},
		{Content: "done"},
	}}
	exec := &mockExecutor{results: map[string]map[string]any{
		"ResetCompShareInstancePassword": {"RetCode": 0},
	}}
	eng := NewWithDeps(llmMock, exec, func(string, map[string]any) bool { return true })

	var callEvent *StepEvent
	reply, err := eng.Chat(context.Background(), "reset password", func(ev StepEvent) {
		if ev.Type == StepToolCall && ev.Action == "ResetCompShareInstancePassword" {
			copy := ev
			callEvent = &copy
		}
	})

	require.NoError(t, err)
	require.Equal(t, "done", reply)
	require.NotNil(t, callEvent)
	assert.Equal(t, "[REDACTED]", callEvent.Args["Password"])
}

func TestNewWarnsWhenPublicKeyMissingForRateLimiter(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{
			BaseURL: "https://api.modelverse.cn/v1",
			APIKey:  "llm-key",
			Model:   "deepseek-v4-flash",
		},
		RateLimit: config.RateLimitConfig{
			LLMQPS:        1,
			LLMDaily:      10,
			MutatingQPS:   1,
			MutatingDaily: 5,
		},
	}}

	var eng *Engine
	stderr := captureStderr(t, func() {
		eng = New(cfg, nil)
	})

	require.NotNil(t, eng)
	assert.Equal(t, governance.AnonymousSubjectKey, eng.rateLimitSubject)
	assert.Contains(t, stderr, "rate limiter using anonymous subject")
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
	filtered := tools.NewSafeToolExecutor(&mockExecutor{}).FilterArgs("GetCompShareInstancePrice", args)

	assert.Contains(t, filtered, "Zone")
	assert.Contains(t, filtered, "GpuType")
	assert.NotContains(t, filtered, "injected_evil")
	assert.NotContains(t, filtered, "__proto__")
}

func TestFilterAllowedParams_PassesThroughUnknownTool(t *testing.T) {
	args := map[string]any{"foo": "bar"}
	filtered := tools.NewSafeToolExecutor(&mockExecutor{}).FilterArgs("NonexistentTool", args)
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
	eng.rateLimiter = governance.NewMemoryLimiter(governance.DefaultLimits(), governance.WithClock(func() time.Time {
		return time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)
	}))
	eng.rateLimitSubject = "sha256:create-workflow"
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
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
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
	eng.registry = entity.NewRegistry(entity.WithClock(func() time.Time { return now }))
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"RetCode":    0,
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-stop-001", "Name": "test", "State": "Running"},
		},
	}, string(entity.SyncEventInit)))
	require.False(t, eng.registry.NeedsRefresh(now.Add(time.Second)))
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
	assert.True(t, eng.registry.NeedsRefresh(now.Add(time.Second)))
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

func TestNewDoesNotRefreshEntityRegistry(t *testing.T) {
	h := &projectListHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()

	eng := New(&config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{
			BaseURL: "https://api.modelverse.cn/v1",
			Model:   "deepseek-v4-flash",
		},
		CompShareAPIURL: srv.URL,
		PublicKey:       "pk",
		PrivateKey:      "sk",
		Region:          "cn-wlcb",
		ProjectId:       "org-cfg-value",
	}}, nil)

	assert.NotNil(t, eng.registry)
	assert.Empty(t, h.actions(), "Engine.New must not perform network refresh")
	assert.Equal(t, string(entity.SyncEventUnavailable), eng.registry.TraceState(time.Now()).SyncEvent)
}

func TestInitRefreshesEntityRegistryThroughSafeExecutor(t *testing.T) {
	attempts := 0
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action != "DescribeCompShareInstance" {
			return map[string]any{"Action": action, "RetCode": 0}, nil
		}
		attempts++
		assert.Equal(t, 100, args["Limit"])
		if attempts == 1 {
			return nil, io.EOF
		}
		return map[string]any{
			"RetCode":    0,
			"TotalCount": float64(1),
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-init", "Name": "init-host", "State": "Running"},
			},
		}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)

	_, err := eng.Init(context.Background())

	assert.NoError(t, err)
	assert.Equal(t, 2, attempts, "init refresh must go through SafeToolExecutor read retry")
	snap := eng.registry.Snapshot()
	assert.Equal(t, string(entity.SyncEventInit), snap.SyncEvent)
	assert.NotEmpty(t, snap.SnapshotID)
	assert.Contains(t, snap.Instances, "uhost-init")
}

func TestRegistryInvalidatesAfterSuccessfulMutatingTool(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	executor := &mockExecutor{results: map[string]map[string]any{
		"StartCompShareInstance": {"RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StartCompShareInstance", `{"UHostId":"uhost-a"}`),
		}},
		{Content: "started"},
	}}
	eng := NewWithDeps(mock, executor, func(string, map[string]any) bool { return true })
	eng.registry = entity.NewRegistry(entity.WithClock(func() time.Time { return now }))
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"RetCode":    0,
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "a", "State": "Stopped"},
		},
	}, string(entity.SyncEventInit)))
	require.False(t, eng.registry.NeedsRefresh(now.Add(time.Second)))
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "start uhost-a", noopStep)

	assert.NoError(t, err)
	assert.Equal(t, "started", reply)
	assert.True(t, eng.registry.NeedsRefresh(now.Add(time.Second)))
}

func TestRegistryTraceStateAccessorReturnsImmutableTraceState(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.registry = entity.NewRegistry(entity.WithClock(func() time.Time { return now }))
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"RetCode":    0,
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-trace", "Name": "trace-host", "State": "Running"},
		},
	}, string(entity.SyncEventInit)))

	state := eng.RegistryTraceState(now.Add(12 * time.Second))

	assert.NotEmpty(t, state.SnapshotID)
	assert.Equal(t, int64(12), state.AgeSeconds)
	assert.Equal(t, string(entity.SyncEventInit), state.SyncEvent)
}

func TestRegistrySnapshotAccessorReturnsImmutableSnapshot(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.registry = entity.NewRegistry(entity.WithClock(func() time.Time { return now }))
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"RetCode":    0,
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-trace", "Name": "trace-host", "State": "Running"},
		},
	}, string(entity.SyncEventInit)))

	snap := eng.RegistrySnapshot()
	require.NotEmpty(t, snap.SnapshotID)
	snap.Instances["uhost-trace"] = entity.InstanceSnapshot{UHostId: "uhost-trace", Name: "mutated"}

	fresh := eng.RegistrySnapshot()
	assert.Equal(t, "trace-host", fresh.Instances["uhost-trace"].Name)
}

func TestPlannerPriorTextSnapshotOmitsSystemAndToolMessages(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
		{Role: openai.ChatMessageRoleUser, Content: "看一下 A 机器"},
		{Role: openai.ChatMessageRoleAssistant, Content: "A 机器正在运行"},
		{Role: openai.ChatMessageRoleTool, Content: `{"UHostId":"uhost-a","PrivateIP":"10.0.0.1"}`},
	}

	prior := eng.PlannerPriorTextSnapshot()

	assert.Contains(t, prior, "user: 看一下 A 机器")
	assert.Contains(t, prior, "assistant: A 机器正在运行")
	assert.NotContains(t, prior, "system prompt")
	assert.NotContains(t, prior, "uhost-a")
	assert.NotContains(t, prior, "10.0.0.1")
}

func TestPlannerPriorTextSnapshotKeepsNewestMessagesWithinRuneBudget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
	}
	for i := 1; i <= 12; i++ {
		eng.messages = append(eng.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: fmt.Sprintf("msg-%02d %s", i, strings.Repeat("中", 450)),
		})
	}

	prior := eng.PlannerPriorTextSnapshot()

	assert.LessOrEqual(t, len([]rune(prior)), maxPlannerPriorTextRunes)
	assert.Contains(t, prior, "msg-12")
	assert.Contains(t, prior, "msg-09")
	assert.NotContains(t, prior, "msg-08")
	assert.NotContains(t, prior, "msg-01")
}

func TestEnsureProjectId_UsesConfigWhenSet(t *testing.T) {
	// Pre-configured ProjectId → GetProjectList must NOT be called.
	eng, h, cleanup := newEngineWithServer(t, &mockLLM{}, "org-cfg-value", "")
	defer cleanup()

	_, err := eng.Init(context.Background())
	assert.NoError(t, err)

	// Verify the underlying executor still carries the config value.
	ext := eng.externalExecutor()
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

	ext := eng.externalExecutor()
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

	ext := eng.externalExecutor()
	assert.Equal(t, "org-first", ext.ProjectId())
}

func TestEnsureProjectId_SilentOnMalformed(t *testing.T) {
	// Empty ProjectSet → no panic, ProjectId stays empty.
	body := `{"RetCode": 0, "ProjectSet": []}`
	eng, _, cleanup := newEngineWithServer(t, &mockLLM{}, "", body)
	defer cleanup()

	_, err := eng.Init(context.Background())
	assert.NoError(t, err, "Init must not fail when GetProjectList returns empty set")

	ext := eng.externalExecutor()
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

type hardBlockMatrixCase struct {
	name        string
	msg         string
	wantBlocked bool
}

func runHardBlockMatrixCase(t *testing.T, tc hardBlockMatrixCase) {
	t.Helper()

	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "fall through"}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), tc.msg, noopStep)
	assert.NoError(t, err)

	if tc.wantBlocked {
		assert.Empty(t, mock.calls, "hard-block must not call LLM")
		assert.Empty(t, executor.calls, "hard-block must not call tools")
		assert.Contains(t, reply, "财务中心")
		return
	}

	assert.Len(t, mock.calls, 1, "non-blocked cases should enter exactly one LLM round")
}

func TestAccountBillingHardBlock_Matrix(t *testing.T) {
	cases := []hardBlockMatrixCase{
		// Branch 1: account-only data always hard-blocks. Instance words
		// must NOT veto balance / total-bill / transaction-flow queries.
		{name: "branch1_refuses_monthly_total_bill", msg: "查本月总账单", wantBlocked: true},
		{name: "branch1_refuses_account_balance", msg: "账号余额还剩多少", wantBlocked: true},
		{name: "branch1_refuses_transaction_flow", msg: "给我消费流水", wantBlocked: true},
		{name: "branch1_refuses_english_balance", msg: "balance 多少", wantBlocked: true},
		{name: "branch1_balance_not_vetoed_by_instance_words", msg: "这些机器导致账号余额还剩多少", wantBlocked: true},
		{name: "branch1_transaction_flow_not_vetoed_by_instance_words", msg: "每台机器的消费流水", wantBlocked: true},

		// Branch 2: monthly account summaries hard-block only when no
		// instance-scope words are present.
		{name: "branch2_refuses_monthly_account_total", msg: "本月总共扣了多少钱", wantBlocked: true},
		{name: "branch2_refuses_monthly_spend", msg: "当月花费多少", wantBlocked: true},
		{name: "branch2_refuses_monthly_bill", msg: "月度账单", wantBlocked: true},
		{name: "branch2_vetoed_by_instance_scope_allows_llm", msg: "本月哪台实例消费最高"},
		{name: "branch2_vetoed_by_specific_instance_word_allows_llm", msg: "当月这台机器扣了多少"},

		// Instance-level billing must pass through the hard-block. It stays
		// on the normal LLM/tool loop because forced object tool_choice is not
		// supported reliably by the ds v4 flash baseline for DiagnoseBilling.
		{name: "instance_top_spender_allows_llm", msg: "我账号下哪台实例消费最高"},
		{name: "instance_cost_breakdown_allows_llm", msg: "当前这些实例费用明细"},
		{name: "stopped_instance_still_charging_allows_llm", msg: "那台机器为什么关机后还在扣费"},
		{name: "named_instance_billing_allows_llm", msg: "uhost-abc123 这台为什么扣费这么多"},
		{name: "instance_cost_ratio_allows_llm", msg: "实例费用占比"},

		// Non-billing turns must not hard-block.
		{name: "monitor_query_passes_through", msg: "看监控"},
		{name: "off_topic_passes_through", msg: "今天天气"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runHardBlockMatrixCase(t, tc)
		})
	}
}

func TestAccountBillingHardBlock_FinanceRealtimeVsRules(t *testing.T) {
	cases := []hardBlockMatrixCase{
		{name: "refuses_invoice_status", msg: "\u6211\u7684\u53d1\u7968\u72b6\u6001\u600e\u4e48\u6837", wantBlocked: true},
		{name: "refuses_invoice_application_approved", msg: "\u5f00\u7968\u7533\u8bf7\u901a\u8fc7\u4e86\u5417", wantBlocked: true},
		{name: "refuses_invoice_review_approved", msg: "\u53d1\u7968\u5ba1\u6838\u901a\u8fc7\u4e86\u5417", wantBlocked: true},
		{name: "refuses_refund_progress", msg: "\u9000\u6b3e\u8fdb\u5ea6\u5230\u54ea\u4e86", wantBlocked: true},
		{name: "refuses_refund_success", msg: "\u9000\u6b3e\u6210\u529f\u4e86\u5417", wantBlocked: true},
		{name: "refuses_refunded_yet", msg: "\u9000\u6b3e\u4e86\u5417", wantBlocked: true},
		{name: "refuses_arrears_amount", msg: "\u6b20\u8d39\u91d1\u989d\u591a\u5c11", wantBlocked: true},
		{name: "refuses_pending_payable_bill", msg: "\u5f85\u652f\u4ed8\u8d26\u5355\u7ed9\u6211\u770b\u4e00\u4e0b", wantBlocked: true},
		{name: "refuses_invoice_delivery", msg: "\u53d1\u7968\u5bc4\u9001\u5230\u54ea\u4e86", wantBlocked: true},
		{name: "refuses_charge_record", msg: "\u6263\u8d39\u8bb0\u5f55\u67e5\u4e00\u4e0b", wantBlocked: true},
		{name: "refuses_transaction_record", msg: "\u4ea4\u6613\u8bb0\u5f55\u67e5\u4e00\u4e0b", wantBlocked: true},
		{name: "refuses_my_package_expiry_time", msg: "\u6211\u7684\u5957\u9910\u4ec0\u4e48\u65f6\u5019\u5230\u671f", wantBlocked: true},
		{name: "refuses_my_recharge_amount", msg: "\u6211\u5145\u503c\u4e86\u591a\u5c11\u94b1", wantBlocked: true},
		{name: "mixed_invoice_rule_and_status_refuses", msg: "\u600e\u4e48\u5f00\u53d1\u7968\uff0c\u6211\u7684\u53d1\u7968\u72b6\u6001\u600e\u4e48\u6837", wantBlocked: true},
		{name: "mixed_refund_rule_and_progress_refuses", msg: "\u9000\u6b3e\u89c4\u5219\u662f\u4ec0\u4e48\uff0c\u6211\u7684\u9000\u6b3e\u8fdb\u5ea6\u5230\u54ea\u4e86", wantBlocked: true},
		{name: "instance_transaction_flow_still_refuses", msg: "\u6bcf\u53f0\u5b9e\u4f8b\u7684\u6d88\u8d39\u6d41\u6c34", wantBlocked: true},
		{name: "invoice_howto_allows_knowledge", msg: "\u600e\u4e48\u5f00\u53d1\u7968"},
		{name: "invoice_application_flow_allows_knowledge", msg: "\u53d1\u7968\u7533\u8bf7\u6d41\u7a0b"},
		{name: "refund_rule_allows_knowledge", msg: "\u9000\u6b3e\u89c4\u5219\u662f\u4ec0\u4e48"},
		{name: "arrears_howto_allows_knowledge", msg: "\u6b20\u8d39\u600e\u4e48\u529e"},
		{name: "billing_mode_difference_allows_knowledge", msg: "\u6309\u91cf\u548c\u5305\u65e5\u6709\u4ec0\u4e48\u533a\u522b"},
		{name: "package_expiry_rule_allows_knowledge", msg: "\u5957\u9910\u5230\u671f\u600e\u4e48\u529e"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runHardBlockMatrixCase(t, tc)
		})
	}
}

func TestAccountBillingHardBlock_DoesNotResetTurnScopedMonitorState(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.currentMonitorWindow = true
	eng.currentMonitorTargets = []string{"uhost-monitor-001"}
	eng.currentMonitorNoData = []string{"uhost-monitor-001"}
	eng.currentMonitorStart = 100
	eng.currentMonitorEnd = 200

	reply, err := eng.Chat(context.Background(), "查一下我这个账号本月总账单、余额和消费流水明细", noopStep)
	assert.NoError(t, err)
	assert.Contains(t, reply, "财务中心")
	assert.Empty(t, mock.calls)
	assert.Empty(t, executor.calls)
	assert.True(t, eng.currentMonitorWindow)
	assert.Equal(t, []string{"uhost-monitor-001"}, eng.currentMonitorTargets)
	assert.Equal(t, []string{"uhost-monitor-001"}, eng.currentMonitorNoData)
	assert.Equal(t, int64(100), eng.currentMonitorStart)
	assert.Equal(t, int64(200), eng.currentMonitorEnd)
}

func TestAccountBillingHardBlock_NotifiesObserverWithoutStepEvent(t *testing.T) {
	executor := billingScenarioExecutor("Running")
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Stopped on-demand instances still charge for disks. [1]"}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "账号余额还剩多少", onStep)

	require.NoError(t, err)
	assert.Equal(t, accountBillingUnsupportedReply, reply)
	assert.Empty(t, mock.calls)
	assert.Empty(t, *events, "hard-block trace signal must not surface as a CLI step")
	require.Len(t, hardBlocks, 1)
	assert.True(t, hardBlocks[0].Hit)
	assert.Equal(t, "account_billing_unsupported", hardBlocks[0].Category)
}

// toolChoiceForMonitor returns true iff req.ToolChoice names GetCompShareInstanceMonitor.
func toolChoiceForMonitor(req llm.ChatRequest) bool {
	tc, ok := req.ToolChoice.(openai.ToolChoice)
	if !ok {
		return false
	}
	return tc.Type == openai.ToolTypeFunction && tc.Function.Name == "GetCompShareInstanceMonitor"
}

func monitorScenarioExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{
					"UHostId": "uhost-monitor-001",
					"Metrics": []any{
						map[string]any{"Name": "CPUUsageRate", "Value": 12},
					},
				},
			}},
		},
	}}
}

func TestMonitorRecallGuard_ForcesMonitorOnAdjacentFollowUp(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "监控已查询"},
		{Content: "fresh monitor"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看看这台机器的监控", noopStep)
	assert.NoError(t, err)
	assert.Equal(t, 1, eng.lastMonitorTurn)

	_, err = eng.Chat(context.Background(), "只看刚才那台机器的 GPU 和显存监控", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.True(t, toolChoiceForMonitor(mock.calls[2]),
			"adjacent monitor follow-up should force GetCompShareInstanceMonitor")
	}
}

func TestMonitorRecallGuard_NonAdjacentTurnDoesNotTrigger(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "监控已查询"},
		{Content: "中间一轮普通回答"},
		{Content: "should not force"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看看这台机器的监控", noopStep)
	assert.NoError(t, err)
	_, err = eng.Chat(context.Background(), "4090 显存多大", noopStep)
	assert.NoError(t, err)
	_, err = eng.Chat(context.Background(), "只看刚才那台机器的 GPU 和显存监控", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 4) {
		assert.Nil(t, mock.calls[3].ToolChoice,
			"non-adjacent monitor follow-up must not force monitor")
	}
}

func TestMonitorRecallGuard_NoFollowUpKeywordDoesNotTrigger(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "监控已查询"},
		{Content: "not forced"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "看看这台机器的监控", noopStep)
	assert.NoError(t, err)
	_, err = eng.Chat(context.Background(), "今天天气如何", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"adjacent turn without monitor follow-up keywords must not force monitor")
	}
}

func TestMonitorRecallGuard_FirstTurnDoesNotTrigger(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "not forced"}}}
	eng := NewWithDeps(mock, monitorScenarioExecutor(), nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "帮我判断 CPU 和 GPU 占用异常吗", noopStep)
	assert.NoError(t, err)

	if assert.Len(t, mock.calls, 1) {
		assert.Nil(t, mock.calls[0].ToolChoice,
			"first-turn monitor wording must not force monitor recall without prior monitor call")
	}
}

// When the active LLM does not support object tool_choice (e.g. ds v4 flash
// in thinking mode), the monitor recall guard must fall through to LLM auto
// routing instead of emitting a forced ToolChoice that would 400.
func TestMonitorRecallGuard_FallsThroughWhenObjectToolChoiceUnsupported(t *testing.T) {
	executor := monitorScenarioExecutor()
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "监控已查询"},
		{Content: "auto routed"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.setSupportsObjectToolChoice(false)

	_, err := eng.Chat(context.Background(), "看看这台机器的监控", noopStep)
	assert.NoError(t, err)

	_, err = eng.Chat(context.Background(), "只看刚才那台机器的 GPU 和显存监控", noopStep)
	assert.NoError(t, err)

	if assert.GreaterOrEqual(t, len(mock.calls), 3) {
		assert.Nil(t, mock.calls[2].ToolChoice,
			"capability-gated guard must not force ToolChoice when object tool_choice is unsupported")
	}
}

func phase1KnownInstanceDescribeResult() map[string]any {
	return map[string]any{
		"TotalCount": 1,
		"UHostSet": []any{
			map[string]any{
				"UHostId":   "uhost-phase1-001",
				"Name":      "phase1-demo",
				"State":     "Running",
				"GPU":       float64(1),
				"GpuType":   "4090",
				"CPU":       float64(8),
				"Memory":    float64(32),
				"ImageType": "Ubuntu",
			},
		},
	}
}

func phase1MultipleInstanceDescribeResult() map[string]any {
	return map[string]any{
		"TotalCount": 2,
		"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-select-002",
				"Name":       "select-b",
				"State":      "Running",
				"GPU":        float64(1),
				"GpuType":    "4090",
				"CPU":        float64(16),
				"Memory":     float64(65536),
				"Zone":       "cn-wlcb-01",
				"ChargeType": "Postpay",
			},
			map[string]any{
				"UHostId":    "uhost-select-001",
				"Name":       "select-a",
				"State":      "Running",
				"GPU":        float64(1),
				"GpuType":    "4090",
				"CPU":        float64(8),
				"Memory":     float64(32768),
				"Zone":       "cn-wlcb-01",
				"ChargeType": "Postpay",
			},
		},
	}
}

func phase1ManyInstanceDescribeResult(count int) map[string]any {
	hosts := make([]any, 0, count)
	for i := 1; i <= count; i++ {
		hosts = append(hosts, map[string]any{
			"UHostId":    fmt.Sprintf("uhost-select-%03d", i),
			"Name":       fmt.Sprintf("select-%03d", i),
			"State":      "Running",
			"GPU":        float64(1),
			"GpuType":    "4090",
			"CPU":        float64(8),
			"Memory":     float64(32768),
			"Zone":       "cn-wlcb-01",
			"ChargeType": "Postpay",
		})
	}
	return map[string]any{
		"TotalCount": count,
		"UHostSet":   hosts,
	}
}

func phase1AmbiguousInstanceDescribeResult() map[string]any {
	return map[string]any{
		"TotalCount": 2,
		"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-dup-002",
				"Name":       "dup",
				"State":      "Running",
				"GPU":        float64(1),
				"GpuType":    "4090",
				"CPU":        float64(16),
				"Memory":     float64(65536),
				"Zone":       "cn-wlcb-01",
				"ChargeType": "Postpay",
			},
			map[string]any{
				"UHostId":    "uhost-dup-001",
				"Name":       "dup",
				"State":      "Running",
				"GPU":        float64(1),
				"GpuType":    "4090",
				"CPU":        float64(8),
				"Memory":     float64(32768),
				"Zone":       "cn-wlcb-01",
				"ChargeType": "Postpay",
			},
		},
	}
}

func phase1ResourcePlan() intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentResourceInfo,
		Slots: intent.Slots{TargetRefs: []intent.TargetRef{{
			Type:       intent.TargetRefName,
			Value:      "phase1-demo",
			Source:     intent.SourceUserText,
			SourceSpan: "phase1-demo",
		}}},
		Retrieval:  intent.Retrieval{Enabled: false},
		Confidence: 0.9,
	}
}

func phase1ResourcePlanWithoutTarget() intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentResourceInfo,
		Slots:         intent.Slots{},
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    0.9,
	}
}

func assertEngineComputedFact(t *testing.T, env envelope.Envelope, key string, want any) {
	t.Helper()
	for _, fact := range env.Computed {
		if fact.Key == key {
			assert.Equal(t, want, fact.Value)
			assert.Equal(t, envelope.FactSourceComputed, fact.Source)
			return
		}
	}
	t.Fatalf("missing computed fact key=%s in %#v", key, env.Computed)
}

func phase1ResourceFilterPlan() intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentResourceInfo,
		Slots: intent.Slots{TargetRefs: []intent.TargetRef{
			{Type: intent.TargetRefFilter, Value: "state=running"},
			{Type: intent.TargetRefFilter, Value: "gpu_type=4090"},
		}},
		Retrieval:  intent.Retrieval{Enabled: false},
		Confidence: 0.9,
	}
}

func phase1MonitorPlan() intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentMonitorQuery,
		Slots: intent.Slots{
			TargetRefs: []intent.TargetRef{{
				Type:       intent.TargetRefName,
				Value:      "phase1-demo",
				Source:     intent.SourceUserText,
				SourceSpan: "phase1-demo",
			}},
		},
		Retrieval:  intent.Retrieval{Enabled: false},
		Confidence: 0.9,
	}
}

func phase1MonitorHistoryPlan() intent.Plan {
	plan := phase1MonitorPlan()
	plan.Intent = intent.IntentMonitorHistory
	plan.Slots.TimeWindow = &intent.TimeWindow{Type: intent.TimeWindowRelative, Value: "yesterday"}
	return plan
}

func phase1MonitorTodayPlan() intent.Plan {
	plan := phase1MonitorPlan()
	plan.Slots.TimeWindow = &intent.TimeWindow{Type: intent.TimeWindowPreset, Value: "today"}
	return plan
}

func phase1MonitorPlanWithoutTarget() intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentMonitorQuery,
		Slots: intent.Slots{
			Metrics: []intent.Metric{intent.MetricCPU},
		},
		Retrieval:  intent.Retrieval{Enabled: false},
		Confidence: 0.9,
	}
}

func phase1MonitorPlanForName(name string) intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentMonitorQuery,
		Slots: intent.Slots{
			TargetRefs: []intent.TargetRef{{
				Type:       intent.TargetRefName,
				Value:      name,
				Source:     intent.SourceUserText,
				SourceSpan: name,
			}},
			Metrics: []intent.Metric{intent.MetricCPU},
		},
		Retrieval:  intent.Retrieval{Enabled: false},
		Confidence: 0.9,
	}
}

func TestPlanWithUserTextMonitorMetricsCorrectsChineseVRAM(t *testing.T) {
	plan := phase1MonitorPlan()
	plan.Slots.Metrics = []intent.Metric{intent.MetricCPU, intent.MetricGPU}

	got := planWithUserTextMonitorMetrics(plan, "uhost-abc 当前 CPU 和显存使用率是多少")

	assert.Equal(t, []intent.Metric{intent.MetricCPU, intent.MetricVRAM}, got.Slots.Metrics)
}

func knowledgeQAPlan(retrievalEnabled bool) intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentKnowledgeQA,
		Slots:         intent.Slots{},
		Retrieval:     intent.Retrieval{Enabled: retrievalEnabled},
		Confidence:    0.9,
	}
}

func diagnosisPlanForUHost(uhostID string) intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentDiagnosis,
		Slots: intent.Slots{
			TargetRefs: []intent.TargetRef{{
				Type:       intent.TargetRefUHostIDUserInput,
				Value:      uhostID,
				Source:     intent.SourceUserText,
				SourceSpan: uhostID,
			}},
		},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    0.9,
	}
}

func unknownEngineTestPlan() intent.Plan {
	return intent.Plan{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentUnknown,
		Retrieval:     intent.Retrieval{Enabled: false},
		Confidence:    0,
	}
}

func TestStage2BRetrievalHitUsesLLMWithoutTools(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "faq-billing-001",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Billing after stop",
		Content:     "Stopped on-demand instances still charge for disks.",
		SourceURL:   "https://example.test/billing",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 80, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Stopped on-demand instances still charge for disks. [1]"}}}
	executor := &mockExecutor{}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	var retrievalTraces []observability.RetrievalTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		plannerTraces = append(plannerTraces, trace)
	})
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "Stopped on-demand instances still charge for disks.")
	require.Len(t, mock.calls, 1)
	assert.Empty(t, mock.calls[0].Tools, "knowledge retrieval must not expose CompShare API tools")
	assert.Empty(t, executor.calls, "knowledge retrieval must not call CompShare API tools")
	require.Len(t, planner.calls, 1)
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "why do stopped instances still bill", retriever.calls[0].question)
	assert.Equal(t, "billing", retriever.calls[0].productArea, "engine must infer product area without relying on planner Scope")
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.CutoverStatusDispatchedRetrieval), plannerTraces[0].CutoverStatus)
	require.Len(t, retrievalTraces, 1)
	assert.True(t, retrievalTraces[0].Enabled)
	assert.Equal(t, "kb.v1", retrievalTraces[0].KBVersion)
	assert.Equal(t, 1, retrievalTraces[0].Hits)
	require.Len(t, retrievalTraces[0].HitItems, 1)
	assert.Equal(t, "faq-billing-001", retrievalTraces[0].HitItems[0].ChunkID)
}

func TestPlannerRoutingControlsStage2BRAGPath(t *testing.T) {
	knowledgeChunk := knowledge.KBChunk{
		ChunkID:     "w0-windows-rdp-audio-a1b2c3d4",
		KBVersion:   "stage2b.w0",
		SourceType:  "runbook",
		ProductArea: "windows",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Windows RDP audio",
		Content:     "Configure remote desktop audio redirection and Windows Audio before reconnecting.",
		SourceURL:   "https://www.compshare.cn/docs/windows-rdp-audio",
	}
	knowledgeResult := knowledge.RetrievalResult{
		Enabled:   true,
		KBVersion: "stage2b.w0",
		Hits:      []knowledge.KBChunk{knowledgeChunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: knowledgeChunk, Score: 80, Kept: true}},
	}

	cases := []struct {
		name          string
		userMsg       string
		plan          intent.Plan
		expectRAGPath bool
		expectTool    string
	}{
		{
			name:          "remote desktop audio how-to routes to RAG",
			userMsg:       "\u8fdc\u7a0b\u684c\u9762\u6ca1\u58f0\u97f3\u8be5\u600e\u4e48\u5904\u7406",
			plan:          knowledgeQAPlan(false),
			expectRAGPath: true,
		},
		{
			name:          "error code explanation routes to RAG",
			userMsg:       "\u9519\u8bef\u7801 226601 \u662f\u4ec0\u4e48\u610f\u601d",
			plan:          knowledgeQAPlan(false),
			expectRAGPath: true,
		},
		{
			name:          "BaseURL config routes to RAG",
			userMsg:       "Coding Plan BaseURL \u600e\u4e48\u586b",
			plan:          knowledgeQAPlan(false),
			expectRAGPath: true,
		},
		{
			name:          "resource inventory does not route to RAG",
			userMsg:       "\u6211\u73b0\u5728\u6709\u591a\u5c11\u673a\u5668",
			plan:          phase1ResourcePlan(),
			expectRAGPath: false,
		},
		{
			name:          "own account gpu instances stay resource path",
			userMsg:       "\u6211\u8d26\u53f7\u4e0b\u6709\u54ea\u4e9b 4090 \u5b9e\u4f8b",
			plan:          phase1ResourcePlan(),
			expectRAGPath: false,
		},
		{
			name:          "platform stock availability does not route to RAG",
			userMsg:       "\u4e0a\u6d77\u673a\u623f\u8fd8\u5269\u6ca1\u5269 H100 \u5e93\u5b58",
			plan:          unknownEngineTestPlan(),
			expectRAGPath: false,
			expectTool:    "DescribeAvailableCompShareInstanceTypes",
		},
		{
			name:          "specific instance diagnosis does not route to RAG",
			userMsg:       "uhost-abc123 \u542f\u52a8\u5931\u8d25",
			plan:          diagnosisPlanForUHost("uhost-abc123"),
			expectRAGPath: false,
		},
		{
			name:          "monitor query does not route to RAG",
			userMsg:       "\u6211\u7684 GPU \u5229\u7528\u7387\u591a\u5c11",
			plan:          phase1MonitorPlanWithoutTarget(),
			expectRAGPath: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: tc.plan}}}
			retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{knowledgeResult}}
			// PR-RAG-PLANNER-INTENT-AUDIT (2026-05-17): non-RAG fallback path
			// is now guarded by the cited-contract invariant — plain text would
			// be coerced to ragNoEvidenceReply. This test focuses on routing
			// behaviour (does the planner intent reach RAG vs ReAct?), so we
			// pre-add a [1] citation to the non-RAG mock reply to bypass the
			// invariant. The invariant itself is covered by the
			// TestRAGCitedContractInvariant* tests below.
			mockReply := "react path [1]"
			if tc.expectRAGPath {
				mockReply = "RAG answer. [1]"
			}
			mock := &mockLLM{responses: []llm.ChatResponse{{Content: mockReply}}}
			eng := NewWithDeps(mock, &mockExecutor{}, nil)
			eng.InitWithContext("test user")
			eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
			eng.SetKnowledgeRetriever(retriever)

			reply, err := eng.Chat(context.Background(), tc.userMsg, noopStep)

			require.NoError(t, err)
			require.Len(t, planner.calls, 1)
			if tc.expectRAGPath {
				assert.True(t, hasNumberedCitation(reply), "RAG path must return cited answer, got %q", reply)
				require.Len(t, retriever.calls, 1)
				assert.Equal(t, tc.userMsg, retriever.calls[0].question)
				require.Len(t, mock.calls, 1)
				assert.Empty(t, mock.calls[0].Tools, "knowledge retrieval must not expose API tools")
				return
			}
			assert.Equal(t, "react path [1]", reply, "non-RAG planner output should fall back to the normal LLM path in this test")
			assert.Empty(t, retriever.calls, "non-knowledge planner output must not call knowledge retriever")
			if tc.expectTool != "" {
				require.NotEmpty(t, mock.calls, "non-RAG fallback should expose tools")
				assert.Contains(t, toolNames(mock.calls[0].Tools), tc.expectTool)
			}
		})
	}
}

func TestStage2BRetrievalHitCallsLLMWithNumberedEvidence(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "w0-billing_rule-stopped-a1b2c3d4",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing_rule",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Stopped instance billing",
		Content:     "Stopped on-demand instances still charge for disks.",
		SourceURL:   "https://www.compshare.cn/docs/billing",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	latency := int64(4987)
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:              true,
		KBVersion:            "kb.v1",
		QueryNormalized:      "stopped instances bill",
		Hits:                 []knowledge.KBChunk{chunk},
		HitItems:             []knowledge.RetrievalHit{{Chunk: chunk, Score: 80, Kept: true}},
		HybridMode:           "bm25_fallback",
		HybridFallbackReason: "embedding_timeout",
		EmbeddingLatencyMS:   &latency,
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Stopped instances still charge for disks. [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "Stopped instances still charge for disks. [1]", reply)
	require.Len(t, mock.calls, 1)
	requestText := requestContent(mock.calls[0])
	assert.Contains(t, requestText, "[1] Stopped instance billing")
	assert.Contains(t, requestText, "Stopped on-demand instances still charge for disks.")
	assert.NotContains(t, requestText, "w0-billing_rule-stopped-a1b2c3d4", "chunk IDs must stay out of LLM context")
	require.Len(t, retrievalTraces, 1)
	assert.Equal(t, "why do stopped instances still bill", retrievalTraces[0].QueryRaw)
	assert.Equal(t, "stopped instances bill", retrievalTraces[0].QueryNormalized)
	assert.Equal(t, 1, retrievalTraces[0].Hits)
	require.Len(t, retrievalTraces[0].HitItems, 1)
	assert.Equal(t, "w0-billing_rule-stopped-a1b2c3d4", retrievalTraces[0].HitItems[0].ChunkID)
	assert.Equal(t, 80.0, retrievalTraces[0].HitItems[0].Score)
	assert.False(t, retrievalTraces[0].WeakEvidence)
	// HybridMode + HybridFallbackReason + EmbeddingLatencyMS must propagate
	// from RetrievalResult into the emitted trace so ops can aggregate
	// fallback rate AND latency distribution across runs.
	assert.Equal(t, "bm25_fallback", retrievalTraces[0].HybridMode)
	assert.Equal(t, "embedding_timeout", retrievalTraces[0].HybridFallbackReason)
	require.NotNil(t, retrievalTraces[0].EmbeddingLatencyMS)
	assert.Equal(t, int64(4987), *retrievalTraces[0].EmbeddingLatencyMS)
}

func TestStage2BRetrievalAmbiguousTopHitsMarksRankingErrorCandidate(t *testing.T) {
	chunkA := knowledge.KBChunk{
		ChunkID:     "w0-billing_rule-stopped-a1b2c3d4",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing_rule",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Stopped instance billing",
		Content:     "Stopped on-demand instances still charge for disks.",
	}
	chunkB := knowledge.KBChunk{
		ChunkID:     "w0-billing_rule-storage-e5f6a7b8",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing_rule",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Storage billing",
		Content:     "Disks keep billing while attached storage exists.",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:         true,
		KBVersion:       "kb.v1",
		QueryNormalized: "stopped instances bill",
		Hits:            []knowledge.KBChunk{chunkA, chunkB},
		HitItems: []knowledge.RetrievalHit{
			{Chunk: chunkA, Score: 80, Kept: true},
			{Chunk: chunkB, Score: 76, Kept: true},
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Stopped instances still charge for disks. [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "Stopped instances still charge for disks. [1]", reply)
	require.Len(t, retrievalTraces, 1)
	assert.False(t, retrievalTraces[0].WeakEvidence)
	assert.Empty(t, retrievalTraces[0].RefusedReason)
	assert.True(t, retrievalTraces[0].RankingErrorCandidate)
	require.Len(t, retrievalTraces[0].HitItems, 2)
}

func TestStage2BRetrievalNormalRefusalSetsRefusedReason(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "w0-billing_rule-stopped-a1b2c3d4",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing_rule",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Stopped instance billing",
		Content:     "Stopped on-demand instances still charge for disks.",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:         true,
		KBVersion:       "kb.v1",
		QueryNormalized: "stopped instances bill",
		Hits:            []knowledge.KBChunk{chunk},
		HitItems:        []knowledge.RetrievalHit{{Chunk: chunk, Score: 80, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: ragNoEvidenceReply}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)

	require.NoError(t, err)
	assert.Equal(t, ragNoEvidenceReply, reply)
	require.Len(t, retrievalTraces, 1)
	assert.False(t, retrievalTraces[0].WeakEvidence)
	assert.Equal(t, "refusal", retrievalTraces[0].RefusedReason)
	assert.False(t, retrievalTraces[0].RankingErrorCandidate)
}

func TestStage2BRetrievalMissReturnsNewNoEvidenceReplyAndTrace(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:         true,
		KBVersion:       "kb.v1",
		QueryNormalized: "imaginary feature",
		Empty:           true,
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "does the platform support imaginary feature", noopStep)

	require.NoError(t, err)
	assert.Equal(t, ragNoEvidenceReply, reply)
	assert.Empty(t, mock.calls)
	require.Len(t, retrievalTraces, 1)
	assert.Equal(t, "no_evidence", retrievalTraces[0].RefusedReason)
	assert.True(t, retrievalTraces[0].RankingErrorCandidate)
}

func TestStage2BRetrievalWeakEvidenceMarksTraceAndAddsPromptHint(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "w0-modelverse-package-a1b2c3d4",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "modelverse",
		ACL:         "customer_safe",
		Confidence:  "medium",
		Title:       "ModelVerse package",
		Content:     "Coding Plan has a quota window.",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:         true,
		KBVersion:       "kb.v1",
		QueryNormalized: "coding quota",
		Hits:            []knowledge.KBChunk{chunk},
		HitItems:        []knowledge.RetrievalHit{{Chunk: chunk, Score: 54.9, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: ragNoEvidenceReply}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "coding quota details", noopStep)

	require.NoError(t, err)
	assert.Equal(t, ragNoEvidenceReply, reply)
	require.Len(t, mock.calls, 1)
	assert.Contains(t, requestContent(mock.calls[0]), "资料相关性较低")
	require.Len(t, retrievalTraces, 1)
	assert.True(t, retrievalTraces[0].WeakEvidence)
	assert.Equal(t, "weak_evidence", retrievalTraces[0].RefusedReason)
	assert.True(t, retrievalTraces[0].RankingErrorCandidate)
}

func TestStage2BRetrievalWeakEvidenceCitedAnswerHasNoRefusedReason(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "w0-modelverse-package-a1b2c3d4",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "modelverse",
		ACL:         "customer_safe",
		Confidence:  "medium",
		Title:       "ModelVerse package",
		Content:     "Coding Plan has a quota window.",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:         true,
		KBVersion:       "kb.v1",
		QueryNormalized: "coding quota",
		Hits:            []knowledge.KBChunk{chunk},
		HitItems:        []knowledge.RetrievalHit{{Chunk: chunk, Score: 54.9, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Coding Plan has a quota window. [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "coding quota details", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "Coding Plan has a quota window. [1]", reply)
	require.Len(t, retrievalTraces, 1)
	assert.True(t, retrievalTraces[0].WeakEvidence)
	assert.Empty(t, retrievalTraces[0].RefusedReason)
	assert.True(t, retrievalTraces[0].RankingErrorCandidate)
}

func TestStage2BRetrievalRetryNoCitationFallsBackToNoEvidence(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "w0-billing_rule-4090-a1b2c3d4",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing_rule",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "4090 pricing",
		Content:     "4090 is billed hourly.",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:         true,
		KBVersion:       "kb.v1",
		QueryNormalized: "4090 hourly price",
		Hits:            []knowledge.KBChunk{chunk},
		HitItems:        []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "4090 is billed hourly."},
		{Content: "4090 hourly billing applies."},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var retrievalTraces []observability.RetrievalTrace
	var outcomeTraces []observability.OutcomeTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetOutcomeTraceObserver(func(trace observability.OutcomeTrace) {
		outcomeTraces = append(outcomeTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-pro"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "4090 hourly price", noopStep)

	require.NoError(t, err)
	assert.Equal(t, ragNoEvidenceReply, reply)
	require.Len(t, mock.calls, 2)
	assert.Contains(t, requestContent(mock.calls[1]), "必须带 [1]")
	require.Len(t, retrievalTraces, 1)
	assert.Equal(t, "retry_no_cite", retrievalTraces[0].RefusedReason)
	assert.True(t, retrievalTraces[0].RankingErrorCandidate)
	require.Len(t, outcomeTraces, 1)
	assert.Equal(t, 1, outcomeTraces[0].AttemptedHallucinatedCount)
	assert.Equal(t, 1, outcomeTraces[0].EscapedHallucinatedCount)
}

func requestContent(req llm.ChatRequest) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		b.WriteString(msg.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestStage2BRetrievalHitClipsStoredAssistantHistory(t *testing.T) {
	longContent := strings.Repeat("A", maxKnowledgeHistoryRunes+512)
	chunk := knowledge.KBChunk{
		ChunkID:     "faq-long-001",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Long billing answer",
		Content:     longContent,
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 80, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: longContent + " [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "explain billing in detail", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, longContent, "user-facing retrieval answer must remain complete")
	require.NotEmpty(t, eng.messages)
	stored := eng.messages[len(eng.messages)-1]
	assert.Equal(t, "assistant", stored.Role)
	assert.Less(t, len([]rune(stored.Content)), len([]rune(reply)), "stored history should be clipped")
	assert.Contains(t, stored.Content, knowledgeHistoryClipMarker)
	require.Len(t, mock.calls, 1)
	assert.Empty(t, mock.calls[0].Tools, "knowledge retrieval hit must still bypass API tools")
}

func TestStage2BRetrievalMissReturnsFixedReply(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Empty:     true,
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	var retrievalTraces []observability.RetrievalTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		plannerTraces = append(plannerTraces, trace)
	})
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		retrievalTraces = append(retrievalTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "does the platform support imaginary feature", noopStep)

	require.NoError(t, err)
	assert.Equal(t, ragNoEvidenceReply, reply)
	assert.Empty(t, mock.calls, "knowledge retrieval miss is handled by fixed reply, not ReAct")
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.CutoverStatusFallbackRetrievalMiss), plannerTraces[0].CutoverStatus)
	require.Len(t, retrievalTraces, 1)
	assert.True(t, retrievalTraces[0].Enabled)
	assert.Equal(t, "kb.v1", retrievalTraces[0].KBVersion)
	assert.Equal(t, 0, retrievalTraces[0].Hits)
	assert.Equal(t, "no_evidence", retrievalTraces[0].RefusedReason)
	assert.True(t, retrievalTraces[0].RankingErrorCandidate)
}

func TestStage2BRetrievalIgnoresPlannerRetrievalFlag(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:     "faq-image-001",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "image",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Images",
		Content:     "The platform provides platform, community, shared, and private images.",
	}
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(true)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 80, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "The platform provides community images. [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		plannerTraces = append(plannerTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "what image types are available", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "community")
	require.Len(t, mock.calls, 1)
	assert.Empty(t, mock.calls[0].Tools)
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "image", retriever.calls[0].productArea)
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.CutoverStatusDispatchedRetrieval), plannerTraces[0].CutoverStatus)
}

func TestStage2BRetrievalDisabledFallsBackToReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(true)}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react fallback"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		plannerTraces = append(plannerTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "what images are available", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "react fallback", reply)
	assert.Len(t, mock.calls, 1)
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.CutoverStatusFallbackRetrievalDisabled), plannerTraces[0].CutoverStatus)
}

func TestStage2BRetrievalCommonPredicateFallbacksDoNotCallRetriever(t *testing.T) {
	cases := []struct {
		name       string
		mutatePlan func(*intent.Plan)
		wantStatus intent.CutoverStatus
	}{
		{
			name: "hard block hint",
			mutatePlan: func(plan *intent.Plan) {
				plan.HardBlockHint = true
			},
			wantStatus: intent.CutoverStatusFallbackHardBlockHint,
		},
		{
			name: "low confidence",
			mutatePlan: func(plan *intent.Plan) {
				plan.Confidence = 0.3
			},
			wantStatus: intent.CutoverStatusFallbackLowConfidence,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := knowledgeQAPlan(false)
			tc.mutatePlan(&plan)
			planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: plan}}}
			retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
				Enabled:   true,
				KBVersion: "kb.v1",
				Empty:     true,
			}}}
			mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react fallback"}}}
			eng := NewWithDeps(mock, &mockExecutor{}, nil)
			eng.InitWithContext("test user")
			var plannerTraces []observability.PlannerTrace
			eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
				plannerTraces = append(plannerTraces, trace)
			})
			var hardBlocks []observability.EngineHardBlockTrace
			eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
				hardBlocks = append(hardBlocks, trace)
			})
			eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
			eng.SetKnowledgeRetriever(retriever)

			reply, err := eng.Chat(context.Background(), "billing FAQ", noopStep)

			require.NoError(t, err)
			// PR-RAG-PLANNER-INTENT-AUDIT (2026-05-17): with RAG enabled, the
			// fallback ReAct path's plain "react fallback" text would silently
			// break the cited 100% contract (no [n], not a refusal template).
			// The engine invariant coerces it to ragNoEvidenceReply and emits a
			// cited_contract_violation hard-block trace.
			assert.Equal(t, ragNoEvidenceReply, reply)
			assert.Empty(t, retriever.calls)
			require.Len(t, plannerTraces, 1)
			assert.Equal(t, string(tc.wantStatus), plannerTraces[0].CutoverStatus)
			require.Len(t, hardBlocks, 1)
			assert.Equal(t, "cited_contract_violation", hardBlocks[0].Category)
			assert.True(t, hardBlocks[0].Hit)
		})
	}
}

func TestRAGCitedContractInvariantSkipsWhenAnswerAlreadyCited(t *testing.T) {
	// PR-RAG-PLANNER-INTENT-AUDIT (2026-05-17): positive case — when the
	// fallback ReAct path happens to emit a properly cited answer (a
	// well-behaved LLM that adds [n] on its own), the invariant must NOT
	// fire and the reply must pass through unchanged.
	plan := knowledgeQAPlan(false)
	plan.HardBlockHint = true // force fallback to ReAct path
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: plan}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Empty:     true,
	}}}
	citedAnswer := "Cited answer body [1]."
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: citedAnswer}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "billing nav", noopStep)

	require.NoError(t, err)
	assert.Equal(t, citedAnswer, reply, "cited fallback answer should pass through unchanged")
	assert.Empty(t, hardBlocks, "invariant must not fire when [n] citation present")
}

func TestRAGCitedContractInvariantSkipsWhenAnswerIsKnowledgeRefusal(t *testing.T) {
	// PR-RAG-PLANNER-INTENT-AUDIT (2026-05-17): negative-cited case — when the
	// fallback ReAct path emits a refusal phrase (no [n] needed), the invariant
	// must recognise the refusal and let it pass through unchanged.
	plan := knowledgeQAPlan(false)
	plan.Confidence = 0.3 // force fallback via low confidence
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: plan}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Empty:     true,
	}}}
	refusalAnswer := "知识库未覆盖这个问题。"
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: refusalAnswer}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "billing nav", noopStep)

	require.NoError(t, err)
	assert.Equal(t, refusalAnswer, reply, "refusal answer should pass through unchanged")
	assert.Empty(t, hardBlocks, "invariant must not fire on recognised refusal template")
}

func TestRAGCitedContractInvariantSkipsWhenRetrieverDisabled(t *testing.T) {
	// PR-RAG-PLANNER-INTENT-AUDIT (2026-05-17): when knowledge retrieval is
	// not enabled at all (legacy/non-RAG deployment), the invariant must NOT
	// fire — a free-form chat answer is expected and the cited contract does
	// not apply.
	plan := knowledgeQAPlan(false)
	plan.HardBlockHint = true
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: plan}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react fallback without RAG"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	// NOTE: no retriever set — knowledgeRetriever is nil, invariant must be inert.

	reply, err := eng.Chat(context.Background(), "billing nav", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "react fallback without RAG", reply, "RAG-disabled deployments must not be affected")
	assert.Empty(t, hardBlocks)
}

func TestStage2BRetrievalHardBlockPrecedesPlanner(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Empty:     true,
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "\u8d26\u53f7\u4f59\u989d\u8fd8\u5269\u591a\u5c11", noopStep)

	require.NoError(t, err)
	assert.Equal(t, accountBillingUnsupportedReply, reply)
	assert.Empty(t, planner.calls, "permanent hard-block must run before Stage 2B planner")
	assert.Empty(t, retriever.calls)
	assert.Empty(t, mock.calls)
}

func TestStage2BFinanceFAQRetrievalUsesBillingArea(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits: []knowledge.KBChunk{{
			ChunkID:     "faq-billing-invoice-001",
			KBVersion:   "kb.v1",
			SourceType:  "faq",
			ProductArea: "billing",
			ACL:         "customer_safe",
			Confidence:  "high",
			Title:       "\u5982\u4f55\u5f00\u53d1\u7968",
			Content:     "\u53d1\u7968\u901a\u5e38\u5728\u63a7\u5236\u53f0\u8d22\u52a1\u4e2d\u5fc3\u7684\u53d1\u7968\u7ba1\u7406\u4e2d\u7533\u8bf7\u3002",
		}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: retriever.results[0].Hits[0].Content + " [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "\u600e\u4e48\u5f00\u53d1\u7968", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "发票管理")
	require.Len(t, mock.calls, 1)
	assert.Empty(t, mock.calls[0].Tools)
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "billing", retriever.calls[0].productArea)
}

func TestStage2BStoppedBillingFAQUsesKnowledgeRetrieval(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits: []knowledge.KBChunk{{
			ChunkID:     "faq-billing-stopped-instance-001",
			KBVersion:   "kb.v1",
			SourceType:  "faq",
			ProductArea: "billing",
			ACL:         "customer_safe",
			Confidence:  "high",
			Title:       "关机后为什么还会产生费用",
			Content:     "关机后是否继续计费取决于实例计费方式。",
		}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: retriever.results[0].Hits[0].Content + " [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "关机后为什么还扣费", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "关机后是否继续计费")
	require.Len(t, mock.calls, 1)
	assert.Empty(t, mock.calls[0].Tools)
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "billing", retriever.calls[0].productArea)
}

func TestStage2BFinanceRealtimeHardBlockPrecedesPlanner(t *testing.T) {
	cases := []string{
		"\u6211\u7684\u53d1\u7968\u72b6\u6001\u600e\u4e48\u6837",
		"\u9000\u6b3e\u8fdb\u5ea6\u5230\u54ea\u4e86",
		"\u6b20\u8d39\u91d1\u989d\u591a\u5c11",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
			retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
				Enabled:   true,
				KBVersion: "kb.v1",
				Empty:     true,
			}}}
			mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
			eng := NewWithDeps(mock, &mockExecutor{}, nil)
			eng.InitWithContext("test user")
			eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
			eng.SetKnowledgeRetriever(retriever)

			reply, err := eng.Chat(context.Background(), msg, noopStep)

			require.NoError(t, err)
			assert.Equal(t, accountBillingUnsupportedReply, reply)
			assert.Empty(t, planner.calls)
			assert.Empty(t, retriever.calls)
			assert.Empty(t, mock.calls)
		})
	}
}

func TestStage2BAndPhase1CutoverShareSinglePlannerCall(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Empty:     true,
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{{Allowed: true}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.rateLimiter = limiter
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-phase1-001")
	require.Len(t, planner.calls, 1)
	assert.Empty(t, retriever.calls, "resource cutover must not also run retrieval")
	require.Len(t, limiter.requests, 2)
	assert.Equal(t, "intent_planner", limiter.requests[0].Action)
	assert.Equal(t, governance.ClassReadExpensiveTool, limiter.requests[1].Class)
	assert.Equal(t, "DescribeCompShareInstance", limiter.requests[1].Action)
	// CLI passes plannerDispatchEnabled into useSeparateShadowRunner, so adding
	// USE_INTENT_PLANNER=shadow on top of Phase 1 + Stage 2B still leaves Engine
	// as the single planner-call owner for this turn.
}

func TestPhase1CutoverGateUnsetDoesNotCallPlanner(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react path"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		Model: "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "phase1-demo status", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "react path", reply)
	assert.Empty(t, planner.calls, "planner must not run when no demo intent is enabled")
	assert.Len(t, mock.calls, 1)
}

func TestPhase1CutoverHardBlockPrecedesPlanner(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "\u8d26\u53f7\u4f59\u989d\u8fd8\u5269\u591a\u5c11", noopStep)

	require.NoError(t, err)
	assert.Equal(t, accountBillingUnsupportedReply, reply)
	assert.Empty(t, planner.calls, "permanent hard-block must run before planner")
	assert.Empty(t, mock.calls)
}

func TestPhase1CutoverResourcePlanBypassesReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	var plannerTraces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		plannerTraces = append(plannerTraces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", onStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-phase1-001")
	assert.Empty(t, mock.calls, "handled resource plan must bypass ReAct")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	require.Len(t, planner.calls, 1)
	assert.NotNil(t, planner.calls[0].Resolver)
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.CutoverStatusDispatched), plannerTraces[0].CutoverStatus)
	require.Len(t, *events, 2)
	assert.Equal(t, observability.ToolSourcePlannerHandler, (*events)[0].Source)
	assert.Equal(t, observability.ToolSourcePlannerHandler, (*events)[1].Source)
}

func TestPhase1CutoverResourcePlanUsesGroundedRenderer(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text:            "renderer says phase1-demo is running",
		Model:           "deepseek-v4-flash",
		AttributionMode: grounded.AttributionEnvelope,
		EnvelopeHash:    "sha256:renderer-envelope",
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")
	var rendererTraces []observability.RendererTrace
	eng.SetRendererTraceObserver(func(trace observability.RendererTrace) {
		rendererTraces = append(rendererTraces, trace)
	})

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "renderer says phase1-demo is running", reply)
	assert.Empty(t, mock.calls, "grounded renderer must not re-enter ReAct")
	require.Len(t, groundedRenderer.requests, 1)
	assert.Contains(t, groundedRenderer.requests[0].Fallback, "uhost-phase1-001")
	assert.Equal(t, "resource_info", string(groundedRenderer.requests[0].Envelope.Kind))
	require.Len(t, rendererTraces, 1)
	assert.True(t, rendererTraces[0].Enabled)
	assert.Equal(t, "rendered", rendererTraces[0].Status)
	assert.Equal(t, "resource_info", rendererTraces[0].EnvelopeKind)
	require.Len(t, rendererTraces[0].InputEnvelopeHashes, 1)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, rendererTraces[0].InputEnvelopeHashes[0])
}

func TestPhase1CutoverResourceFilterPlanSendsFilteredEnvelopeToRenderer(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourceFilterPlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": 3,
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-running-4090", "Name": "run-4090", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64)},
				map[string]any{"UHostId": "uhost-running-v100", "Name": "run-v100", "State": "Running", "GpuType": "V100S", "GPU": float64(1), "CPU": float64(10), "Memory": float64(64)},
				map[string]any{"UHostId": "uhost-stopped-4090", "Name": "stop-4090", "State": "Stopped", "GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64)},
			},
		},
	}}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text:            "renderer says one running 4090 instance",
		Model:           "deepseek-v4-flash",
		AttributionMode: grounded.AttributionEnvelope,
		EnvelopeHash:    "sha256:renderer-envelope",
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")

	reply, err := eng.Chat(context.Background(), "show running 4090 instances", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "renderer says one running 4090 instance", reply)
	assert.Empty(t, mock.calls)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	require.Len(t, groundedRenderer.requests, 1)
	env := groundedRenderer.requests[0].Envelope
	require.Len(t, env.Subjects, 1)
	assert.Equal(t, "uhost-running-4090", env.Subjects[0].ID)
	assertEngineComputedFact(t, env, "filter_applied", "state=running,gpu_type=4090")
	assertEngineComputedFact(t, env, "matched_count", "1")
	assertEngineComputedFact(t, env, "total_count", "3")
}

func TestPhase1CutoverGroundedRendererFallbackUsesDeterministicReply(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text:            "fallback from deterministic handler",
		Model:           "deepseek-v4-flash",
		AttributionMode: grounded.AttributionEnvelope,
		EnvelopeHash:    "sha256:renderer-envelope",
		FallbackUsed:    true,
		FallbackReason:  grounded.FallbackValidationFailed,
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")
	var rendererTraces []observability.RendererTrace
	eng.SetRendererTraceObserver(func(trace observability.RendererTrace) {
		rendererTraces = append(rendererTraces, trace)
	})

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "fallback from deterministic handler", reply)
	assert.Empty(t, mock.calls, "renderer fallback must not fall through to ReAct")
	require.Len(t, rendererTraces, 1)
	assert.True(t, rendererTraces[0].FallbackUsed)
	assert.Equal(t, grounded.FallbackValidationFailed, rendererTraces[0].FallbackReason)
}

func TestPhase1CutoverGroundedRendererRateLimitDenialUsesDeterministicReply(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonQPSExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
	}}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text: "should not be used",
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")
	var rendererTraces []observability.RendererTrace
	eng.SetRendererTraceObserver(func(trace observability.RendererTrace) {
		rendererTraces = append(rendererTraces, trace)
	})

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-phase1-001")
	assert.Empty(t, groundedRenderer.requests, "renderer quota denial must skip renderer LLM call")
	require.Len(t, limiter.requests, 3)
	assert.Equal(t, "grounded_renderer", limiter.requests[2].Action)
	require.Len(t, rendererTraces, 1)
	assert.True(t, rendererTraces[0].FallbackUsed)
	assert.Equal(t, grounded.FallbackRateLimited, rendererTraces[0].FallbackReason)
}

func TestPhase1CutoverMonitorPlanBypassesReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{
					"UHostId": "uhost-phase1-001",
					"Metrics": []any{
						map[string]any{"Name": "CPUUsageRate", "Value": 12},
						map[string]any{"Name": "GPUUsageRate", "Value": 34},
					},
				},
			}},
		},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	onStep, events := collectSteps()

	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})
	reply, err := eng.Chat(context.Background(), "show phase1-demo cpu monitor", onStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "CPUUsageRate")
	assert.Empty(t, mock.calls, "handled monitor plan must bypass ReAct")
	assert.Equal(t, []string{"GetCompShareInstanceMonitor"}, executor.calls)
	require.Len(t, *events, 2)
	assert.Equal(t, observability.ToolSourcePlannerHandler, (*events)[0].Source)
	assert.Equal(t, observability.ToolSourcePlannerHandler, (*events)[1].Source)
	assert.NotEmpty(t, (*events)[1].RendererInputToolArgHashes)
}

func TestPhase1CutoverMonitorTodayWindowReturnsFixedReplyWithoutReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorTodayPlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	var traces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		traces = append(traces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show today's cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Equal(t, monitorHistoryUnsupportedReply, reply)
	assert.Empty(t, mock.calls, "non-current monitor window must not fall back to ReAct")
	assert.Empty(t, executor.calls, "non-current monitor window must not call monitor as current data")
	require.Len(t, traces, 1)
	assert.Equal(t, string(intent.CutoverStatusFallbackTimeWindow), traces[0].CutoverStatus)
}

func TestPlannerMonitorHistoryReturnsFixedReplyWithoutReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorHistoryPlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	var traces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		traces = append(traces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "historical cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Equal(t, monitorHistoryUnsupportedReply, reply)
	assert.Empty(t, mock.calls, "historical monitor planner output must not fall back to ReAct")
	assert.Empty(t, executor.calls)
	require.Len(t, traces, 1)
	assert.Equal(t, string(intent.CutoverStatusFallbackTimeWindow), traces[0].CutoverStatus)
}

func TestPhase1CutoverMonitorMissingTargetReturnsResourceSelection(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{}},
		},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	var traces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		traces = append(traces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "请选择一个")
	assert.Contains(t, reply, "uhost-select-001")
	assert.Contains(t, reply, "uhost-select-002")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	assert.Empty(t, mock.calls, "selection prompt must not fall back to ReAct")
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Len(t, eng.pendingResourceSelection.candidates, 2)
	require.Len(t, traces, 1)
	assert.Equal(t, string(intent.CutoverStatusSelectionRequired), traces[0].CutoverStatus)
}

func TestPhase1CutoverMonitorSelectionPromptStatesWhenCandidatesTruncated(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1ManyInstanceDescribeResult(21),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "前 20")
	assert.Contains(t, reply, "名称")
	assert.Contains(t, reply, "ID")
	assert.Contains(t, reply, "uhost-select-020")
	assert.NotContains(t, reply, "uhost-select-021")
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Len(t, eng.pendingResourceSelection.candidates, 20)
}

func TestPhase1CutoverMonitorEmptyFreshSnapshotRefreshesBeforeSelection(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			require.Equal(t, "DescribeCompShareInstance", action)
			return phase1MultipleInstanceDescribeResult(), nil
		},
	}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": 0,
		"UHostSet":   []any{},
	}, "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-select-001")
	assert.Contains(t, reply, "uhost-select-002")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Len(t, eng.pendingResourceSelection.candidates, 2)
}

func TestPhase1CutoverMonitorCandidateRefreshFailureDoesNotFallBackToReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			require.Equal(t, "DescribeCompShareInstance", action)
			return nil, fmt.Errorf("upstream unavailable")
		},
	}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	var traces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		traces = append(traces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, intent.FriendlyToolFailureReply)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	assert.Empty(t, mock.calls, "candidate refresh failure must not fall back to ReAct")
	assert.Nil(t, eng.pendingResourceSelection)
	require.Len(t, traces, 1)
	assert.Equal(t, string(intent.CutoverStatusFailureAfterTool), traces[0].CutoverStatus)
}

func TestPhase1CutoverMonitorSelectionPromptDoesNotUseGroundedRenderer(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
	}}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text: "renderer should not be used",
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")

	reply, err := eng.Chat(context.Background(), "show cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-select-001")
	assert.Empty(t, groundedRenderer.requests)
	assert.Empty(t, mock.calls)
	require.NotNil(t, eng.pendingResourceSelection)
}

func TestPhase1CutoverMonitorAmbiguousNameReturnsMatchingResourceSelection(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanForName("dup")}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{}},
		},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1AmbiguousInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show dup cpu monitor", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-dup-001")
	assert.Contains(t, reply, "uhost-dup-002")
	assert.Empty(t, executor.calls, "ambiguous selection must not call monitor")
	assert.Empty(t, mock.calls, "selection prompt must not fall back to ReAct")
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Len(t, eng.pendingResourceSelection.candidates, 2)
}

func TestPhase1CutoverMonitorSingleCandidateContinuesDirectly(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{
				map[string]any{
					"UHostId": "uhost-phase1-001",
					"Metrics": []any{
						map[string]any{"Name": "CPUUsageRate", "Value": 12},
					},
				},
			}},
		},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show cpu monitor", noopStep)

	require.NoError(t, err)
	assert.NotContains(t, reply, "请选择一个")
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, executor.calls)
	assert.Nil(t, eng.pendingResourceSelection)
	assert.Empty(t, mock.calls, "single-candidate continuation must bypass ReAct")
}

func TestPhase1ResourceInfoNoTargetStillListsInstancesWithoutPendingSelection(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "list my resources", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-select-001")
	assert.Contains(t, reply, "uhost-select-002")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	assert.Nil(t, eng.pendingResourceSelection)
	assert.Empty(t, mock.calls)
}

func TestResourceSelectionContinuationOrdinalRunsOriginalMonitorQuery(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	var monitorArgs map[string]any
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return phase1MultipleInstanceDescribeResult(), nil
			case "GetCompShareInstanceMonitor":
				monitorArgs = args
				return map[string]any{"CPU": float64(12.5), "GPU": float64(8), "VRAM": "1GB"}, nil
			default:
				return nil, fmt.Errorf("unexpected action %s", action)
			}
		},
	}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	firstReply, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	assert.Contains(t, firstReply, "uhost-select-001")
	assert.Contains(t, firstReply, "uhost-select-002")

	secondReply, err := eng.Chat(context.Background(), "2", noopStep)
	require.NoError(t, err)
	assert.Contains(t, secondReply, "CPU")
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, executor.calls)
	assert.Equal(t, []string{"uhost-select-002"}, monitorArgs["UHostIds"])
	assert.Empty(t, mock.calls, "selection continuation must not fall back to ReAct")
	assert.Nil(t, eng.pendingResourceSelection)
	assert.Len(t, planner.calls, 1, "selection reply should reuse the stored plan instead of calling the planner again")
}

func TestResourceSelectionContinuationExactIDRunsMonitorQuery(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	var monitorArgs map[string]any
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return phase1MultipleInstanceDescribeResult(), nil
			case "GetCompShareInstanceMonitor":
				monitorArgs = args
				return map[string]any{"CPU": float64(7), "GPU": float64(3), "VRAM": "1GB"}, nil
			default:
				return nil, fmt.Errorf("unexpected action %s", action)
			}
		},
	}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "uhost-select-001", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "CPU")
	assert.Equal(t, []string{"uhost-select-001"}, monitorArgs["UHostIds"])
	assert.Empty(t, mock.calls)
	assert.Nil(t, eng.pendingResourceSelection)
}

func TestResourceSelectionContinuationExactNameUsesGroundedRendererAndPlannerSource(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return phase1MultipleInstanceDescribeResult(), nil
			case "GetCompShareInstanceMonitor":
				return map[string]any{"CPU": float64(9), "GPU": float64(1), "VRAM": "1GB"}, nil
			default:
				return nil, fmt.Errorf("unexpected action %s", action)
			}
		},
	}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text: "grounded monitor summary",
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})
	onStep, events := collectSteps()

	_, err := eng.Chat(context.Background(), "CPU 高怎么办", onStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "select-a", onStep)

	require.NoError(t, err)
	assert.Equal(t, "grounded monitor summary", reply)
	require.Len(t, groundedRenderer.requests, 1)
	assertEngineComputedFact(t, groundedRenderer.requests[0].Envelope, "answer_mode", "troubleshooting")
	assertEngineComputedFact(t, groundedRenderer.requests[0].Envelope, "issue_metric", "cpu")
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, executor.calls)
	require.Len(t, *events, 2)
	assert.Equal(t, StepToolCall, (*events)[0].Type)
	assert.Equal(t, StepToolResult, (*events)[1].Type)
	assert.Equal(t, "GetCompShareInstanceMonitor", (*events)[0].Action)
	assert.Equal(t, observability.ToolSourcePlannerHandler, (*events)[0].Source)
	assert.Equal(t, observability.ToolSourcePlannerHandler, (*events)[1].Source)
	assert.NotEmpty(t, (*events)[1].RendererInputToolArgHashes)
	assert.Empty(t, mock.calls)
	assert.Nil(t, eng.pendingResourceSelection)
}

func TestResourceSelectionContinuationTroubleshootingFallbackAddsSafeContext(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return phase1MultipleInstanceDescribeResult(), nil
			case "GetCompShareInstanceMonitor":
				return map[string]any{"CPU": float64(0)}, nil
			default:
				return nil, fmt.Errorf("unexpected action %s", action)
			}
		},
	}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "select-a", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "CPU")
	assert.Contains(t, reply, "0")
	assert.Contains(t, reply, "当前这一次采样")
	assert.Contains(t, reply, "只能说明当前时刻")
	assert.Contains(t, reply, "历史波动")
	assert.Contains(t, reply, "控制台")
	assert.NotContains(t, reply, "信封")
	assert.NotContains(t, reply, "驱动")
	assert.NotContains(t, reply, "日志")
	assert.NotContains(t, reply, "SSH")
}

func TestResourceSelectionContinuationLoadAssessmentUsesGroundedRendererMode(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return phase1MultipleInstanceDescribeResult(), nil
			case "GetCompShareInstanceMonitor":
				return map[string]any{"CPU": "7%", "GPU": "0%"}, nil
			default:
				return nil, fmt.Errorf("unexpected action %s", action)
			}
		},
	}
	groundedRenderer := &mockGroundedRenderer{result: grounded.RenderResult{
		Text:            "从当前采样看，这台实例现在不算忙。",
		Model:           "deepseek-v4-flash",
		AttributionMode: grounded.AttributionEnvelope,
		EnvelopeHash:    "sha256:renderer-envelope",
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetGroundedRenderer(groundedRenderer, "deepseek-v4-flash")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "这台机器现在忙不忙", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "select-a", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "从当前采样看，这台实例现在不算忙。", reply)
	require.Len(t, groundedRenderer.requests, 1)
	assertEngineComputedFact(t, groundedRenderer.requests[0].Envelope, "answer_mode", "load_assessment")
	assert.Empty(t, mock.calls)
}

func TestMonitorTroubleshootingQuestionDoesNotMatchNormalGPUCardQuery(t *testing.T) {
	assert.False(t, isMonitorTroubleshootingQuestion("看一下显卡利用率"))
	assert.False(t, isMonitorTroubleshootingQuestion("highmem 机器现在 GPU 使用率是多少"))

	assert.True(t, isMonitorTroubleshootingQuestion("CPU 高怎么办"))
	assert.True(t, isMonitorTroubleshootingQuestion("这台机器有点卡顿，帮我排查"))
}

func TestMonitorLoadAssessmentQuestionIsNarrow(t *testing.T) {
	assert.True(t, isMonitorLoadAssessmentQuestion("qa-shadow 现在忙不忙？"))
	assert.True(t, isMonitorLoadAssessmentQuestion("这台机器空闲吗"))
	assert.True(t, isMonitorLoadAssessmentQuestion("GPU 忙不忙"))
	assert.True(t, isMonitorLoadAssessmentQuestion("负载怎么样"))

	assert.False(t, isMonitorLoadAssessmentQuestion("看一下显卡利用率"))
	assert.False(t, isMonitorLoadAssessmentQuestion("highmem 机器现在 GPU 使用率是多少"))
}

func TestMonitorLoadAssessmentFallbackIgnoresDiskPercentages(t *testing.T) {
	reply := monitorLoadAssessmentFallbackReply("CPU 使用率=7%; GPU 使用率=0%; 系统盘使用率=55%; 数据盘使用率=80%")

	assert.Contains(t, reply, "不算忙")
	assert.Contains(t, reply, "系统盘使用率=55%")
}

func TestMonitorHistoryUnsupportedReplyUsesCurrentScopeWording(t *testing.T) {
	assert.Contains(t, monitorHistoryUnsupportedReply, "当前暂不支持指定历史时间段的监控查询")
	assert.Contains(t, monitorHistoryUnsupportedReply, "实时监控")
	assert.NotContains(t, monitorHistoryUnsupportedReply, "暂不稳定支持")
}

func TestResourceSelectionContinuationDuplicateNameRepeatsPrompt(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanForName("dup")}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{"List": []any{}},
		},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1AmbiguousInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "show dup cpu monitor", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "dup", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-dup-001")
	assert.Contains(t, reply, "uhost-dup-002")
	assert.Empty(t, executor.calls, "ambiguous follow-up must not call monitor")
	assert.Empty(t, mock.calls)
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Equal(t, 1, eng.pendingResourceSelection.invalidAttempts)
}

func TestResourceSelectionContinuationInvalidOnceRepeatsPrompt(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "随便看看", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "uhost-select-001")
	assert.Contains(t, reply, "uhost-select-002")
	assert.Empty(t, mock.calls)
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Equal(t, 1, eng.pendingResourceSelection.invalidAttempts)
}

func TestResourceSelectionContinuationStaleInvalidClearsAndFallsBack(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react fallback"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	_, err = eng.Chat(context.Background(), "随便看看", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "还是不知道", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "react fallback", reply)
	assert.Nil(t, eng.pendingResourceSelection)
	assert.Len(t, mock.calls, 1, "second invalid selection should clear pending and resume normal routing")
}

func TestResourceSelectionContinuationHardBlockClearsPending(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1MonitorPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "normal fallback"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	require.NotNil(t, eng.pendingResourceSelection)

	reply, err := eng.Chat(context.Background(), "账号余额还有多少", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, accountBillingUnsupportedReply)
	assert.Nil(t, eng.pendingResourceSelection)

	reply, err = eng.Chat(context.Background(), "2", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "normal fallback", reply)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	assert.Len(t, mock.calls, 1, "numeric input after hard-block must not resume stale selection")
}

func TestPhase1CutoverInvalidAndIneligiblePlansFallBackToReAct(t *testing.T) {
	cases := []struct {
		name       string
		result     intent.PlannerResult
		wantStatus intent.CutoverStatus
	}{
		{
			name:       "fallback result",
			result:     intent.PlannerResult{Fallback: true, Plan: unknownEngineTestPlan()},
			wantStatus: intent.CutoverStatusFallbackInvalid,
		},
		{
			name: "hard block hint",
			result: intent.PlannerResult{Plan: intent.Plan{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: true,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			}},
			wantStatus: intent.CutoverStatusFallbackHardBlockHint,
		},
		{
			name: "low confidence",
			result: intent.PlannerResult{Plan: intent.Plan{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentResourceInfo,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.3,
			}},
			wantStatus: intent.CutoverStatusFallbackLowConfidence,
		},
		{
			name: "not enabled",
			result: intent.PlannerResult{Plan: intent.Plan{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingInstance,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			}},
			wantStatus: intent.CutoverStatusFallbackIneligible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planner := &scriptedIntentPlanner{results: []intent.PlannerResult{tc.result}}
			mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react fallback"}}}
			eng := NewWithDeps(mock, &mockExecutor{}, nil)
			eng.InitWithContext("test user")
			var traces []observability.PlannerTrace
			eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
				traces = append(traces, trace)
			})
			eng.SetIntentPlanner(planner, IntentPlannerOptions{
				EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery},
				Model:          "deepseek-v4-flash",
			})

			reply, err := eng.Chat(context.Background(), "phase1 fallback", noopStep)

			require.NoError(t, err)
			assert.Equal(t, "react fallback", reply)
			assert.Len(t, mock.calls, 1)
			require.Len(t, traces, 1)
			assert.Equal(t, string(tc.wantStatus), traces[0].CutoverStatus)
		})
	}
}

func TestPhase1CutoverFailureAfterToolDoesNotFallBackToReAct(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			return nil, fmt.Errorf("upstream unavailable")
		},
	}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	var traces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(trace observability.PlannerTrace) {
		traces = append(traces, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, intent.FriendlyToolFailureReply)
	assert.Empty(t, mock.calls, "tool failure after handler dispatch must not fall back to ReAct")
	require.Len(t, traces, 1)
	assert.Equal(t, string(intent.CutoverStatusFailureAfterTool), traces[0].CutoverStatus)
}

func TestPhase1CutoverReadExpensiveQuotaDenialUsesFriendlyMessage(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true, SubjectHash: "sha256:subject"},
		{Allowed: false, Reason: governance.ReasonQPSExceeded, SubjectHash: "sha256:subject", Err: governance.ErrRateLimited},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	assert.Equal(t, rateLimitQPSMessage, reply)
	assert.Empty(t, mock.calls, "quota denial after planner dispatch must not fall back to ReAct")
	assert.Empty(t, executor.calls)
}

func TestPhase1CutoverFallbackPreservesMonitorRecallForceTool(t *testing.T) {
	executor := monitorScenarioExecutor()
	planner := &scriptedIntentPlanner{
		results: []intent.PlannerResult{{Fallback: true, Plan: unknownEngineTestPlan()}},
	}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "GetCompShareInstanceMonitor", `{"UHostIds":["uhost-monitor-001"]}`),
		}},
		{Content: "monitor done"},
		{Content: "react fallback"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	_, err := eng.Chat(context.Background(), "show monitor", noopStep)
	require.NoError(t, err)
	assert.Equal(t, 1, eng.lastMonitorTurn)

	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery},
		Model:          "deepseek-v4-flash",
	})
	_, err = eng.Chat(context.Background(), "只看刚才那台机器的 GPU 和显存监控", noopStep)

	require.NoError(t, err)
	require.GreaterOrEqual(t, len(mock.calls), 3)
	assert.True(t, toolChoiceForMonitor(mock.calls[2]),
		"fallback-before-tool path must preserve existing monitor recall force-tool behavior")
}

func TestPhase1CutoverChecksPlannerQuotaOnce(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: phase1ResourcePlan()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1KnownInstanceDescribeResult(),
	}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{{Allowed: true}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.rateLimiter = limiter
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(phase1KnownInstanceDescribeResult(), "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	_, err := eng.Chat(context.Background(), "show phase1-demo resource", noopStep)

	require.NoError(t, err)
	require.Len(t, planner.calls, 1)
	require.Len(t, limiter.requests, 2)
	assert.Equal(t, governance.ClassLLM, limiter.requests[0].Class)
	assert.Equal(t, "intent_planner", limiter.requests[0].Action)
	assert.Equal(t, governance.ClassReadExpensiveTool, limiter.requests[1].Class)
	assert.Equal(t, "DescribeCompShareInstance", limiter.requests[1].Action)
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
		"开不了机",
		"启动失败",
		"无法启动",
		"启动不了",
		"开机失败",
		"stop 后启动失败",
		"stop后启动失败",
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
		"SSH 起不来",
		"服务起不来",
		"Jupyter 起不来",
		"起不来",
		"起不了",
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
