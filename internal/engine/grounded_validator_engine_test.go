package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatResetsSearchKnowledgeLedgerEachTurn proves the per-turn ChunkID ledger is
// zeroed at the top of every turn, so one turn's accumulated evidence cannot leak
// into the next (the cross-tenant/cross-turn concern engine_session_test.go guards).
func TestChatResetsSearchKnowledgeLedgerEachTurn(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "a", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "b", "State": "Running"},
		},
	}, "test"))
	// Simulate a prior turn that accumulated agentic evidence into the per-turn ledger.
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "stale-1"}}}
	_, err := eng.Chat(context.Background(), "我的机器有问题", noopStep)
	require.NoError(t, err)
	assert.Empty(t, eng.searchKnowledgeLedgerThisTurn.Items, "per-turn agentic ledger must be reset at the top of each turn")
}
