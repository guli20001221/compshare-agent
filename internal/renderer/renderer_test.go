package renderer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroundedRendererSendsOnlyEnvelopeAndNoTools(t *testing.T) {
	mock := &mockRendererLLM{response: "train-a 当前运行中。"}
	r := NewGroundedRenderer(mock)

	result := r.Render(context.Background(), RenderRequest{
		Envelope: testResourceEnvelope(),
		Fallback: "fallback",
		Model:    "deepseek-v4-flash",
	})

	require.False(t, result.FallbackUsed)
	assert.Equal(t, "train-a 当前运行中。", result.Text)
	assert.Equal(t, "deepseek-v4-flash", result.Model)
	assert.Equal(t, AttributionEnvelope, result.AttributionMode)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, result.EnvelopeHash)
	require.Len(t, mock.requests, 1)
	assert.Empty(t, mock.requests[0].Tools)
	assert.Empty(t, mock.requests[0].ToolChoice)
	require.Len(t, mock.requests[0].Messages, 2)
	wantEnvelope, err := json.Marshal(testResourceEnvelope())
	require.NoError(t, err)
	assert.JSONEq(t, string(wantEnvelope), mock.requests[0].Messages[1].Content)
	assert.NotContains(t, mock.requests[0].Messages[1].Content, "RawAPI")
}

func TestGroundedRendererFallsBackOnLLMError(t *testing.T) {
	r := NewGroundedRenderer(&mockRendererLLM{err: errors.New("llm down")})

	result := r.Render(context.Background(), RenderRequest{
		Envelope: testResourceEnvelope(),
		Fallback: "deterministic fallback",
		Model:    "deepseek-v4-flash",
	})

	assert.True(t, result.FallbackUsed)
	assert.Equal(t, FallbackLLMError, result.FallbackReason)
	assert.Equal(t, "deterministic fallback", result.Text)
}

func TestGroundedRendererFallsBackOnRateLimit(t *testing.T) {
	r := NewGroundedRenderer(&mockRendererLLM{err: governance.ErrRateLimited})

	result := r.Render(context.Background(), RenderRequest{
		Envelope: testResourceEnvelope(),
		Fallback: "deterministic fallback",
		Model:    "deepseek-v4-flash",
	})

	assert.True(t, result.FallbackUsed)
	assert.Equal(t, FallbackRateLimited, result.FallbackReason)
	assert.Equal(t, "deterministic fallback", result.Text)
}

func TestGroundedRendererFallsBackOnValidationFailure(t *testing.T) {
	r := NewGroundedRenderer(&mockRendererLLM{response: "uhost-invented 正在运行。"})

	result := r.Render(context.Background(), RenderRequest{
		Envelope: testResourceEnvelope(),
		Fallback: "fallback",
		Model:    "deepseek-v4-flash",
	})

	assert.True(t, result.FallbackUsed)
	assert.Equal(t, FallbackValidationFailed, result.FallbackReason)
	assert.Equal(t, "fallback", result.Text)
}

func TestGroundedRendererFallsBackOnUnknownInstanceName(t *testing.T) {
	r := NewGroundedRenderer(&mockRendererLLM{response: "prod-db-01 正在运行。"})

	result := r.Render(context.Background(), RenderRequest{
		Envelope: testResourceEnvelope(),
		Fallback: "fallback",
		Model:    "deepseek-v4-flash",
	})

	assert.True(t, result.FallbackUsed)
	assert.Equal(t, FallbackValidationFailed, result.FallbackReason)
}

func TestGroundedRendererFallsBackOnEmptyOutput(t *testing.T) {
	r := NewGroundedRenderer(&mockRendererLLM{response: "  "})

	result := r.Render(context.Background(), RenderRequest{
		Envelope: testResourceEnvelope(),
		Fallback: "fallback",
		Model:    "deepseek-v4-flash",
	})

	assert.True(t, result.FallbackUsed)
	assert.Equal(t, FallbackValidationFailed, result.FallbackReason)
	assert.Equal(t, "fallback", result.Text)
}

func TestGroundedRendererPromptIncludesNoInventInstruction(t *testing.T) {
	mock := &mockRendererLLM{response: "train-a 当前运行中。"}
	r := NewGroundedRenderer(mock)

	r.Render(context.Background(), RenderRequest{Envelope: testResourceEnvelope(), Fallback: "fallback"})

	require.Len(t, mock.requests, 1)
	assert.Contains(t, mock.requests[0].Messages[0].Content, "禁止编造")
	assert.Contains(t, mock.requests[0].Messages[0].Content, "envelope")
}

func TestGroundedRendererPromptIncludesResourceListRules(t *testing.T) {
	mock := &mockRendererLLM{response: "train-a"}
	r := NewGroundedRenderer(mock)

	r.Render(context.Background(), RenderRequest{Envelope: testResourceEnvelope(), Fallback: "fallback"})

	require.Len(t, mock.requests, 1)
	prompt := mock.requests[0].Messages[0].Content
	assert.Contains(t, prompt, "resource_info")
	assert.Contains(t, prompt, "ALL subjects")
	assert.Contains(t, prompt, "computed.total_count")
	assert.Contains(t, prompt, "duplicate names")
	assert.Contains(t, prompt, "Do not rank")
}

func TestGroundedRendererPromptIncludesTroubleshootingRules(t *testing.T) {
	mock := &mockRendererLLM{response: "train-a"}
	r := NewGroundedRenderer(mock)

	r.Render(context.Background(), RenderRequest{Envelope: testResourceEnvelope(), Fallback: "fallback"})

	require.Len(t, mock.requests, 1)
	prompt := mock.requests[0].Messages[0].Content
	assert.Contains(t, prompt, "computed.answer_mode")
	assert.Contains(t, prompt, "troubleshooting")
	assert.Contains(t, prompt, "threshold/baseline")
	assert.Contains(t, prompt, "instance-internal root cause")
}

type mockRendererLLM struct {
	response string
	err      error
	requests []llm.ChatRequest
}

func (m *mockRendererLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.requests = append(m.requests, req)
	if m.err != nil {
		return nil, m.err
	}
	return &llm.ChatResponse{Content: m.response}, nil
}

func testResourceEnvelope() envelope.Envelope {
	return envelope.Envelope{
		Kind:          envelope.KindResourceInfo,
		SourceActions: []string{"DescribeCompShareInstance"},
		Subjects: []envelope.Subject{{
			ID:   "uhost-a",
			Name: "train-a",
			Type: envelope.SubjectInstance,
		}},
		Facts: []envelope.Fact{{
			SubjectID: "uhost-a",
			Key:       "state",
			Label:     "状态",
			Value:     "Running",
			Source:    envelope.FactSourceAPI,
		}},
		Constraints: envelope.Constraints{
			DoNotInventInstances:   true,
			DoNotAnswerAccountBill: true,
		},
	}
}

func testMultiResourceEnvelope() envelope.Envelope {
	env := testResourceEnvelope()
	env.Subjects = []envelope.Subject{
		{ID: "uhost-a", Name: "train-a", Type: envelope.SubjectInstance},
		{ID: "uhost-b", Name: "train-b", Type: envelope.SubjectInstance},
	}
	env.Facts = []envelope.Fact{
		{SubjectID: "uhost-a", Key: "state", Label: "状态", Value: "Running", Source: envelope.FactSourceAPI},
		{SubjectID: "uhost-b", Key: "state", Label: "状态", Value: "Running", Source: envelope.FactSourceAPI},
	}
	env.Computed = []envelope.Fact{{
		Key:    "total_count",
		Label:  "Total count",
		Value:  "2",
		Source: envelope.FactSourceComputed,
	}}
	return env
}
