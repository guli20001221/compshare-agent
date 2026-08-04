package engine

import (
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/stretchr/testify/assert"
)

// maxReplayedHistoryRunes is not a tuning knob, it is the result of an arithmetic
// derivation over three inputs: the per-turn token cap, how many times a turn
// re-sends history, and the share of the turn budget history is allowed.
//
// Pinning the number alone would let any of those three move without the number
// moving with it — which is exactly what happened to the history-ceiling test's
// copy of the cap. So this re-runs the derivation instead of restating its output.
func TestReplayBudgetStillMatchesItsDerivation(t *testing.T) {
	// The share is the one genuinely chosen input: history gets at most half the
	// turn, leaving the other half for the system prompt, tool results and the
	// completion. Changing it is a decision, so it is written here rather than
	// inferred.
	const historyShareDenominator = 2

	perRequestTokens := config.ShippedMaxTokensPerTurn / historyShareDenominator / maxReActRounds

	// 1 rune = 1 token for CJK, the deliberately conservative side: CJK is closer
	// to 1 token per character than Latin, so this under-counts capacity rather
	// than over-committing it.
	//
	// Asserted as a band, not equality: the shipped 12000 is the derived 12500
	// rounded down to a legible number, and pinning equality would mean either
	// writing 12500 or carrying a magic offset that looks like arithmetic and is
	// not. The band says what is actually required — at or under the derivation,
	// and close enough to it to still BE the derivation.
	assert.LessOrEqual(t, maxReplayedHistoryRunes, perRequestTokens,
		"maxReplayedHistoryRunes=%d exceeds its own derivation: %d tokens/turn ÷ %d ÷ %d "+
			"rounds = %d per request",
		maxReplayedHistoryRunes, config.ShippedMaxTokensPerTurn, historyShareDenominator,
		maxReActRounds, perRequestTokens)

	assert.Greater(t, maxReplayedHistoryRunes, perRequestTokens*4/5,
		"maxReplayedHistoryRunes=%d has fallen well below the %d its inputs now derive. "+
			"One of them moved — most likely the token cap or maxReActRounds — and the "+
			"budget was left at a number that used to be right",
		maxReplayedHistoryRunes, perRequestTokens)
}

// The budget is worth having only if it is bigger than real sessions, or it trims
// every conversation. 11,271 runes is the largest full-history replay measured
// across the three 2026-07 production exports (p90 1,801; p99 5,764).
func TestReplayBudgetClearsTheLargestObservedSession(t *testing.T) {
	const largestObservedReplayRunes = 11_271

	assert.Greater(t, maxReplayedHistoryRunes, largestObservedReplayRunes,
		"the budget now bites on sessions that have actually occurred; every observed "+
			"session replayed complete when it was set")
}
