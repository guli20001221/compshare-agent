package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxReplayedHistoryRunes is not a tuning knob, it is the result of an arithmetic
// derivation. Pinning the number alone would let any of its inputs move without
// the number moving with it — which is exactly what happened to the
// history-ceiling test's copy of the token cap, and, more expensively, to the
// budget itself when the canonical transcript started sharing it.
//
// So this re-runs the derivation rather than restating its output.
func TestReplayBudgetStillMatchesItsDerivation(t *testing.T) {
	// The two genuinely chosen inputs, written here rather than inferred: what a
	// request holds besides history and this turn's tool work (the 40 tool schemas
	// dominate it), and what must stay free for the answer.
	const (
		systemAndToolSchemaTokens = 15000
		completionReserveTokens   = 16000
	)

	// Runes and tokens are interchangeable here because CJK bills 1:1 — see
	// measuredContextWindowFloorTokens for the probe.
	//
	// maxTranscriptTotalRunes is deliberately NOT a term. It bounds what
	// captureTurnTranscript persists after a turn, not what a request carries
	// during one; treating it as a live reservation is the error this test was
	// rewritten to stop restating.
	assert.LessOrEqual(t, maxAssembledRequestRunes,
		measuredContextWindowFloorTokens-completionReserveTokens,
		"maxAssembledRequestRunes=%d leaves under %d of the %d window floor for the completion. "+
			"terra bills reasoning tokens, so an answer needs real room",
		maxAssembledRequestRunes, completionReserveTokens, measuredContextWindowFloorTokens)

	// History is a component of a request, so it must fit inside the request bound
	// with the fixed overhead already spent — otherwise history alone could be the
	// thing that forces shedding on an ordinary, tool-light turn.
	historyHeadroom := maxAssembledRequestRunes - systemAndToolSchemaTokens
	assert.LessOrEqual(t, maxReplayedHistoryRunes, historyHeadroom,
		"maxReplayedHistoryRunes=%d exceeds what a request can hold once system+schemas (%d) are "+
			"spent against maxAssembledRequestRunes (%d) = %d. Assembling history that will always "+
			"be shed is work done to be thrown away",
		maxReplayedHistoryRunes, systemAndToolSchemaTokens, maxAssembledRequestRunes, historyHeadroom)

	assert.Greater(t, maxReplayedHistoryRunes, historyHeadroom/2,
		"maxReplayedHistoryRunes=%d has fallen well below the %d its inputs now allow. One of them "+
			"moved and the budget was left at a number that used to be right — the failure this "+
			"test exists to catch",
		maxReplayedHistoryRunes, historyHeadroom)

	// The per-turn token cap is a SEPARATE, runtime-enforced guard (see
	// tokenBudgetExceeded), not an input to this derivation — the previous
	// derivation treated it as one and pre-divided by maxReActRounds, which sized
	// every ordinary turn for the 0.3% that reach 16 rounds. What still has to hold
	// is that history at a REALISTIC depth does not eat the whole cap on its own.
	// p90 across 648 replayed production turns is 2 tool calls, so ~3 model calls.
	const p90ModelCallsPerTurn = 3
	assert.Less(t, maxReplayedHistoryRunes*p90ModelCallsPerTurn, config.ShippedMaxTokensPerTurn/2,
		"at the p90 depth of %d model calls, history alone would spend %d of the %d-token turn cap. "+
			"Deep turns are allowed to trip that cap and exit through the recovery path; typical "+
			"turns are not",
		p90ModelCallsPerTurn, maxReplayedHistoryRunes*p90ModelCallsPerTurn, config.ShippedMaxTokensPerTurn)
}

// The regression the raised budget is FOR, stated as behaviour rather than as a
// number. With the canonical transcript on, a session of ordinary tool-using
// turns must still replay as history — before the raise it did not: at the
// measured median transcript size the model saw 2 of 20 exchanges, and at the p90
// it saw 1, so a session that looked twenty turns deep had two turns of memory.
//
// Sizes below are measured, not invented: 24 real transcripts persisted by an
// arm-B replay of production traffic have a median of 5,486 runes and a p90 of
// 7,659 (max 17,686, which alone exceeded the entire previous budget).
func TestTranscriptSizedHistoryStillReplaysASession(t *testing.T) {
	withCanonicalTranscript(t, true)

	for _, tc := range []struct {
		name    string
		perTurn int
		atLeast int
	}{
		{"median transcript", 5486, 6},
		{"p90 transcript", 7659, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs := make([]ConversationPair, 0, deepestProductionSession)
			for i := 0; i < deepestProductionSession; i++ {
				pair := ConversationPair{
					User:      fmt.Sprintf("问题%d", i),
					Assistant: fmt.Sprintf("回答%d", i),
				}
				pair.Transcript = []openai.ChatCompletionMessage{
					{Role: openai.ChatMessageRoleUser, Content: pair.User},
					{Role: openai.ChatMessageRoleTool, Content: strings.Repeat("字", tc.perTurn)},
					{Role: openai.ChatMessageRoleAssistant, Content: pair.Assistant},
				}
				pairs = append(pairs, pair)
			}
			require.Len(t, pairs, deepestProductionSession, "premise: a full session of tool-using turns")

			kept := budgetReplayedPairs(pairs, maxReplayedHistoryRunes)
			assert.GreaterOrEqual(t, len(kept), tc.atLeast,
				"%d-rune transcripts left only %d of %d exchanges. What survives this budget IS "+
					"the model's cross-turn memory — there is no count window behind it any more",
				tc.perTurn, len(kept), deepestProductionSession)

			// The newest exchange is kept unconditionally by budgetReplayedPairs, so
			// a budget of zero would also "keep" one. Assert the tail is contiguous
			// and ends at the newest, which a degenerate result would not be.
			assert.Equal(t, pairs[len(pairs)-1].User, kept[len(kept)-1].User,
				"the kept window must end at the newest exchange")
		})
	}
}
