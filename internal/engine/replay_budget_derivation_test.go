package engine

import (
	"fmt"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The history budget must preserve the actual conversation, not merely a suffix
// whose size happens to fit beside every historical tool result. Tool traffic is
// the expandable part; old tool detail is therefore compacted before a complete
// user/assistant exchange is discarded.
//
// Sizes below are measured, not invented: 24 real transcripts persisted by an
// arm-B replay of production traffic have a median of 5,486 runes and a p90 of
// 7,659 (max 17,686, which alone exceeded the entire previous budget).
func TestTranscriptSizedHistoryPreservesSessionDialogueAndCompactsOldDetail(t *testing.T) {
	withCanonicalTranscript(t, true)

	for _, tc := range []struct {
		name    string
		perTurn int
	}{
		{"median transcript", 5486},
		{"p90 transcript", 7659},
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
			require.Len(t, kept, deepestProductionSession,
				"%d-rune tool results must not make the model forget complete dialogue", tc.perTurn)
			assert.Empty(t, kept[0].Transcript,
				"premise: old detail must actually be compacted, or this test only proves a roomy budget")
			assert.NotEmpty(t, kept[len(kept)-1].Transcript,
				"the newest tool detail is the likely antecedent of a follow-up and must win")
			assert.Equal(t, pairs[len(pairs)-1].User, kept[len(kept)-1].User,
				"the newest exchange must remain in the continuous conversation")
		})
	}
}
