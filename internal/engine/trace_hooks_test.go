package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTraceSnapshotReportsOnlyBoundedContinuityMetadata(t *testing.T) {
	eng := &Engine{
		turnContextViewThisTurn: AgentContext{
			CurrentQuestion:    "secret question",
			RecentConversation: []ConversationPair{{User: "secret user", Assistant: "secret answer"}},
		},
		promptSectionIDsThisTurn:             []string{"identity", "knowledge_turn_policy", "user_state"},
		verifiedEvidenceUpdateThisTurn:       evidenceUpdateRecorded,
		groundingOutcomeThisTurn:             groundingSupported,
		promptMessagesRawPeakThisTurn:        12,
		promptMessagesAssembledPeakThisTurn:  9,
		promptMessagesCapAppliedThisTurn:     true,
		selectedInstanceIDAtTurnStart:        "uhost-start",
		selectedInstanceSourceAtTurnStart:    SelectedInstanceSourceUser,
		selectedInstanceFreshnessAtTurnStart: ContinuityFreshnessExpired,
	}
	snapshot := eng.TraceSnapshot(time.Now())
	require.Equal(t, []string{"recent_pairs"}, snapshot.ContextSources)
	require.Equal(t, string(ResponseAgent), snapshot.ResponseContract)
	require.Equal(t, []string{"identity", "knowledge_turn_policy", "user_state"}, snapshot.PromptSectionIDs)
	require.Equal(t, 12, snapshot.PromptMessagesRawPeak)
	require.Equal(t, 9, snapshot.PromptMessagesAssembledPeak)
	require.True(t, snapshot.PromptMessagesCapApplied)
	require.Equal(t, evidenceUpdateRecorded, snapshot.EvidenceUpdateSource)
	require.Equal(t, groundingSupported, snapshot.GroundingOutcome)
	require.Equal(t, "uhost-start", snapshot.SelectedInstanceIDAtStart)
	require.Equal(t, SelectedInstanceSourceUser, snapshot.SelectedInstanceSourceAtStart)
	require.Equal(t, ContinuityFreshnessExpired, snapshot.SelectedInstanceFreshnessAtStart)
	for _, value := range append(append([]string{}, snapshot.ContextSources...), snapshot.PromptSectionIDs...) {
		require.NotContains(t, value, "secret")
	}
}

func TestTraceSnapshotReportsPolicyTerminal(t *testing.T) {
	eng := &Engine{
		hardBlockStandingThisTurn: true,
	}

	snapshot := eng.TraceSnapshot(time.Now())
	require.Equal(t, string(ResponsePolicyTerminal), snapshot.ResponseContract)
}
