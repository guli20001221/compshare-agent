package engine

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/require"
)

func TestAttachTraceHooksWiresEveryEngineObserver(t *testing.T) {
	eng := &Engine{}
	eng.AttachTraceHooks(TraceHooks{
		Retrieval: func(observability.RetrievalTrace) {}, Freshness: func(observability.FreshnessTrace) {},
		Diagnosis: func(observability.DiagnosisTrace) {}, Outcome: func(observability.OutcomeTrace) {},
		Renderer: func(observability.RendererTrace) {}, HardBlock: func(observability.EngineHardBlockTrace) {},
		Completion: func(observability.TurnCompletionTrace) {},
		RateLimit:  func(governance.Decision) {}, TokenUsage: func(llm.TokenUsage) {},
		Confirmation: func(observability.ConfirmationTrace) {},
	})
	require.NotNil(t, eng.retrievalTraceObserver)
	require.NotNil(t, eng.freshnessTraceObserver)
	require.NotNil(t, eng.diagnosisTraceObserver)
	require.NotNil(t, eng.outcomeTraceObserver)
	require.NotNil(t, eng.rendererTraceObserver)
	require.NotNil(t, eng.hardBlockObserver)
	require.NotNil(t, eng.turnCompletionObserver)
	require.NotNil(t, eng.rateLimitObserver)
	require.NotNil(t, eng.tokenUsageObserver)
	require.NotNil(t, eng.confirmationTraceObserver)
}

func TestTraceSnapshotReportsOnlyBoundedContinuityMetadata(t *testing.T) {
	eng := &Engine{
		turnContextViewThisTurn: TurnContextView{
			CurrentQuestion:    "secret question",
			RecentConversation: []ConversationPair{{User: "secret user", Assistant: "secret answer"}},
			ActiveTask:         &TaskSnapshot{Goal: "secret task"},
			ContinuityNotices:  []string{"secret notice"},
		},
		promptSectionIDsThisTurn:   []string{"identity", "knowledge_turn_policy", "user_state"},
		memoryUpdateSourceThisTurn: memoryUpdateCompactor,
		groundingOutcomeThisTurn:   groundingSupported,
	}
	snapshot := eng.TraceSnapshot(time.Now())
	require.Equal(t, []string{"recent_pairs", "active_task", "notices"}, snapshot.ContextSources)
	require.Equal(t, string(ResponseAgent), snapshot.ResponseContract)
	require.Equal(t, []string{"identity", "knowledge_turn_policy", "user_state"}, snapshot.PromptSectionIDs)
	require.Equal(t, memoryUpdateCompactor, snapshot.MemoryUpdateSource)
	require.Equal(t, groundingSupported, snapshot.GroundingOutcome)
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
