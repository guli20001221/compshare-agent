package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedClient returns canned replies in order and records the messages of each
// call so tests can assert the working-set fed back to the model.
type scriptedClient struct {
	replies  []string
	calls    int
	lastMsgs []openai.ChatCompletionMessage
	usageN   int
}

func (c *scriptedClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.lastMsgs = req.Messages
	i := c.calls
	c.calls++
	reply := `{"final":"(script exhausted)"}`
	if i < len(c.replies) {
		reply = c.replies[i]
	}
	return &llm.ChatResponse{Content: reply, Usage: llm.TokenUsage{TotalTokens: 7}}, nil
}

// scriptedExec records calls and returns canned results keyed by action.
type scriptedExec struct {
	results map[string]map[string]any
	calls   []string
}

func (e *scriptedExec) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, action)
	if r, ok := e.results[action]; ok {
		return r, nil
	}
	return map[string]any{"action": action}, nil
}

func toolDef(name string) openai.Tool {
	return openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: name, Description: name + " 说明"}}
}

func readOnlyTools() []openai.Tool {
	return []openai.Tool{toolDef("DescribeCompShareInstance"), toolDef("DescribeCompShareSoftwarePort")}
}

// Happy path: the model calls one tool, the executor feeds the result BACK into
// the working-set, and the model's next turn (which sees that result) finalizes.
// This is the (A) step-feedback keystone the saga has no infrastructure for.
func TestRunReadOnlySkill_FeedsToolResultBackToModel(t *testing.T) {
	client := &scriptedClient{replies: []string{
		`{"action":"DescribeCompShareInstance","args":{"UHostIds":["u1"]}}`,
		`{"final":"实例运行中，JupyterLab 端口 8888"}`,
	}}
	exec := &scriptedExec{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"State": "Running", "Name": "gpu-a"}}},
	}}
	var usage int

	reply, err := RunReadOnlySkill(context.Background(), "JupyterLab 打不开", map[string]any{"UHostId": "u1"}, SkillExecOptions{
		Body:    "诊断正文：先查实例状态。",
		Tools:   readOnlyTools(),
		Exec:    exec,
		Client:  client,
		OnUsage: func(llm.TokenUsage) { usage++ },
	})

	require.NoError(t, err)
	assert.Equal(t, "实例运行中，JupyterLab 端口 8888", reply)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, exec.calls, "only the one tool the model chose")
	assert.Equal(t, 2, usage, "OnUsage fires once per LLM call")

	// The SECOND LLM call must have seen the first tool's result in its messages —
	// this is the working-set feedback. Without it the loop is a fixed pipeline.
	var sawResult bool
	for _, m := range client.lastMsgs {
		if strings.Contains(m.Content, "Running") {
			sawResult = true
		}
	}
	assert.True(t, sawResult, "tool result must be fed back into the model's working-set")

	// The private working-set must NOT be the seed alone: system + user-seed +
	// assistant(tool-call) + user(tool-result) = 4 messages by the final call.
	assert.GreaterOrEqual(t, len(client.lastMsgs), 4)
}

// Lead rule (2026-06-01): a malformed step gets ONE corrective retry, then recovers.
func TestRunReadOnlySkill_MalformedOnceRecovers(t *testing.T) {
	client := &scriptedClient{replies: []string{
		"这不是 JSON，我先想想……",
		`{"final":"已诊断完成"}`,
	}}
	reply, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{
		Body: "body", Tools: readOnlyTools(), Exec: &scriptedExec{}, Client: client,
	})
	require.NoError(t, err)
	assert.Equal(t, "已诊断完成", reply)
	assert.Equal(t, 2, client.calls, "one corrective retry consumed, then the final")
}

// Lead rule: a SECOND malformed step safe-fails (ErrSkillExecUnrecovered), no mutation.
func TestRunReadOnlySkill_MalformedTwiceSafeFails(t *testing.T) {
	client := &scriptedClient{replies: []string{"garbage one", "garbage two", "garbage three"}}
	exec := &scriptedExec{}
	reply, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{
		Body: "body", Tools: readOnlyTools(), Exec: exec, Client: client,
	})
	require.ErrorIs(t, err, ErrSkillExecUnrecovered)
	assert.Empty(t, reply)
	assert.Empty(t, exec.calls, "no tool ever runs on a malformed loop")
	assert.Equal(t, 2, client.calls, "second malformed safe-fails immediately")
}

// An unavailable tool is treated like a malformed step: one corrective retry,
// then recover if the model complies.
func TestRunReadOnlySkill_UnavailableToolRecovers(t *testing.T) {
	client := &scriptedClient{replies: []string{
		`{"action":"TerminateCompShareInstance","args":{}}`, // not in the read-only set
		`{"final":"好的，只读排查完成"}`,
	}}
	exec := &scriptedExec{}
	reply, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{
		Body: "body", Tools: readOnlyTools(), Exec: exec, Client: client,
	})
	require.NoError(t, err)
	assert.Equal(t, "好的，只读排查完成", reply)
	assert.Empty(t, exec.calls, "the unavailable (mutating) tool must never execute")
}

// An unavailable tool twice safe-fails — a read-only executor never runs a tool
// outside its declared set, so a model insisting on one cannot escalate.
func TestRunReadOnlySkill_UnavailableToolTwiceSafeFails(t *testing.T) {
	client := &scriptedClient{replies: []string{
		`{"action":"TerminateCompShareInstance","args":{}}`,
		`{"action":"TerminateCompShareInstance","args":{}}`,
	}}
	exec := &scriptedExec{}
	_, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{
		Body: "body", Tools: readOnlyTools(), Exec: exec, Client: client,
	})
	require.ErrorIs(t, err, ErrSkillExecUnrecovered)
	assert.Empty(t, exec.calls)
}

// A model that never finalizes is bounded by MaxRounds and safe-fails.
func TestRunReadOnlySkill_MaxRoundsExhaustedSafeFails(t *testing.T) {
	client := &scriptedClient{replies: []string{
		`{"action":"DescribeCompShareInstance","args":{}}`,
		`{"action":"DescribeCompShareInstance","args":{}}`,
		`{"action":"DescribeCompShareInstance","args":{}}`,
	}}
	exec := &scriptedExec{}
	_, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{
		Body: "body", Tools: readOnlyTools(), Exec: exec, Client: client, MaxRounds: 2,
	})
	require.ErrorIs(t, err, ErrSkillExecUnrecovered)
	assert.Len(t, exec.calls, 2, "exactly MaxRounds tool calls, then safe-fail")
}

func TestRunReadOnlySkill_NoClientSafeFails(t *testing.T) {
	_, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{Body: "b", Tools: readOnlyTools()})
	require.ErrorIs(t, err, ErrSkillExecUnrecovered)
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"final":"ok"}`, `{"final":"ok"}`},
		{"prose before {\"a\":1} prose after", `{"a":1}`},
		{`{"a":{"b":2},"c":3}`, `{"a":{"b":2},"c":3}`},
		{`{"s":"has } brace in string"}`, `{"s":"has } brace in string"}`},
		{"no object here", ""},
		{`{"s":"escaped \" quote"}`, `{"s":"escaped \" quote"}`},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, extractJSONObject(c.in), c.in)
	}
}

func TestParseSkillStep(t *testing.T) {
	final, err := parseSkillStep(`{"final":"done"}`)
	require.NoError(t, err)
	assert.Equal(t, "done", final.Final)

	tool, err := parseSkillStep(`{"action":"X","args":{"k":"v"}}`)
	require.NoError(t, err)
	assert.Equal(t, "X", tool.Action)
	assert.Equal(t, "v", tool.Args["k"])

	_, err = parseSkillStep(`{"unrelated":true}`)
	assert.Error(t, err, "neither action nor final is malformed")

	_, err = parseSkillStep(`not json`)
	assert.Error(t, err)
}

// Guard: the sentinel error must remain wrappable so callers can distinguish a
// safe-fail (render friendly reply) from an unexpected panic.
func TestErrSkillExecUnrecovered_IsWrappable(t *testing.T) {
	wrapped := errors.New("x")
	_ = wrapped
	_, err := RunReadOnlySkill(context.Background(), "q", nil, SkillExecOptions{Tools: readOnlyTools()})
	assert.True(t, errors.Is(err, ErrSkillExecUnrecovered))
}
