package agentpool_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type committedTailStore struct {
	*mockMessageStore
	tailCalls int
	tail      []store.Message
}

func (s *committedTailStore) ListCommittedTail(
	_ context.Context,
	_ store.Owner,
	_ string,
	turnLimit int,
) ([]store.Message, error) {
	s.tailCalls++
	if turnLimit <= 0 {
		panic("NewTurnEngine requested an unbounded committed history")
	}
	return append([]store.Message(nil), s.tail...), nil
}

func TestNewTurnEngine_IsPrivateAndUsesOnlyCommittedTail(t *testing.T) {
	ms := &committedTailStore{
		mockMessageStore: &mockMessageStore{},
		tail: []store.Message{
			{Role: "user", Content: "已提交问题", Status: "ok"},
			{Role: "assistant", Content: "已提交回答", Status: "ok"},
		},
	}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{
		Capacity: 1,
		IdleTTL:  5 * time.Minute,
	})
	defer pool.Close()

	first, err := pool.NewTurnEngine(context.Background(), owner1, "sess-private")
	require.NoError(t, err)
	second, err := pool.NewTurnEngine(context.Background(), owner1, "sess-private")
	require.NoError(t, err)

	require.NotSame(t, first, second, "every turn needs a private mutable engine")
	require.Same(t, first.LLMClientPointer(), second.LLMClientPointer(), "process-wide dependencies stay shared")
	assert.Equal(t, 2, ms.tailCalls)
	assert.Zero(t, ms.listCalls, "v2 must not fall back to the legacy head-page reader")
	assert.Zero(t, pool.SizeForTest(), "turn engines must never enter the shared LRU")
	assert.Equal(t, first.MessagesSnapshot(), second.MessagesSnapshot())

	history := first.MessagesSnapshot()
	require.Len(t, history, 3) // system + one complete committed pair
	assert.Equal(t, "user", history[1].Role)
	assert.Equal(t, "已提交问题", history[1].Content)
	assert.Equal(t, "assistant", history[2].Role)
	assert.Equal(t, "已提交回答", history[2].Content)
}

func TestNewTurnEngine_MissingCommittedTailCapabilityFailsLoud(t *testing.T) {
	legacyOnly := &mockMessageStore{messages: []store.Message{
		{Role: "user", Content: "legacy head row", Status: "ok"},
	}}
	pool := agentpool.New(minimalConfig(), legacyOnly, agentpool.Options{})
	defer pool.Close()

	eng, err := pool.NewTurnEngine(context.Background(), owner1, "sess-no-tail")
	require.Error(t, err)
	assert.Nil(t, eng)
	assert.True(t, strings.Contains(err.Error(), "committed tail"), err.Error())
	assert.Zero(t, legacyOnly.listCalls, "failure must not silently read the oldest legacy page")
}

func TestNewTurnEngine_RejectsHalfCommittedTail(t *testing.T) {
	ms := &committedTailStore{
		mockMessageStore: &mockMessageStore{},
		tail: []store.Message{
			{Role: "user", Content: "orphan user", Status: "ok"},
		},
	}
	pool := agentpool.New(minimalConfig(), ms, agentpool.Options{})
	defer pool.Close()

	eng, err := pool.NewTurnEngine(context.Background(), owner1, "sess-half")
	require.Error(t, err)
	assert.Nil(t, eng)
	assert.Contains(t, err.Error(), "invalid committed tail")
}

func TestNewTurnEngine_RejectsEmptyCommittedMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail []store.Message
	}{
		{
			name: "empty user",
			tail: []store.Message{
				{Role: "user", Content: "  \n", Status: "ok"},
				{Role: "assistant", Content: "answer", Status: "ok"},
			},
		},
		{
			name: "empty assistant",
			tail: []store.Message{
				{Role: "user", Content: "question", Status: "ok"},
				{Role: "assistant", Content: "\t", Status: "ok"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &committedTailStore{mockMessageStore: &mockMessageStore{}, tail: tc.tail}
			pool := agentpool.New(minimalConfig(), ms, agentpool.Options{})
			defer pool.Close()

			eng, err := pool.NewTurnEngine(context.Background(), owner1, "sess-empty")
			require.Error(t, err)
			assert.Nil(t, eng)
			assert.Contains(t, err.Error(), "invalid committed tail")
		})
	}
}
