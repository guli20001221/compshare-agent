package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/knowledge"
)

// VerifiedKnowledge had TWO consumers, and only one of them was semantic memory.
//
//   - The context card injected it into the model as 已验证知识：问=…；答=… . That is
//     the semantic layer the canonical transcript replaces, and it is deleted.
//   - knowledgeLedgerForVerification feeds it to the answer-grounding verifier.
//     The MODEL NEVER READS IT. It is the evidence a later answer is checked
//     against, in the same role as this turn's retrieval and tool results.
//
// Deleting both together would have been the easy reading of "delete the
// semantic injection", and it would have broken grounding exactly where it is
// load-bearing: a follow-up turn that retrieves nothing of its own has an empty
// current-turn ledger, so prior verified evidence is the ONLY thing the verifier
// can check against. That shape is not hypothetical — replaying 30 real
// production sessions through the current stack, the share of turns issuing any
// knowledge retrieval falls from 31.7% at turn 1 to 4.8% past turn 6.
func TestVerifierStillSeesPriorEvidenceAfterTheInjectionIsGone(t *testing.T) {
	prior := knowledge.EvidenceLedger{
		Query: "4090 一小时多少钱",
		Items: []knowledge.EvidenceItem{{
			ChunkID: "w0-pricing-4090",
			Title:   "计费概览",
			Snippet: "RTX 4090 按量计费为每小时 2.00 元。",
		}},
	}
	eng := &Engine{}
	eng.rememberVerifiedKnowledge("4090 一小时多少钱", "每小时 2 元。", prior)
	require.Len(t, eng.sessionState.VerifiedKnowledge, 1,
		"premise: the entry must actually be stored, or the assertions below prove nothing")

	// The follow-up turn: no retrieval of its own, no platform read. This is the
	// case the deletion could have broken.
	require.Empty(t, eng.searchKnowledgeLedgerThisTurn.Items, "premise: this turn retrieved nothing")
	require.Empty(t, eng.readResponseEvidenceThisTurn, "premise: this turn read nothing")

	ledger := eng.knowledgeLedgerForVerification("那包月呢")
	assert.Equal(t, "那包月呢", ledger.Query,
		"the ledger is relabelled with the follow-up, so the verifier judges the question actually asked")
	require.NotEmpty(t, ledger.Items,
		"a follow-up that retrieves nothing must still be checkable against prior verified evidence; "+
			"an empty ledger here makes every such answer unsupported")
	assert.Equal(t, "w0-pricing-4090", ledger.Items[0].ChunkID)
}

// TestVerifiedKnowledgeNoLongerReachesTheModel is the other half: the same stored
// entry must not come back through any model-facing path, with the transcript on
// OR off. Off matters because this is a deletion, not a suppression — there is no
// flag position that restores the block, and a test asserting only the on-path
// would keep passing if someone reintroduced the injection behind the flag.
func TestVerifiedKnowledgeNoLongerReachesTheModel(t *testing.T) {
	stored := []VerifiedKnowledgeTurn{{
		Question: "4090 一小时多少钱",
		Answer:   "每小时 2 元。",
		Evidence: knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "w0-pricing-4090"}}},
	}}

	for _, on := range []bool{true, false} {
		withCanonicalTranscript(t, on)
		eng := &Engine{sessionStateHydrated: true, sessionState: SessionState{VerifiedKnowledge: stored}}
		// A live element the card always keeps. Without it the card is only its
		// header, renderAgentContextCard returns "", and every NotContains below
		// passes on an empty string — which is how this test read before the
		// premise assertion was added.
		eng.continuityAdvisories.Notices = []string{"上一轮的停止操作已恢复"}
		view := (ContextCompiler{}).Compile(eng, "那包月呢", time.Unix(1_750_000_000, 0))
		card := renderAgentContextCard(view)

		// A NotContains assertion passes for free on an empty card. Pin that the
		// card was actually rendered first, or this test degrades into a tripwire
		// that never trips.
		require.Contains(t, card, "【本轮统一上下文",
			"canonical=%v: premise — the card must have rendered at all", on)

		assert.NotContains(t, card, "已验证知识：",
			"canonical=%v: the block is deleted, not gated", on)
		assert.NotContains(t, card, "每小时 2 元。",
			"canonical=%v: the stored answer must not reach the model by any other label", on)
	}
}
