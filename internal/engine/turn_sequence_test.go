package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type turnSequenceLLM struct{}

func (turnSequenceLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "ok"}, nil
}

func TestSessionOptions_InitialCommittedTurnsKeepsFreshEngineOnAuthoritativeSequence(t *testing.T) {
	deps := &SharedDeps{LLMClient: turnSequenceLLM{}}
	warm := NewSession(deps, SessionOptions{})
	_, err := warm.Chat(context.Background(), "first", nil)
	require.NoError(t, err)
	_, err = warm.Chat(context.Background(), "second", nil)
	require.NoError(t, err)

	fresh := NewSession(deps, SessionOptions{InitialCommittedTurns: 1})
	_, err = fresh.Chat(context.Background(), "second", nil)
	require.NoError(t, err)

	assert.Equal(t, 2, warm.userTurn)
	assert.Equal(t, warm.userTurn, fresh.userTurn,
		"a rebuilt second turn must not reset turn-relative state to turn one")
}
