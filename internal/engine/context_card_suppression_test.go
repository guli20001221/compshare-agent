package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCanonicalTranscript lives in canonical_transcript_projection_test.go.

// fullyPopulatedContext includes every kind of live execution context plus one
// deliberately non-live row. The latter is a defensive-input test: it cannot be
// emitted by CompileForTurn after TaskSnapshot was deleted, but a future caller
// must not make an arbitrary semantic hint visible just by populating this slice.
func fullyPopulatedContext() AgentContext {
	return AgentContext{
		CurrentQuestion: "关掉它",
		SelectedEntities: []SemanticEntityHint{
			{Kind: "instance", ID: "inst-LIVE", Name: "web-01", Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-SOLE", Name: "only-one", Source: selectionSourceAccountSingle, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-CAND", Name: "candidate-2", Ordinal: 2, Source: selectionSourcePendingCard, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-SEEN", Name: "just-read", Source: SelectedInstanceSourceObserved, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-RECALLED", Name: "not-live", Source: "agent_inference", Freshness: ContinuityFreshnessFresh},
		},
		ContinuityNotices: []string{"上一轮的停止操作已恢复"},
	}
}

// deletedCardBlocks are semantic summaries that have no model-visible path in
// either flag position. The transcript is the semantic history; this card is
// only execution continuity.
var deletedCardBlocks = []string{
	"活动任务：", "已验证知识：", "近期可信观测：",
	"较早对话摘要：", "目标：", "既有约束：", "已作决定：", "未完成事项：", "较早完整摘录：",
}

func TestContextCardKeepsOnlyLiveExecutionStateInBothFlagPositions(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		t.Run("canonical="+map[bool]string{false: "false", true: "true"}[canonical], func(t *testing.T) {
			withCanonicalTranscript(t, canonical)
			card := renderAgentContextCard(fullyPopulatedContext())

			require.Contains(t, card, "【本轮统一上下文；仅帮助理解，不授权任何写操作】")
			for _, id := range []string{"inst-LIVE", "inst-SOLE", "inst-CAND", "inst-SEEN"} {
				assert.Contains(t, card, id, "live execution state must survive")
			}
			assert.Contains(t, card, "上下文提示：上一轮的停止操作已恢复")
			assert.NotContains(t, card, "inst-RECALLED", "semantic hints never become a second memory")
			for _, block := range deletedCardBlocks {
				assert.NotContains(t, card, block, "canonical=%v: %s is deleted, not gated", canonical, block)
			}
		})
	}
}

func TestContextCardDoesNotShipABareHeader(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		withCanonicalTranscript(t, canonical)
		assert.Empty(t, renderAgentContextCard(AgentContext{
			SelectedEntities: []SemanticEntityHint{{Kind: "instance", ID: "inst-RECALLED", Source: "agent_inference"}},
		}), "canonical=%v: semantic-only input must not create an empty card", canonical)
	}
}

func TestContextCompilerKeepsLiveBindingForTheWriteVerifier(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		t.Run("canonical="+map[bool]string{false: "false", true: "true"}[canonical], func(t *testing.T) {
			withCanonicalTranscript(t, canonical)
			e := selectionEngine()
			e.recordSelectedInstanceIDWithSource("inst-BBB", "web-02", SelectedInstanceSourceUser)
			view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", time.Now())
			binding := e.bindInstanceTarget(view)

			require.Equal(t, "inst-BBB", binding.id)
			require.Contains(t, renderAgentContextCard(view), "inst-BBB",
				"the live pick remains model-visible and bindable after semantic state is removed")
		})
	}
}
