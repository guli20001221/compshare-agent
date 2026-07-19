package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

// TestProposalRejectsModelSelfElectedTargetViaSingleDescribe pins the mid-turn
// self-election vector through the real Resolver path. The workflow-layer trust
// gate that used to catch it (workflowTargetIsTrusted) is gone; authority now
// lives only in the Resolver's dual proof.
//
// Chain: a single-host DescribeCompShareInstance the model issues in-loop is an
// OriginDirectLLM tool call -> recordToolFacts -> recordInstanceStateFacts
// (len==1) -> recordObservedInstanceID, which sets sessionState.SelectedInstanceID
// with source OBSERVED. staleStateNote actively nudges the model to describe
// before mutating, so describe-one-then-stop is a realistic flash path, not a
// contrived one. The user's "怎么关机" referenced no instance, so the observed
// referent supplies no SelectionProof and the resolver refuses to make the stop
// ready — it can never reach executeResolvedWorkflow.
func TestProposalRejectsModelSelfElectedTargetViaSingleDescribe(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "怎么关机"               // zero-target request on a multi-instance account
	eng.selectedInstanceIDAtTurnStart = "" // nothing selected before this turn
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))

	// Model describes the ONE instance it chose (nudged by staleStateNote).
	eng.recordToolFacts("DescribeCompShareInstance", map[string]any{"UHostId": "uhost-a"}, &tools.SafeToolResult{
		RawResult: map[string]any{"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
		}},
	})
	require.Equal(t, "uhost-a", eng.sessionState.SelectedInstanceID,
		"precondition: a single-host describe self-elects the target mid-turn")
	require.Equal(t, SelectedInstanceSourceObserved, eng.sessionState.SelectedInstanceSource,
		"precondition: single-host tool observations are recorded as observed, not user-selected")

	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-selfelect", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-selfelect", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-a"}},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"a model self-elected target (via a single describe) supplies no user selection and must be refused")
}
