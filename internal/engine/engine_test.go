package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/refusal"
	"github.com/compshare-agent/internal/textutil"
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
	if isCreatePreferenceMockRequest(req) && !nextMockResponseLooksLikeCreatePreference(m) {
		return &llm.ChatResponse{Content: `{"workload_pref":"","image_pref":"","image_source":"","gpu_pref":"","zone_pref":"","purpose":""}`}, nil
	}
	if m.callIdx >= len(m.responses) {
		return &llm.ChatResponse{Content: "no more mock responses"}, nil
	}
	resp := m.responses[m.callIdx]
	m.callIdx++
	return &resp, nil
}

func isCreatePreferenceMockRequest(req llm.ChatRequest) bool {
	if len(req.Messages) == 0 {
		return false
	}
	return strings.Contains(req.Messages[0].Content, "创建/部署偏好抽取器")
}

func nextMockResponseLooksLikeCreatePreference(m *mockLLM) bool {
	if m == nil || m.callIdx >= len(m.responses) {
		return false
	}
	content := m.responses[m.callIdx].Content
	return strings.Contains(content, "workload_pref") ||
		strings.Contains(content, "image_pref") ||
		strings.Contains(content, "image_source") ||
		strings.Contains(content, "gpu_pref") ||
		strings.Contains(content, "zone_pref") ||
		strings.Contains(content, "purpose")
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
	before    func(governance.Request)
}

func (l *scriptedRateLimiter) Allow(req governance.Request) governance.Decision {
	if l.before != nil {
		l.before(req)
	}
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

type scriptedKnowledgeRetriever struct {
	results []knowledge.RetrievalResult
	calls   []knowledgeRetrievalCall
}

type knowledgeRetrievalCall struct {
	question    string
	productArea string
}

func (r *scriptedKnowledgeRetriever) RetrieveContext(_ context.Context, question, productArea string) knowledge.RetrievalResult {
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

func renderTestMessages(messages []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for _, message := range messages {
		b.WriteString(message.Content)
		b.WriteByte('\n')
	}
	return b.String()
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

func TestChat_AssignsTurnIdentityWhenTransportDoesNotProvideOne(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	_, err := eng.Chat(context.Background(), "把实例制作为镜像，名称 demo-image", noopStep)
	require.NoError(t, err)
	require.NotEmpty(t, eng.turnContextViewThisTurn.TurnID)
	require.Equal(t, "engine-turn-1", eng.turnContextViewThisTurn.TurnID)
}

func TestChat_ExternalTool_L0(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
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

func TestChat_ReActDisplayedInstanceListRecordsPendingSelection(t *testing.T) {
	var monitorIDs []string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-visible-1", "Name": "visible-one", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(16), "Memory": float64(65536), "Zone": "cn-wlcb-01"},
					map[string]any{"UHostId": "uhost-visible-2", "Name": "visible-two", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(16), "Memory": float64(65536), "Zone": "cn-wlcb-01"},
				},
				"TotalCount": float64(2),
				"RetCode":    0,
			}, nil
		case "GetCompShareInstanceMonitor":
			monitorIDs = stringSliceArg(args["UHostIds"])
			return map[string]any{"RetCode": 0}, nil
		default:
			return map[string]any{"Action": action, "RetCode": 0}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
		}},
		{Content: "1. visible-one\n2. visible-two"},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc2", "ReadCapability_monitor_query", `{"targets":[{"type":"uhost_id_user_input","value":"uhost-visible-1","source":"user_text"}]}`),
		}},
		{Content: "第 1 台 GPU 当前空闲。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)

	reply, err := eng.Chat(context.Background(), "我有哪些实例", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "visible-one")
	state, _, _ := eng.SessionStateSnapshot()
	require.Len(t, state.PendingSelectionItems, 2)
	assert.Equal(t, "uhost-visible-1", state.PendingSelectionItems[0].ID)
	assert.Equal(t, "uhost-visible-2", state.PendingSelectionItems[1].ID)

	followup, err := eng.Chat(context.Background(), "第1台 GPU 忙不忙", noopStep)
	require.NoError(t, err)
	require.Equal(t, []string{"uhost-visible-1"}, monitorIDs)
	assert.NotContains(t, followup, "请选择")
}

func TestChat_ReActHiddenInstanceLookupDoesNotRecordPendingSelection(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{
				"UHostSet": []any{
					map[string]any{"UHostId": "uhost-hidden-1", "Name": "hidden-one", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(16), "Memory": float64(65536), "Zone": "cn-wlcb-01"},
					map[string]any{"UHostId": "uhost-hidden-2", "Name": "hidden-two", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(16), "Memory": float64(65536), "Zone": "cn-wlcb-01"},
				},
				"TotalCount": float64(2),
				"RetCode":    0,
			}, nil
		default:
			return map[string]any{"Action": action, "RetCode": 0}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
		}},
		{Content: "hidden-one 和 hidden-two 都已检查，未发现异常。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1}, 1)

	reply, err := eng.Chat(context.Background(), "帮我检查实例", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "未发现异常")
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.PendingSelectionItems)
}

func TestChat_ExternalToolEventsCarryTraceMetadata(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "uhost-1"}}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
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
		if ev.Action != "ReadCapability_resource_info" {
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
		assert.Equal(t, map[string]any{}, callEvent.Args)
	}
	if assert.NotNil(t, resultEvent) {
		assert.Equal(t, observability.ToolSourceMainReAct, resultEvent.Source)
		assert.Equal(t, 0, resultEvent.Attempts, "aggregate capability event is distinct from its internal API attempt")
		assert.NotNil(t, resultEvent.TraceResult)
		assert.Equal(t, "resource_info", resultEvent.TraceResult["capability"])
		assert.Contains(t, resultEvent.TraceResult, "evidence")
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
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
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
	assert.Contains(t, toolMsg.Content, "resource_info")
	assert.NotContains(t, toolMsg.Content, "API")
}

func TestChat_HistoricalMonitorToolCallExecutesWithTemporalGuard(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{
		ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_monitor_history", `{"targets":[{"type":"uhost_id_user_input","value":"uhost-1","source":"user_text"}],"time_window":{"type":"preset","preset":"yesterday","source_span":"昨天"}}`),
		},
	}, {Content: "没有返回有效监控数据"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":   {"RetCode": 0, "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "State": "Running"}}},
		"GetCompShareInstanceMonitor": {"RetCode": 0},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "看昨天 uhost-1 的监控", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "没有返回有效监控数据")
	assert.Equal(t, []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}, executor.calls)
}

// The historical-monitor guard no longer rewrites the model's prose: the correct
// window now ships deterministically from the structured render (see
// RenderHistoricalMonitorSummary), not a post-hoc regex. When data is present the
// guard is a pure passthrough, so a window the model stated — and every unrelated
// date — survive verbatim (the old code rewrote them). The only remaining behavior
// is the all-no-data whole-answer override, covered by the Chat-level tests above.
func TestGuardMonitorNoDataFinalReplyPassthroughWhenDataPresent(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)
	eng.currentMonitorWindow = true
	eng.currentMonitorStart = 1777442400
	eng.currentMonitorEnd = 1777444200
	// currentMonitorTargets is empty → not all-no-data → the override does not fire.

	answer := "该实例创建于 2026-01-15，到期时间 2027-03-20。历史监控显示 2025-06-30 13:00 ~ 13:30 CPU 42%"
	reply := eng.guardMonitorNoDataFinalReply(answer)

	assert.Equal(t, answer, reply, "with data present the guard is a passthrough; it no longer rewrites windows, dates, or phrases")
}

func TestChat_ClearHistoricalMonitorQuestionMayUseReActHistoryTool(t *testing.T) {
	msg := "过去一小时 uhost-1 的 CPU 监控"
	mock := &mockLLM{responses: []llm.ChatResponse{{
		ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_monitor_history", `{"targets":[{"type":"uhost_id_user_input","value":"uhost-1","source":"user_text"}],"metrics":["cpu"],"time_window":{"type":"relative","amount":1,"unit":"hour","source_span":"过去一小时"}}`),
		},
	}, {Content: "没有返回有效监控数据"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":   {"RetCode": 0, "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "State": "Running"}}},
		"GetCompShareInstanceMonitor": {"RetCode": 0},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), msg, noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "没有返回有效监控数据")
	assert.NotEmpty(t, mock.calls)
	assert.Contains(t, executor.calls, "GetCompShareInstanceMonitor")
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
			assert.Contains(t, ev.Message, "未开放")
		}
	}
	assert.True(t, hasBlocked, "L2 operation should be blocked")
}

func TestChat_HiddenDirectWriteCannotBypassActionFirst(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"StopCompShareInstance": {"RetCode": 0},
	}}
	confirmCalls := 0
	confirmFn := func(action string, args map[string]any) bool {
		confirmCalls++
		return true
	}
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
	assert.Empty(t, executor.calls)
	assert.Zero(t, confirmCalls, "a name absent from the tool window must not reach confirmation")
}

func TestChat_HiddenDirectWriteEmitsAnHonestBlockedStep(t *testing.T) {
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
		if ev.Type == StepBlocked && strings.Contains(ev.Message, "未开放") {
			hasBlocked = true
		}
		assert.NotContains(t, ev.Message, "已取消",
			"a not-granted confirm must not falsely claim the user cancelled")
	}
	assert.True(t, hasBlocked)
}

func TestChat_HiddenDirectWriteDoesNotFabricateAConfirmationTrace(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "StopCompShareInstance", `{"UHostId":"uhost-xxx"}`),
		}},
		{Content: "操作未执行。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	var confirmations []observability.ConfirmationTrace
	eng.SetConfirmationTraceObserver(func(trace observability.ConfirmationTrace) {
		confirmations = append(confirmations, trace)
	})

	_, err := eng.ChatWithOptions(context.Background(), "关机", noopStep, ChatOptions{
		ConfirmResultFunc: func(string, map[string]any) ConfirmationResult {
			return ConfirmationResult{TerminalReason: observability.ConfirmationReasonTimeout}
		},
	})
	require.NoError(t, err)
	assert.Empty(t, confirmations, "a hidden tool name is rejected before any consent card is created")
}

func TestChat_InvalidToolArgs(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `not json`),
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
				toolCall("tc", "ReadCapability_resource_info", `{}`),
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
	// No SearchKnowledge ran (plain reads only) → empty ledger → the loop-ceiling
	// recovery must NOT fire and the canned message stays byte-identical. Pins the
	// no-fabrication contract that gates synthesizeOnBudgetExceeded at this exit.
	assert.Empty(t, eng.searchKnowledgeHitsThisTurn, "no evidence gathered → recovery must not fabricate over the ceiling refusal")
}

// Knowledge-route tools run locally and never reach the API executor.
func TestKnowledgeTool_DoesNotCallExecutor(t *testing.T) {
	executor := &mockExecutor{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "SearchKnowledge", `{"query":"A100 规格"}`),
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

// Two calls in one round are both executed and returned to the model.
func TestMultipleToolCalls(t *testing.T) {
	idx0 := 0
	idx1 := 1
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			{ID: "tc1", Type: openai.ToolTypeFunction, Index: &idx0,
				Function: openai.FunctionCall{Name: "ReadCapability_resource_info", Arguments: `{}`}},
			{ID: "tc2", Type: openai.ToolTypeFunction, Index: &idx1,
				Function: openai.FunctionCall{Name: "ReadCapability_gpu_specs_query", Arguments: `{"gpu_type":"4090"}`}},
		}},
		{Content: "对比结果"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":               {"UHostSet": []any{}, "RetCode": 0},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{}, "RetCode": 0},
	}}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "对比账号实例和 4090 规格", onStep)
	assert.NoError(t, err)
	assert.Equal(t, "对比结果", reply)

	// Should have 4 events: 2x (tool_call + tool_result)
	toolCalls := 0
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Source == observability.ToolSourceMainReAct {
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

	// A name absent from the exact request window is rejected at the model boundary.
	hasBlocked := false
	for _, ev := range *events {
		if ev.Type == StepBlocked && strings.Contains(ev.Message, "未开放") {
			hasBlocked = true
		}
	}
	assert.True(t, hasBlocked, "unknown action should produce a blocked event")
}

func TestTrimHistory(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	// The fixture is DERIVED from the ceiling, not hardcoded. It used to be a
	// literal 50 — chosen to overflow a ceiling of 40 — which meant raising the
	// ceiling sailed this test straight down trimHistory's no-op branch: no trim,
	// nothing asserted, still green. It then derived from maxHistoryMessages, and
	// now from maxRawHistoryRunes, for the same reason.
	const perMessage = 500
	pairs := maxRawHistoryRunes/perMessage + 4 // comfortably over, whatever the budget is
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
	}
	for i := 0; i < pairs; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("q%d", i) + strings.Repeat("问", perMessage)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
				Content: fmt.Sprintf("a%d", i) + strings.Repeat("答", perMessage)},
		)
	}
	before := len(eng.messages)
	require.Greater(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes,
		"input must overflow the ceiling or trimHistory is a no-op")

	eng.trimHistory()

	assert.Less(t, len(eng.messages), before, "the trim must actually have fired")
	assert.LessOrEqual(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, "system prompt", eng.messages[0].Content,
		"the system prompt is kept whatever the budget, and is not charged against it")

	// The cut takes the OLDEST. Keeping the oldest and dropping the newest would
	// satisfy the size bound and destroy the conversation.
	lastMsg := eng.messages[len(eng.messages)-1]
	assert.True(t, strings.HasPrefix(lastMsg.Content, fmt.Sprintf("a%d", pairs-1)),
		"the newest message must survive")
}

// The ceiling is a SIZE, so a session of many small turns must not be trimmed for
// having many turns. This is the count deletion stated as behaviour: at the old
// maxHistoryMessages = 120 this fixture lost everything before turn 141, while
// costing a twentieth of the budget it now answers to.
func TestTrimHistoryDoesNotTrimManySmallTurns(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
	}
	const pairs = 200
	for i := 0; i < pairs; i++ {
		eng.messages = append(eng.messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("好贵%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("明白%d", i)},
		)
	}
	require.Less(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes,
		"premise: %d short turns must genuinely fit the size budget", pairs)

	eng.trimHistory()

	assert.Len(t, eng.messages, 1+2*pairs,
		"a session was trimmed for its turn COUNT; the ceiling is supposed to be a size")
	assert.Contains(t, renderTestMessages(eng.messages), "好贵0", "including its opening turn")
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

// The safe-boundary rule: a cut may never leave an orphaned tool response, nor an
// assistant(tool_calls) without the results answering it. That is a malformed
// request, which a provider rejects outright rather than degrading.
//
// It is driven through rawHistoryCutPoint rather than through trimHistory,
// because trimHistory CANNOT reach this case. stripHistoricalToolTranscript runs
// first and deletes every tool message and every assistant(tool_calls) before the
// cut point is computed, so the boundary walk sees a list that already has no
// unsafe positions in it. Two tests (SkipsToolCallGroup, CutPointIsOrphanedTool)
// drove it through trimHistory anyway: they asserted a premise about the naive cut
// landing on a tool message against the PRE-strip list, then asserted "the first
// kept message is not a tool" against a list the strip had already emptied of
// them. Both halves passed without the walk ever running. Testing the unit that
// owns the rule is the only way to reach it while the strip stands in front.
//
// Budgets are swept exhaustively rather than sampled: the interesting cuts are
// the ones that land mid-group, and which budget does that depends on message
// sizes the test should not have to predict.
func TestRawHistoryCutPointNeverLandsInsideAToolGroup(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system"}}
	for i := 0; i < 5; i++ {
		call := fmt.Sprintf("tc%d", i)
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("u%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{toolCall(call, "DescribeCompShareInstance", `{}`)}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: call,
				Content: strings.Repeat("资", 40)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("summary%d", i)},
		)
	}

	// An INDEPENDENT reimplementation of where the cut would go with no boundary
	// rule at all. Comparing against the function's own internals would agree with
	// any bug they contained.
	naive := func(budget int) int {
		spent, candidate := 0, len(msgs)
		for i := len(msgs) - 1; i >= 1; i-- {
			spent += len([]rune(msgs[i].Content))
			for _, c := range msgs[i].ToolCalls {
				spent += len([]rune(c.Function.Name)) + len([]rune(c.Function.Arguments))
			}
			if spent > budget {
				break
			}
			candidate = i
		}
		return candidate
	}

	total := assembledRequestRunes(msgs[1:])
	moved, cuts := 0, 0
	for budget := 1; budget < total; budget++ {
		cut := rawHistoryCutPoint(msgs, budget)
		if cut < 0 {
			continue
		}
		cuts++
		if cut != naive(budget) {
			moved++
		}
		kept := append([]openai.ChatCompletionMessage{msgs[0]}, msgs[cut:]...)
		require.NotEqual(t, openai.ChatMessageRoleTool, msgs[cut].Role,
			"budget=%d cut at %d, an orphaned tool response", budget, cut)
		assertToolCallPairsValid(t, kept)
		assert.LessOrEqual(t, assembledRequestRunes(msgs[cut:]), budget,
			"budget=%d: aligning forward may only ever shrink the kept slice", budget)
	}

	require.Greater(t, cuts, 0, "premise: some budget must actually produce a cut")
	require.Greater(t, moved, 0,
		"the boundary walk never moved a cut in %d budgets — every unsafe position was "+
			"already safe, so this test is not exercising the rule it names", cuts)
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
	// No SearchKnowledge ran before the error → empty ledger → the LLM-error
	// recovery must NOT fire and the error must still propagate (never masked by a
	// fabricated answer). Pins the no-evidence half of the recovery contract.
	assert.Empty(t, eng.searchKnowledgeHitsThisTurn, "no evidence gathered → LLM error must propagate, not be masked by recovery")
}

func TestChat_LLMRateLimitDenialSkipsLLM(t *testing.T) {
	const sensitiveReply = "Jupyter Token：server-owned-token"
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be used"}}}
	limiter := &scriptedRateLimiter{decisions: []governance.Decision{{
		Allowed:     false,
		Reason:      governance.ReasonQPSExceeded,
		SubjectHash: "sha256:subject",
		Err:         governance.ErrRateLimited,
	}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	limiter.before = func(governance.Request) {
		// Model a protected read that completed before the next Agent call was
		// denied. The terminal rate-limit message must not discard its result.
		eng.sensitiveRepliesThisTurn = []string{sensitiveReply}
	}
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "hello", noopStep)

	require.NoError(t, err)
	assert.Equal(t, sensitiveReply+"\n\n请求过于频繁，请稍后再试。", reply)
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
	rawTenantIdentity := "12345:67890"
	subjectHash, ok := governance.SubjectKeyFromOrganization(12345, 67890)
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
	assert.NotContains(t, fmt.Sprintf("%+v", observed[0]), rawTenantIdentity)
}

func TestChat_ReadExpensiveRateLimitDenialBecomesToolResult(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
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
	assertNoStepTypeForAction(t, *events, StepError, "ReadCapability_resource_info")
}

func TestChat_ReadExpensiveTargetCapBecomesToolResult(t *testing.T) {
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = fmt.Sprintf("uhost-%02d", i)
	}
	targets := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, map[string]any{"type": "uhost_id_user_input", "value": id, "source": "user_text"})
	}
	rawArgs, err := json.Marshal(map[string]any{"targets": targets})
	require.NoError(t, err)
	hosts := make([]any, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, map[string]any{"UHostId": id, "State": "Running"})
	}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"RetCode": 0, "UHostSet": hosts},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_monitor_query", string(rawArgs)),
		}},
		{Content: "scope narrowed"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	onStep, events := collectSteps()

	reply, err := eng.Chat(context.Background(), "monitor all", onStep)

	require.NoError(t, err)
	assert.Equal(t, "scope narrowed", reply)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	assert.Len(t, mock.calls, 2)
	assertStepWithType(t, *events, StepBlocked, "GetCompShareInstanceMonitor", toolCapExceededMessage)
	assertNoStepTypeForAction(t, *events, StepError, "ReadCapability_monitor_query")
}

func TestWorkflowInternalReadExpensiveConsumesSubjectQuotaButSkipsTurnBudget(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{"UHostId": "uhost-stop-001", "State": "Running", "Zone": "cn-wlcb-01"}},
		},
		"DescribeCompShareSupportZone": {
			"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}},
		},
		"StopCompShareInstance": {"RetCode": 0},
	}}
	limiter := &scriptedRateLimiter{}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	eng.readExpensiveCallsThisTurn = maxReadExpensiveCallsPerTurn

	reply := eng.executeResolvedWorkflow(context.Background(), mustConfirmable("StopInstanceWorkflow", map[string]any{"UHostId": "uhost-stop-001"}, zoneRefData(nil)), noopStep)

	assert.Contains(t, reply, "已向实例 uhost-stop-001 提交关机请求", "successful stop reports asynchronous request acceptance")
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

	reply := eng.executeResolvedWorkflow(context.Background(), mustConfirmable("StopInstanceWorkflow", map[string]any{"UHostId": "uhost-stop-001"}, zoneRefData(nil)), onStep)

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

	reply := eng.executeDiagnosis(context.Background(), "DiagnoseBilling", map[string]any{"UHostId": "uhost-diag-001"}, onStep)

	assert.Equal(t, finalReplyPrefix+readExpensiveTurnBudgetMessage, reply)
	assert.Empty(t, executor.calls, "diagnosis internal read-expensive calls must stop when turn budget is exhausted")
	assertStepWithType(t, *events, StepBlocked, "DescribeCompShareInstance", readExpensiveTurnBudgetMessage)
	assertStepWithType(t, *events, StepBlocked, "DiagnoseBilling", readExpensiveTurnBudgetMessage)
	assertNoStepTypeForAction(t, *events, StepError, "DescribeCompShareInstance")
}

// A turn that reports two symptoms at once ("CPU 跑满" + "一直在扣费") must get BOTH
// answered. The billing exit used to be finalReplyPrefix, which ended the turn:
// live probes (N=5, both description arms) showed the Agent fetch the monitoring
// evidence, then call DiagnoseBilling, then return a bare price card — the CPU
// question unanswered and the evidence already gathered discarded, 5/5 runs.
//
// The two guarantees are now separate: the figures are delivered byte-exact, and
// the turn survives. The model's context must never contain the rendered figures —
// that, not turn termination, is what makes re-summing periods / extrapolating an
// hourly quote to monthly spend / inferring a free quota from a zero price
// impossible, since all three require seeing the numbers.
func TestDiagnoseBillingAnswersTheRestOfTheTurnAndHidesFiguresFromTheModel(t *testing.T) {
	const cpuAnswer = "CPU 100% 的原因是 3 个 kworkerd 进程占满了核心。"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-bill-001", "State": "Running", "ChargeType": "Dynamic",
				"InstancePrice": float64(1), "DiskPrice": float64(0.1),
			}},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`)}},
		{Content: cpuAnswer},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "这台 CPU 一直 100% 跑满，而且费用也一直在扣", noopStep)
	require.NoError(t, err)

	// The turn continued past billing: a second LLM round happened at all.
	require.Len(t, mock.calls, 2, "billing must not terminate the turn — the Agent needs a round to answer the CPU half")
	// Both halves are in the one reply.
	require.Contains(t, reply, cpuAnswer, "the non-billing half of the question must still be answered")
	require.Contains(t, reply, "uhost-bill-001", "the deterministic billing card must still reach the user")

	// Isolate the verbatim card and prove the model never received it. Guarded
	// against vacuity: an empty or figure-free card would make the loop below pass
	// no matter what the engine did.
	card := strings.TrimSpace(strings.TrimSuffix(reply, cpuAnswer))
	require.Contains(t, card, "uhost-bill-001", "test setup: the isolated card must be the real rendered block")
	require.Regexp(t, `\d`, card, "test setup: the card must carry figures, else the leak assertion is vacuous")
	for callIdx, call := range mock.calls {
		for msgIdx, msg := range call.Messages {
			require.NotContains(t, msg.Content, card,
				"rendered billing figures leaked into the model's context (call %d, message %d)", callIdx, msgIdx)
		}
	}
	// What the model saw instead: the amount-free note.
	var sawObservation bool
	for _, msg := range mock.calls[1].Messages {
		if strings.Contains(msg.Content, verbatimBlockObservation) {
			sawObservation = true
		}
	}
	require.True(t, sawObservation, "the model must be told the figures were already shown, or it will claim it cannot look pricing up")
}

// A turn asking ONLY about price must come back as the card and nothing else.
//
// Making the billing exit non-terminal created this hazard: the Agent gets a round it
// did not previously have, and an empty final answer used to be overwritten with
// "本次没有生成有效回复" — so stopping was illegal and it padded with generic prose
// instead ("费用通常按以下部分拆分…", vaguer than the card above it, observed 3/5 live).
// A delivered verbatim block now counts as a non-empty turn, which makes silence the
// cheap, correct move.
func TestPureBillingTurnReturnsTheCardAloneWhenTheAgentAddsNothing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-bill-001", "State": "Running", "ChargeType": "Dynamic",
				"InstancePrice": float64(1), "DiskPrice": float64(0.1),
			}},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`)}},
		{Content: ""}, // nothing to add: the card already answers the question
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "这台实例现在每小时多少钱？", noopStep)
	require.NoError(t, err)

	require.Contains(t, reply, "uhost-bill-001", "the card must still be delivered")
	require.NotContains(t, reply, emptyReplyFallbackMessage,
		"a turn that already delivered the card is not an empty turn")
	require.Equal(t, strings.TrimSpace(reply), strings.TrimSpace(eng.verbatimBlocksThisTurn[0]),
		"the reply must be exactly the card — no trailing prose, no apology")
}

// streamingScriptedLLM replays a script AND fires OnTextDelta for each text
// response, the way the real client streams. The plain mockLLM never streams, so
// a bug that only shows on the token stream hides behind it.
type streamingScriptedLLM struct {
	responses []llm.ChatResponse
	idx       int
	calls     []llm.ChatRequest
}

func (m *streamingScriptedLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls = append(m.calls, req)
	var resp llm.ChatResponse
	if m.idx < len(m.responses) {
		resp = m.responses[m.idx]
		m.idx++
	}
	if req.OnTextDelta != nil && resp.Content != "" {
		req.OnTextDelta(resp.Content)
	}
	return &resp, nil
}

// The web client renders the assistant bubble from the token stream, not from the
// returned reply — the reply is only what gets persisted. So a verbatim block that
// lands in one but not the other is a user-visible bug, and every test and live
// probe so far ran with a nil emitter, leaving this path unexercised.
//
// The ordinary answer path already holds reply == join(deltas)
// (TestOnTextDeltaReplaysRawDeltasWhenNoOverride); composing a block in at the turn
// exit must not break that, or the streamed bubble and the reloaded conversation
// disagree about what the assistant said.
func TestVerbatimBillingBlockStreamsExactlyAsItIsPersisted(t *testing.T) {
	const cpuAnswer = "CPU 100% 的原因是 3 个 kworkerd 进程占满了核心。"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-bill-001", "State": "Running", "ChargeType": "Dynamic",
				"InstancePrice": float64(1), "DiskPrice": float64(0.1),
			}},
		},
	}}
	mock := &streamingScriptedLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "DiagnoseBilling", `{"UHostId":"uhost-bill-001"}`)}},
		{Content: cpuAnswer},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "这台 CPU 一直 100% 跑满，而且费用也一直在扣", noopStep, ChatOptions{
		OnTextDelta: func(d string) { deltas = append(deltas, d) },
	})
	require.NoError(t, err)

	// Non-vacuity: without a real card carrying figures the assertions below would
	// pass on an engine that streamed nothing at all.
	require.Len(t, eng.verbatimBlocksThisTurn, 1, "test setup: this turn must produce exactly one billing card")
	card := eng.verbatimBlocksThisTurn[0]
	require.Regexp(t, `\d`, card, "test setup: the card must carry figures")
	require.Contains(t, reply, cpuAnswer, "test setup: the turn must have continued past billing")

	streamed := strings.Join(deltas, "")
	assert.Equal(t, reply, streamed,
		"the streamed bubble must be byte-identical to the persisted reply")
	assert.Equal(t, 1, strings.Count(streamed, card),
		"the card must be streamed exactly once — twice renders it twice in the bubble")
	assert.Less(t, strings.Index(streamed, card), strings.Index(streamed, cpuAnswer),
		"the card must stream before the answer that follows it")
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
	onStep, events := collectSteps()

	reply := eng.executeDiagnosis(context.Background(), "DiagnoseBilling", map[string]any{}, onStep)

	assert.Contains(t, reply, "uhost-bill-001")
	assert.True(t, strings.HasPrefix(reply, verbatimReplyPrefix),
		"billing figures must reach the user verbatim so the model never re-derives them")
	assert.False(t, strings.HasPrefix(reply, finalReplyPrefix),
		"verbatim must not also mean terminal: ending the turn here discarded every other symptom in the question")
	var readExpensive []governance.Request
	for _, req := range limiter.requests {
		if req.Class == governance.ClassReadExpensiveTool {
			readExpensive = append(readExpensive, req)
		}
	}
	require.Len(t, readExpensive, 2, "DiagnoseBilling intentionally consumes quota for both list and price-detail Describe calls")
	assert.Equal(t, "DescribeCompShareInstance", readExpensive[0].Action)
	assert.Equal(t, "DescribeCompShareInstance", readExpensive[1].Action)
	assertStepWithType(t, *events, StepToolResult, "DiagnoseBilling", "诊断完成")
}

func TestChat_ReadOnlyToolDoesNotConsumeMutatingQuota(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_resource_info", `{}`),
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
	require.Len(t, limiter.requests, 2)
	assert.Equal(t, governance.ClassLLM, limiter.requests[0].Class)
	assert.Equal(t, governance.ClassLLM, limiter.requests[1].Class)
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
	assert.Contains(t, names, capability.ReadToolName(intent.IntentResourceInfo))
	assert.NotContains(t, names, "ReadPlatformCapability")
	assert.Contains(t, names, "ReadCapability_instance_access")
	assert.NotContains(t, names, "DiagnoseSSH")
	assert.NotContains(t, names, "DescribeCompShareJupyterToken")
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
	assertStepWithType(t, *events, StepBlocked, "StopInstanceWorkflow", "未开放")
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

func TestChatHiddenPasswordActionNeverReachesTheTraceOrExecutor(t *testing.T) {
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

	var blockedEvent *StepEvent
	reply, err := eng.Chat(context.Background(), "reset password", func(ev StepEvent) {
		if ev.Type == StepBlocked && ev.Action == "ResetCompShareInstancePassword" {
			copy := ev
			blockedEvent = &copy
		}
	})

	require.NoError(t, err)
	require.Equal(t, "done", reply)
	require.NotNil(t, blockedEvent)
	assert.Empty(t, exec.calls)
	assert.NotContains(t, fmt.Sprintf("%+v", blockedEvent), "Secret123!")
}

// TestKnowledgeTool_ArgsFiltered pins the knowledge route's arg allowlist.
// SearchKnowledge is now that route's only member (the GetGPUSpecs this used to
// drive is deleted with the static GPU table).
//
// It deliberately does NOT drive Chat and inspect the StepToolCall event, the way
// its ancestor did. That shape CANNOT work here: executeSearchKnowledge
// hand-builds its event args as {"query": query} (engine.go), so the event never
// carries the raw map and `NotContains(ev.Args, "evil")` holds no matter what the
// filter does. Verified by mutation — deleting the FilterArgs call left the
// event-driven version green, i.e. it was still a vacuous gate after being
// retargeted. Assert the filter itself, which is the thing with teeth.
func TestKnowledgeTool_ArgsFiltered(t *testing.T) {
	filtered := tools.NewSafeToolExecutor(&mockExecutor{}).FilterArgs("SearchKnowledge", map[string]any{
		"query":        "4090 显存",
		"context_hint": "创建实例",
		"evil":         "injection",
	})
	assert.Equal(t, map[string]any{"query": "4090 显存", "context_hint": "创建实例"}, filtered,
		"unknown params must be stripped before a knowledge tool sees them")
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
// The subject is the tool-result WIRE FORMAT: whatever a tool returns must reach
// the model as parseable JSON on a role=tool message. The vehicle moved from the
// deleted GetGPUSpecs to a surviving external read; the "96" assertion went with
// it, because it pinned H20's VRAM from the static table — a platform fact this
// repo no longer stores. The JSON contract it was really guarding is unchanged.
func TestToolResult_IsValidJSON(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{}`),
		}},
		{Content: "done"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0, "TotalCount": float64(0)},
	}}, nil)
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
	assert.NotEmpty(t, parsed, "tool result must be a non-empty JSON object, not a bare string")
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

func TestChat_InstanceAccessTool_SSHStopped(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-diag-001", "State": "Stopped"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_instance_access", `{"targets":[{"type":"uhost_id_user_input","value":"uhost-diag-001","source":"user_text"}],"access_type":"ssh"}`),
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
		if ev.Type == StepToolCall && ev.Action == "ReadCapability_instance_access" {
			hasDiagCall = true
		}
	}
	assert.True(t, hasDiagCall)

	toolMsg := mock.calls[1].Messages[len(mock.calls[1].Messages)-1]
	assert.Equal(t, openai.ChatMessageRoleTool, toolMsg.Role)
	assert.Contains(t, toolMsg.Content, `"cloud_precheck_status"`)
	assert.Contains(t, toolMsg.Content, `"value":"blocked"`)
	assert.Contains(t, toolMsg.Content, `"value":"Stopped"`)
}

func TestChat_InstanceAccessDiagnosisCanUseKnowledgeWithoutRewritingFacts(t *testing.T) {
	const chunkID = "pod-port-configuration"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "cpod-diag-001", "Name": "comfy-pod", "State": "Running",
				"InstanceType": "Container",
				"Ports": map[string]any{
					"TcpPorts": []any{float64(22)},
				},
			}},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("access", "ReadCapability_instance_access", `{"targets":[{"type":"uhost_id_user_input","value":"cpod-diag-001","source":"user_text"}],"access_type":"custom_port","protocol":"tcp","port":8188}`),
		}},
		{ToolCalls: []openai.ToolCall{
			toolCall("knowledge", "SearchKnowledge", `{"query":"Pod 添加 TCP 端口映射的方法"}`),
		}},
		plannerEcho("Pod 添加 TCP 端口映射的方法"),
		{Content: "诊断结果：Pod 当前云侧端口配置中没有登记 TCP 8188。\n\n处理建议：按平台文档添加 TCP 8188 映射，然后确认应用监听该端口。[[" + chunkID + "]]"},
	}}
	chunk := knowledge.KBChunk{
		ChunkID: chunkID, KBVersion: "test", Title: "Pod 端口配置",
		Content: "Pod 可在实例端口配置中添加 TCP 端口映射；修改后还要确认实例内应用监听相同端口。",
	}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "test", Hits: []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "cpod-diag-001 的 8188 端口打不开，怎么修", noopStep)
	require.NoError(t, err)
	// Three Agent turns — diagnose, retrieve guidance, answer — plus the internal
	// query planner that the SearchKnowledge runs through. The planner is control,
	// not an Agent turn, so it is named here rather than folded into the count.
	require.Len(t, mock.calls, 4, "three Agent turns plus one internal planner call")
	require.Len(t, retriever.calls, 1, "an echoing planner leaves the retrieval the Agent asked for unchanged")
	assert.Contains(t, reply, "Pod 当前云侧端口配置中没有登记 TCP 8188")
	assert.Contains(t, reply, "按平台文档添加 TCP 8188 映射")
	assert.NotContains(t, reply, "防火墙拒绝")
	assert.NotContains(t, reply, "{{READ_OBSERVATION_")
	assert.NotContains(t, reply, "[[")
}

func TestChat_InstanceAccessTokenReturnsThroughTheCentralAgent(t *testing.T) {
	const token = "stable-console-visible-token"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-token-001", "Name": "token-vm", "State": "Running",
				"InstanceType": "UHost",
			}},
		},
		"DescribeCompShareJupyterToken": {"JupyterToken": token},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("token", "ReadCapability_instance_access", `{"targets":[{"type":"uhost_id_user_input","value":"uhost-token-001","source":"user_text"}],"access_type":"jupyter_token"}`),
		}},
		{Content: "Token 已获取。"},
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "查询 uhost-token-001 的 Jupyter Token", noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 2)
	assert.Contains(t, reply, token)
	toolResult := mock.calls[1].Messages[len(mock.calls[1].Messages)-1].Content
	assert.NotContains(t, toolResult, token, "the opaque value must not pass through the model")
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
	assert.Contains(t, executor.calls, "DescribeCompShareJupyterToken")
}

func TestChat_InstanceAccessTool_UnknownArgsRejectedBeforeUpstream(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-diag-002", "State": "Running"},
			},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "ReadCapability_instance_access", `{"targets":[{"type":"uhost_id_user_input","value":"uhost-diag-002","source":"user_text"}],"access_type":"ssh","evil":"injection"}`),
		}},
		{Content: "done"},
	}}
	onStep, events := collectSteps()
	eng := NewWithDeps(mock, executor, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	eng.Chat(context.Background(), "test", onStep)

	assert.Empty(t, executor.calls)
	assertStepWithType(t, *events, StepError, "ReadCapability_instance_access", "unknown field")
}

// Freshness is compiled into AgentContext instead of being injected as a
// turn-local system-message patch. The compiler contract is covered by
// agent_context_compiler_test.go.
func TestFreshness_DoesNotInjectEphemeralSystemMessage(t *testing.T) {
	// Structural check: when stale, the note must appear immediately before
	// the latest user message, not at index 1. This maximizes model attention
	// in long conversations where the user's ask is far from the system prompt.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "ReadCapability_resource_info", `{}`)}},
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

	for _, msg := range turn2Msgs {
		assert.NotContains(t, msg.Content, "实例状态信息可能已过时")
	}
}

func TestRegistryInvalidatesAfterSuccessfulMutatingTool(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"RetCode": 0, "TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-a", "Name": "a", "State": "Stopped",
				"Region": "cn-bj2", "Zone": "cn-bj2-04",
			}},
		},
		"StartCompShareInstance": {"RetCode": 0},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("read", "ReadCapability_resource_info", `{"resource_ids":["uhost-a"]}`),
		}},
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "RequestStartInstance", `{"UHostId":"uhost-a","StartMode":"normal"}`),
		}},
	}}
	eng := NewWithDeps(mock, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(true)
	eng.registry = entity.NewRegistry(entity.WithClock(func() time.Time { return now }))
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"RetCode":    0,
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "a", "State": "Stopped"},
		},
	}, string(entity.SyncEventSyncRefresh)))
	require.False(t, eng.registry.NeedsRefresh(now.Add(time.Second)))
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "start uhost-a", noopStep)

	assert.NoError(t, err)
	assert.Contains(t, reply, "已为实例 uhost-a 执行开机")
	assert.True(t, eng.registry.NeedsRefresh(now.Add(time.Second)),
		"executor calls=%v proposal disposition=%q", executor.calls, eng.ActionProposalDispositionThisTurn())
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
	}, string(entity.SyncEventSyncRefresh)))

	state := eng.RegistryTraceState(now.Add(12 * time.Second))

	assert.NotEmpty(t, state.SnapshotID)
	assert.Equal(t, int64(12), state.AgeSeconds)
	assert.Equal(t, string(entity.SyncEventSyncRefresh), state.SyncEvent)
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
	}, string(entity.SyncEventSyncRefresh)))

	snap := eng.RegistrySnapshot()
	require.NotEmpty(t, snap.SnapshotID)
	snap.Instances["uhost-trace"] = entity.InstanceSnapshot{UHostId: "uhost-trace", Name: "mutated"}

	fresh := eng.RegistrySnapshot()
	assert.Equal(t, "trace-host", fresh.Instances["uhost-trace"].Name)
}

func TestMonitorHistoryUnsupportedReplyUsesCurrentScopeWording(t *testing.T) {
	assert.Contains(t, refusal.MonitorHistoryUnsupported, "最多查询 20 台实例")
	assert.Contains(t, refusal.MonitorHistoryUnsupported, "30 天")
	assert.NotContains(t, refusal.MonitorHistoryUnsupported, "暂不支持指定历史时间段")
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
			assert.Equal(t, tc.want, textutil.Normalize(tc.in))
		})
	}
}

// TestChat_TokenBudgetExceeded_BreaksAtIterationBoundary — verifies the
// per-turn token cap fires at the TOP of a ReAct iteration, AFTER the
// previous iteration's tool_call/tool_result pair has fully completed.
//
// Setup: round 0 LLM returns one tool_call + reports 60000 tokens used,
// which is over the 50000 cap. The engine MUST:
//  1. Still execute that tool_call and append its tool_result (so the WS
//     client never sees an orphan tool_call frame — protocol invariant).
//  2. NOT make a second LLM call (round 1 budget check trips first).
//  3. Return tokenBudgetExceededMessage with status mapped to "blocked"
//     via the hard-block observer (Category="token_budget_exceeded").
//
// WHY: the (c) constraint from 2026-05-21 review — a token cap that
// breaks mid-tool would leave the client framing broken. Encode the
// boundary placement as a test so future refactors can't silently move
// the check inside the tool_call inner loop.
func TestChat_TokenBudgetExceeded_BreaksAtIterationBoundary(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		// Round 0: emits a tool_call AND reports 60k tokens (over the
		// 50k cap). Second response would be returned if round 1 ran.
		{
			ToolCalls: []openai.ToolCall{
				toolCall("tc1", "ReadCapability_resource_info", `{}`),
			},
			Usage: llm.TokenUsage{TotalTokens: 60000},
		},
		{Content: "this must never be returned — budget should trip first"},
	}}
	onStep, events := collectSteps()
	var hardBlockHits []observability.EngineHardBlockTrace

	// This test is about the iteration boundary, not evidence-based recovery.
	// Keep the tool call non-evidentiary so a successful read cannot legitimately
	// trigger the separate one-call synthesis path.
	eng := NewWithDeps(mock, &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("test upstream failure")
	}}, nil)
	eng.maxTokensPerTurn = 50000
	eng.SetHardBlockObserver(func(t observability.EngineHardBlockTrace) {
		hardBlockHits = append(hardBlockHits, t)
	})
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "4090什么配置", onStep)
	require.NoError(t, err)
	assert.Equal(t, tokenBudgetExceededMessage, reply,
		"budget exceeded should short-circuit to the canned reply, not the round-1 LLM response")

	// Exactly one LLM call: round 0 happens, round 1 hits the gate.
	assert.Len(t, mock.calls, 1,
		"second LLM call must not happen once budget is exceeded; got %d calls", len(mock.calls))

	// The tool_call from round 0 must have run to completion — the test
	// asserts BOTH the tool_call and tool_result events fired, proving
	// the pair stays atomic across the budget break.
	var sawToolCall, sawToolResult bool
	for _, ev := range *events {
		if ev.Type == StepToolCall && ev.Action == "ReadCapability_resource_info" {
			sawToolCall = true
		}
		if ev.Type == StepToolResult && ev.Action == "ReadCapability_resource_info" {
			sawToolResult = true
		}
	}
	assert.True(t, sawToolCall, "round 0 tool_call must be emitted before the budget break")
	assert.True(t, sawToolResult, "round 0 tool_result must be emitted before the budget break (protocol invariant)")

	// Hard-block observer fired with the expected category, so downstream
	// status mapping in trace_recorder produces status="blocked".
	require.Len(t, hardBlockHits, 1, "expected exactly one hard-block emission")
	assert.True(t, hardBlockHits[0].Hit)
	assert.Equal(t, "token_budget_exceeded", hardBlockHits[0].Category)
	// PR #61: single-source attribution — token budget is its own trigger class
	assert.Equal(t, observability.HardBlockTriggerTokenBudget, hardBlockHits[0].TriggeredBy)
}

// TestChat_TokenBudget_DisabledByDefault — sanity check that
// maxTokensPerTurn=0 means "no cap" (the production default). Without
// this, a refactor that flipped the sense of the comparison would
// silently start blocking every turn at 0 tokens.
func TestChat_TokenBudget_DisabledByDefault(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "done", Usage: llm.TokenUsage{TotalTokens: 999999}},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	// maxTokensPerTurn left at 0 (default)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "test"},
	}

	reply, err := eng.Chat(context.Background(), "hi", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "done", reply,
		"with maxTokensPerTurn=0 the LLM reply must pass through even with absurdly high usage")
}

func TestChat_TokenBudgetDoesNotDiscardCompletedAnswer(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{
		Content: "这是已经生成完成的答案。",
		Usage:   llm.TokenUsage{TotalTokens: 60000},
	}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) { hardBlocks = append(hardBlocks, trace) })

	reply, err := eng.Chat(context.Background(), "请解释一下", noopStep)

	require.NoError(t, err)
	assert.Equal(t, "这是已经生成完成的答案。", reply)
	assert.Len(t, mock.calls, 1)
	assert.Empty(t, hardBlocks, "an already-complete answer is not a blocked turn")
}
