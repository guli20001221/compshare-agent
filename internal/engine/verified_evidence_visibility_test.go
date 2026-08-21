package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/knowledge"
)

// Verified evidence remains available to the verifier but never enters model
// context. A follow-up with no new retrieval can therefore still be checked.
func TestVerifierCanUsePriorEvidence(t *testing.T) {
	prior := knowledge.EvidenceLedger{
		Query: "4090 一小时多少钱",
		Items: []knowledge.EvidenceItem{{
			ChunkID: "w0-pricing-4090",
			Title:   "计费概览",
			Snippet: "RTX 4090 按量计费为每小时 2.00 元。",
		}},
	}
	eng := &Engine{}
	eng.rememberVerifiedEvidence("4090 一小时多少钱", prior)
	require.Len(t, eng.sessionState.VerifiedEvidence, 1,
		"premise: the entry must actually be stored, or the assertions below prove nothing")

	// The follow-up turn has no retrieval or platform read of its own.
	require.Empty(t, eng.searchKnowledgeLedgerThisTurn.Items, "premise: this turn retrieved nothing")
	require.Empty(t, eng.platformReadEvidenceThisTurn, "premise: this turn read nothing")

	ledger := eng.knowledgeLedgerForVerification("那包月呢")
	assert.Equal(t, "那包月呢", ledger.Query,
		"the ledger is relabelled with the follow-up, so the verifier judges the question actually asked")
	require.NotEmpty(t, ledger.Items,
		"a follow-up that retrieves nothing must still be checkable against prior verified evidence; "+
			"an empty ledger here makes every such answer unsupported")
	assert.Equal(t, "w0-pricing-4090", ledger.Items[0].ChunkID)
}

// The same stored entry must not return through a model-facing path.
func TestVerifiedEvidenceNeverReachesTheModel(t *testing.T) {
	stored := []VerifiedEvidenceTurn{{
		Question: "4090 一小时多少钱",
		Evidence: knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{
			ChunkID: "w0-pricing-4090",
			Snippet: "verifier-only-evidence-marker",
		}}},
	}}

	eng := &Engine{sessionStateHydrated: true, sessionState: SessionState{
		VerifiedEvidence:   stored,
		SelectedInstanceID: "uhost-1", SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix:    time.Unix(1_750_000_000, 0).Unix(),
		SelectedInstanceFreshness: ContinuityFreshnessFresh,
	}}
	view := (ContextCompiler{}).Compile(eng, "那包月呢", time.Unix(1_750_000_001, 0))
	card := renderAgentContextCard(view)

	require.Contains(t, card, "【本轮执行上下文",
		"premise: a live execution row must make the card non-empty")
	assert.NotContains(t, card, "verifier-only-evidence-marker", "stored evidence must not reach the model")
}
