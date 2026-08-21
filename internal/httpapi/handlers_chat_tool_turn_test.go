package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// toolTurnLLM is a two-round mock: round 1 emits a
// DescribeCompShareInstance tool call and round 2 emits a final text reply. It
// is shared by HTTP step and label tests.
type toolTurnLLM struct{ round int }

func (m *toolTurnLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.round++
	if m.round == 1 {
		return &llm.ChatResponse{
			ToolCalls: []openai.ToolCall{{
				ID:   "call-1",
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      "DescribeCompShareInstance",
					Arguments: `{"Limit":100}`,
				},
			}},
			Usage: llm.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}, nil
	}
	if req.OnTextDelta != nil {
		req.OnTextDelta("ok")
	}
	return &llm.ChatResponse{
		Content: "ok",
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

// toolTurnExecutor returns a single-host UHostSet for
// DescribeCompShareInstance.
type toolTurnExecutor struct{}

func (toolTurnExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	if action == "DescribeCompShareInstance" {
		return map[string]any{
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-e2e",
					"Name":    "e2e-host",
					"State":   "Running",
					"GPU":     1,
					"GpuType": "RTX4090",
					"CPU":     16,
					"Memory":  65536,
					"Zone":    "cn-wlcb-01",
				},
			},
		}, nil
	}
	return map[string]any{}, nil
}

// newChatTestHandlersWith is a variant of newChatTestHandlers that takes a
// pre-built engine + sessions so callers can install a custom LLM before
// construction. It keeps the rest of the handler wiring identical.
func newChatTestHandlersWith(t *testing.T, eng *engine.Engine, sessions *mockSessions) *Handlers {
	t.Helper()
	return NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)
}
