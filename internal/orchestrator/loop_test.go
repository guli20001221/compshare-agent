package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/llm"
)

// TestAgentLoop_WiresTierAgent proves the strong-model binding (ADR-006 §决策5:
// the saga path's LLM client comes from router.For(TierAgent), NOT TierFast).
// Distinct per-tier models + pointer identity make this falsifiable: each tier
// gets its own *Client allocation, so assert.Same fails if NewAgentLoop bound
// the wrong tier. NewRouter does no network at construction.
func TestAgentLoop_WiresTierAgent(t *testing.T) {
	router, err := llm.NewRouter(
		config.LLMConfig{Model: "fast-model"},
		map[llm.Tier]config.LLMConfig{llm.TierAgent: {Model: "agent-model"}},
	)
	require.NoError(t, err)
	loop := NewAgentLoop(router)
	require.NotNil(t, loop)
	// decisive: the bound client must be the TierAgent client, not TierFast
	assert.Same(t, router.For(llm.TierAgent), loop.client)
	assert.NotSame(t, router.For(llm.TierFast), loop.client)
	assert.Equal(t, "agent-model", router.Model(llm.TierAgent))
}

func TestAgentLoop_NilReceiverErrors(t *testing.T) {
	var loop *AgentLoop
	_, err := loop.Reason(context.Background(), nil)
	require.Error(t, err)

	empty := &AgentLoop{}
	_, err = empty.Reason(context.Background(), nil)
	require.Error(t, err)
}
