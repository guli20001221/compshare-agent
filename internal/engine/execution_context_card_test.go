package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullyPopulatedContext includes every kind of live execution context plus one
// deliberately non-live row. A future caller must not make an arbitrary semantic
// hint visible merely by populating this slice.
func fullyPopulatedContext() AgentContext {
	return AgentContext{
		CurrentQuestion: "关掉它",
		SelectedEntities: []SelectedEntityHint{
			{Kind: "instance", ID: "inst-LIVE", Name: "web-01", Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-SOLE", Name: "only-one", Source: selectionSourceAccountSingle, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-CAND", Name: "candidate-2", Ordinal: 2, Source: selectionSourcePendingCard, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-SEEN", Name: "just-read", Source: SelectedInstanceSourceObserved, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-RECALLED", Name: "not-live", Source: "agent_inference", Freshness: ContinuityFreshnessFresh},
		},
	}
}

// semanticBlockLabels must never appear in the execution-state card. The
// transcript is the model's semantic history.
var semanticBlockLabels = []string{
	"活动任务：", "已验证知识：", "近期可信观测：",
	"较早对话摘要：", "目标：", "既有约束：", "已作决定：", "未完成事项：", "较早完整摘录：",
}

func TestContextCardKeepsOnlyLiveExecutionState(t *testing.T) {
	card := renderAgentContextCard(fullyPopulatedContext())

	require.Contains(t, card, "【本轮执行上下文（仅用于目标指代）】")
	assert.NotContains(t, card, "不授权任何写操作")
	for _, id := range []string{"inst-LIVE", "inst-SOLE", "inst-CAND", "inst-SEEN"} {
		assert.Contains(t, card, id, "live execution state must survive")
	}
	assert.NotContains(t, card, "inst-RECALLED", "semantic hints never become a second memory")
	for _, block := range semanticBlockLabels {
		assert.NotContains(t, card, block, "%s is semantic memory, not execution state", block)
	}
}

func TestContextCardDoesNotShipABareHeader(t *testing.T) {
	assert.Empty(t, renderAgentContextCard(AgentContext{
		SelectedEntities: []SelectedEntityHint{{Kind: "instance", ID: "inst-RECALLED", Source: "agent_inference"}},
	}), "semantic-only input must not create an empty card")
}

func TestContextCompilerKeepsLiveBindingForTheWriteVerifier(t *testing.T) {
	e := selectionEngine()
	e.recordSelectedInstanceIDWithSource("inst-BBB", "web-02", SelectedInstanceSourceUser)
	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", time.Now())
	binding := e.bindInstanceTarget(view)

	require.Equal(t, "inst-BBB", binding.id)
	require.Contains(t, renderAgentContextCard(view), "inst-BBB",
		"the live pick remains model-visible and bindable after semantic state is removed")
}
