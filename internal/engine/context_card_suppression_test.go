package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// With the canonical transcript on it IS the semantic history, so the context card
// restating a summary of the same turns is a second, lossier memory. Suppressing it
// is the first step of replacing the semantic layer rather than running two.
//
// The danger is not under-suppressing, it is over-suppressing: SelectedEntities is
// assembled from five sources in CompileForTurn and three of them are live execution
// state, including the binding the write path proves its target with.

// withCanonicalTranscript lives in canonical_transcript_projection_test.go.

// fullyPopulatedContext carries one row of every block the card can render, so a
// block that stops being suppressed (or starts being suppressed) shows up as a
// named assertion rather than as a length change.
func fullyPopulatedContext() AgentContext {
	return AgentContext{
		CurrentQuestion: "关掉它",
		ActiveTask: &TaskSnapshot{
			Goal: "创建一台推理机器", Stage: "选规格", MissingSlots: []string{"GPU 型号"},
			Freshness: ContinuityFreshnessFresh,
		},
		ConversationDigest: ConversationDigest{
			Narrative:       "用户先问了价格，再问库存",
			Goals:           []string{"跑 SDXL"},
			Constraints:     []string{"预算每小时 5 元以内"},
			Decisions:       []string{"选用按量计费"},
			UnresolvedTasks: []string{"还没决定可用区"},
			Excerpts:        []ConversationExcerpt{{User: "有 4090 吗", Assistant: "有"}},
		},
		SelectedEntities: []SemanticEntityHint{
			// Live execution state — must survive suppression.
			{Kind: "instance", ID: "inst-LIVE", Name: "web-01",
				Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-SOLE", Name: "only-one",
				Source: selectionSourceAccountSingle, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-CAND", Name: "candidate-2", Ordinal: 2,
				Source: selectionSourcePendingCard, Freshness: ContinuityFreshnessFresh},
			{Kind: "instance", ID: "inst-SEEN", Name: "just-read",
				Source: SelectedInstanceSourceObserved, Freshness: ContinuityFreshnessFresh},
			// Semantic memory — a TaskSnapshot / digest entity, carrying an
			// actionresolver CandidateSource rather than a selection source.
			{Kind: "instance", ID: "inst-RECALLED", Name: "from-a-summary",
				Source: "agent_inference", Freshness: ContinuityFreshnessFresh},
		},
		ContinuityNotices:  []string{"上一轮的停止操作已恢复"},
	}
}

func entityIDs(hints []SemanticEntityHint) []string {
	out := make([]string, 0, len(hints))
	for _, h := range hints {
		out = append(out, h.ID)
	}
	return out
}

// semanticCardBlocks are the labels the transcript replaces. Listed by label rather
// than by count so that adding a block to the card without deciding which side of
// this line it falls on is a visible omission here, not a silent pass.
// 已验证知识 and 近期可信观测 are deliberately absent: they are no longer
// suppressed, they are DELETED.
// The card projection and AgentContext.VerifiedKnowledge were removed once the
// transcript carried the same turns verbatim, so no flag position brings the
// block back. That is the step, and this list is where it had to be declared.
// What survives is the verifier's evidence ledger, which the model never reads:
// see TestVerifierStillSeesPriorEvidenceAfterTheInjectionIsGone.
var semanticCardBlocks = []string{
	"活动任务：", "较早对话摘要：", "目标：", "既有约束：", "已作决定：",
	"未完成事项：", "较早完整摘录：",
}

func TestWithTheTranscriptOffTheCardStillCarriesEverySemanticBlock(t *testing.T) {
	withCanonicalTranscript(t, false)
	card := renderAgentContextCard(fullyPopulatedContext())

	for _, block := range semanticCardBlocks {
		assert.Contains(t, card, block,
			"flag off must be the pre-change card; suppression leaked into the default path")
	}
	assert.Contains(t, card, "inst-RECALLED",
		"a semantic entity is part of the flag-off card and must not be filtered there")
	assert.Contains(t, card, "inst-LIVE")
	assert.Contains(t, card, "上下文提示：上一轮的停止操作已恢复")
}

func TestWithTheTranscriptOnTheCardKeepsOnlyLiveExecutionState(t *testing.T) {
	withCanonicalTranscript(t, true)
	card := renderAgentContextCard(fullyPopulatedContext())

	for _, block := range semanticCardBlocks {
		assert.NotContains(t, card, block,
			"the transcript already carries this verbatim; the card restating it is the second memory")
	}
	assert.NotContains(t, card, "inst-RECALLED",
		"an entity recalled from a summary is semantic memory even though it rides in SelectedEntities")

	// Everything a transcript cannot carry stays.
	assert.Contains(t, card, "【本轮统一上下文；仅帮助理解，不授权任何写操作】",
		"the no-write header is a permission notice, not a memory")
	assert.Contains(t, card, "inst-LIVE", "the current user pick is the live selection")
	assert.Contains(t, card, "inst-SOLE", "the account's sole instance is live registry state")
	assert.Contains(t, card, "inst-CAND", "the pending selection card's 第2台 must stay addressable")
	assert.Contains(t, card, "inst-SEEN", "an observed referent is this session's live state, not a summary")
	assert.Contains(t, card, "上下文提示：上一轮的停止操作已恢复",
		"continuity notices are execution state: what this turn recovered and what it may not do")
}

func TestSuppressionEmptiesTheCardRatherThanShippingABareHeader(t *testing.T) {
	withCanonicalTranscript(t, true)
	semanticOnly := AgentContext{
		ConversationDigest: ConversationDigest{Narrative: "只有摘要"},
		SelectedEntities: []SemanticEntityHint{
			{Kind: "instance", ID: "inst-RECALLED", Source: "agent_inference"},
		},
	}
	assert.Empty(t, renderAgentContextCard(semanticOnly),
		"with nothing live left there is no card, not a header announcing an empty one")
}

// The control, and the reason the suppression lives in renderAgentContextCard
// rather than in CompileForTurn.
//
// selection_binder and the write-proposal path read view.SelectedEntities as a
// struct; they never see the rendered string. Filtering upstream would narrow
// 执行侧目标绑定 — the write path's proof of which instance the user meant — while
// the two card tests above still passed. This goes through CompileForTurn on a real
// engine for exactly that reason: a filter moved into the compiler fails HERE.
func TestSuppressingTheCardDoesNotChangeWhatTheWriteVerifierSees(t *testing.T) {
	compile := func(enabled bool) (AgentContext, selectionBinding) {
		withCanonicalTranscript(t, enabled)
		now := time.Now()
		e := selectionEngine()
		e.recordSelectedInstanceIDWithSource("inst-BBB", "web-02", SelectedInstanceSourceUser)
		// A SEMANTIC entity must be in the compiled view or this test is an empty
		// gate: with only the live pick present, a filter moved into CompileForTurn
		// removes nothing and the comparison below passes for the wrong reason.
		// Verified by mutation — without this the upstream-filter mutation survived.
		e.sessionState.TaskSnapshot = TaskSnapshot{
			Goal:   "创建一台推理机器",
			Status: TaskSnapshotStatusActive,
			Entities: []SemanticEntityHint{{
				Kind: "instance", ID: "inst-RECALLED", Name: "from-a-summary",
				Source: "agent_inference", Freshness: ContinuityFreshnessFresh,
			}},
		}
		e.refreshConversationDigest(now)
		view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
		return view, e.bindInstanceTarget(view)
	}

	offView, offBinding := compile(false)
	onView, onBinding := compile(true)

	require.Equal(t, "inst-BBB", offBinding.id, "premise: the pick binds with the flag off")
	require.Contains(t, entityIDs(offView.SelectedEntities), "inst-RECALLED",
		"premise: the compiled view carries a semantic entity for an upstream filter to remove")
	assert.Equal(t, offBinding, onBinding,
		"suppressing the CARD must not change which instance a write would target")
	assert.Equal(t, offView.SelectedEntities, onView.SelectedEntities,
		"the compiled view is execution input; only its rendering is suppressed")

	// And the rendering really did change, or the two assertions above are vacuous.
	withCanonicalTranscript(t, false)
	offCard := renderAgentContextCard(fullyPopulatedContext())
	withCanonicalTranscript(t, true)
	onCard := renderAgentContextCard(fullyPopulatedContext())
	require.NotEqual(t, offCard, onCard, "premise: the flag changes the card")
	require.Less(t, len(strings.Split(onCard, "\n")), len(strings.Split(offCard, "\n")))
}
