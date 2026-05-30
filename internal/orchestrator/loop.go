package orchestrator

import (
	"context"
	"errors"

	"github.com/compshare-agent/internal/llm"

	openai "github.com/sashabaranov/go-openai"
)

// AgentLoop is the thin agent-tier reasoning shell (ADR-006 §决策5 + ADR-007:
// deliberately minimal). The real multi-round LLM-in-the-loop reasoning is B8's
// deploy_model skill body, NOT framework here — this just holds the strong
// (TierAgent) client and exposes a single-shot call so B8 has a wiring seam.
//
// This is the one place B6.2 binds the strong model: NewAgentLoop calls
// router.For(llm.TierAgent) (ADR-006 §决策5 acceptance: grep r.For(TierAgent)
// must be ≥ 1). Per-call LLM timeout is the ADR-002 agent-tier timeout (180s),
// which stays strictly below the per-step DefaultStepTimeout (240s) — see the
// compile-time invariant in step.go.
type AgentLoop struct {
	client *llm.Client
}

// NewAgentLoop builds the loop with the strong-tier client from the router.
func NewAgentLoop(router *llm.Router) *AgentLoop {
	return &AgentLoop{client: router.For(llm.TierAgent)}
}

// Reason makes one strong-model call and returns the assistant text. B8 builds
// the real tool-using, multi-round skill reasoning on top of this seam.
func (l *AgentLoop) Reason(ctx context.Context, messages []openai.ChatCompletionMessage) (string, error) {
	if l == nil || l.client == nil {
		return "", errors.New("orchestrator: agent loop has no LLM client")
	}
	resp, err := l.client.Chat(ctx, llm.ChatRequest{Messages: messages})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
