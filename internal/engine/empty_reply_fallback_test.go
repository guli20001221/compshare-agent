package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChat_EmptyLLMContent_ReturnsHonestFallback is the P0 empty-reply guard.
// When a turn finishes with no error, no tool call, and empty content (flash
// intermittently returns empty content), the user must get a non-empty honest
// message instead of a blank "Assistant>" reply ("空回复"). This is the
// load-bearing production fix: a successful turn must never surface as empty.
func TestChat_EmptyLLMContent_ReturnsHonestFallback(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: ""}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)
	assert.Equal(t, emptyReplyFallbackMessage, reply,
		"an empty successful turn must surface the honest fallback, never a blank reply")
}

// TestChat_NonEmptyLLMContent_Unchanged guards that the fallback fires ONLY on
// empty content — a normal answer must pass through untouched, so the guard
// can never mask or replace a real reply.
func TestChat_NonEmptyLLMContent_Unchanged(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "实例运行正常。"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "实例运行正常。", reply,
		"a non-empty reply must pass through unchanged — the fallback must not fire")
}

// TestChat_WhitespaceOnlyLLMContent_ReturnsFallback covers the case where the
// model returns only whitespace — still a blank reply to the user, so the
// honest fallback must fire (TrimSpace guard).
func TestChat_WhitespaceOnlyLLMContent_ReturnsFallback(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "  \n\t "}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)
	assert.Equal(t, emptyReplyFallbackMessage, reply,
		"a whitespace-only reply is still blank to the user — the fallback must fire")
}
