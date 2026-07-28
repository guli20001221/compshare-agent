package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

// createProposalArgs is a CreateInstanceWorkflow proposal naming a GPU the user
// really did say — the only thing that can go wrong here is on our side.
func createProposalArgs(turnID, gpuType string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "operation": "CreateInstanceWorkflow",
		"slots": []any{map[string]any{
			"name": "GpuType", "value": gpuType, "source": "user_explicit",
			"evidence": map[string]any{"quote": gpuType},
		}},
	}
}

func TestProposalImageCatalogSourceUsesCapabilityDefaultWithoutModelConstant(t *testing.T) {
	spec := actionresolver.OperationSpec{ImageCatalogSource: "custom"}
	require.Equal(t, "custom", proposalImageCatalogSource(actionresolver.ActionProposal{}, spec))
	require.Equal(t, "sharing", proposalImageCatalogSource(actionresolver.ActionProposal{
		Slots: []actionresolver.SlotCandidate{{Name: "ImageSource", Value: "sharing"}},
	}, spec))
}

// TestProposeActionCatalogOutageParksNoPendingTask is the engine half of the
// "our failure is not the user's fault" contract. The resolver reporting a
// DependencyFailure is not enough on its own: the engine must also refuse to
// persist a task frame from it. A frame here would survive into later turns as
// "waiting for the user to supply GpuType" — a task they cannot possibly resolve,
// because the value was never the problem.
func TestProposeActionCatalogOutageParksNoPendingTask(t *testing.T) {
	// mockExecutor has no result for DescribeAvailableCompShareInstanceTypes, so
	// querySafeRead returns nil and the snapshot reports Available=false — the
	// same shape a real upstream outage produces.
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.sessionStateHydrated = true
	eng.lastUserMsg = "给我开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-outage", time.Now())
	eng.turnContextViewReady = true

	_ = eng.executeActionProposal(context.Background(), createProposalArgs("turn-outage", "4090"), noopStep)

	require.Empty(t, eng.sessionState.ContextFrame.Workflow,
		"a catalog outage must not park a task frame asking the user to re-supply GpuType")
	require.Empty(t, eng.sessionState.ContextFrame.MissingSlots)
}

// TestProposeActionResolvesGpuTypeAgainstLiveCatalog is the engine-level wiring
// gate: the proposal path must actually query the upstream catalog and carry the
// canonical name through. The resolver unit tests cannot see this — they are
// handed a snapshot, so they would keep passing if the engine never fetched one.
func TestProposeActionResolvesGpuTypeAgainstLiveCatalog(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090"},
			map[string]any{"Name": "4090_48G"},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "给我开一台 4090 48G"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-live", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), createProposalArgs("turn-live", "4090 48G"))

	require.NoError(t, err)
	require.Empty(t, resolved.action.DependencyFailures, "the catalog was reachable")
	require.Equal(t, "4090_48G", resolved.action.Arguments["GpuType"],
		"the engine must fetch the live catalog and the resolver must canonicalize against it")
	require.NotNil(t, resolved.action.Confirmation)
	require.Equal(t, "4090_48G", resolved.action.Confirmation.Arguments["GpuType"],
		"confirm card and executed args must be the same string")
}

func TestProposeActionShadowRejectsSubstringTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "pytest"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-2", time.Now())
	eng.turnContextViewReady = true
	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-2","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"test","source":"user_explicit","evidence":{"message_id":"turn-2","start":2,"end":6,"quote":"test"}}]}`), noopStep)
	var resolved actionresolver.ResolvedAction
	require.NoError(t, json.Unmarshal([]byte(out), &resolved))
	require.False(t, resolved.ReadyForConfirmation)
	require.NotEmpty(t, resolved.Rejected)
}

func TestProposeActionShadowNeverEchoesSensitiveValues(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1"}}}, "test"))
	eng.lastUserMsg = "重置密码"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-secret", time.Now())
	eng.turnContextViewReady = true
	var events []StepEvent
	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-secret","operation":"ResetPasswordWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"verified_context","evidence":{"context_field":"selected_entities"}},{"name":"Password","value":"SecurePass123!","source":"agent_inference"}]}`), func(event StepEvent) { events = append(events, event) })
	require.NotContains(t, out, "SecurePass123!")
	for _, event := range events {
		payload, _ := json.Marshal(event.TraceResult)
		require.False(t, strings.Contains(string(payload), "SecurePass123!"))
	}
}

func TestCentralAgentProposalExecutesOnlyThroughExistingWorkflowGate(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, executor, func(action string, args map[string]any) bool {
		confirmCalls++
		require.Equal(t, "StopInstanceWorkflow", action)
		return true
	})
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "停止 uhost-1"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-write", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-write","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"user_explicit","evidence":{"message_id":"turn-write","start":3,"end":10,"quote":"uhost-1"}}]}`), noopStep)

	require.Contains(t, out, "执行关机")
	require.Equal(t, 1, confirmCalls)
	require.Contains(t, executor.calls, "StopCompShareInstance")
}

// A concrete target the Agent proposes but the server cannot confirm EXISTS (a
// point-query that echoes no matching id) is refused before the confirmation card —
// the human gate is never reached and nothing mutates. The point-query itself is a
// legitimate, expected existence check under the uniform model.
func TestNonexistentProposedTargetIsRefusedBeforeConfirmation(t *testing.T) {
	executor := &mockExecutor{} // DescribeCompShareInstance echoes no matching UHostSet
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool {
		t.Fatal("a target the server cannot confirm exists must not reach the confirmation card")
		return true
	})
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-unverified", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-unverified","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-invented","source":"agent_inference"}]}`), noopStep)

	require.Contains(t, out, "target existence could not be confirmed")
	require.NotContains(t, executor.calls, "StopCompShareInstance", "a nonexistent target must not mutate")
}

func TestCentralAgentProposalCannotWriteWithoutRequiredJournal(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	confirm := func(string, map[string]any) bool { return true }
	eng := NewWithDeps(&mockLLM{}, executor, confirm)
	eng.safeExecutor = newSafeToolExecutor(executor, confirm, nil, true)
	eng.safeExecutor.SetMutatingToolsEnabled(true)
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "停止 uhost-1"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-no-journal", time.Now())
	eng.turnContextViewReady = true

	_ = eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-no-journal","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-1","source":"user_explicit","evidence":{"message_id":"turn-no-journal","start":3,"end":10,"quote":"uhost-1"}}]}`), noopStep)

	require.NotContains(t, executor.calls, "StopCompShareInstance")
}

// TestInferredTargetPointQueriedRefusedWhenResponseDoesNotEcho: an inferred target
// with no same-id-verified read this turn and a cold registry is POINT-QUERIED for
// existence (uniform verification — no source gate). A point-query whose response
// echoes no matching id is NotFound, so the write is refused: existence comes only
// from a response that echoes the exact id, never from a bare inference.
func TestInferredTargetPointQueriedRefusedWhenResponseDoesNotEcho(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil) // Describe echoes no matching UHostSet
	eng.lastUserMsg = "停止它"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-read-only", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-read-only", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1", "source": "agent_inference"}},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "a point-query that echoes no id is not existence")
	require.Contains(t, resolved.action.Rejected, "UHostId: target existence could not be confirmed")
}

// TestSameIdVerifiedReadIsExistenceForInferredTarget is the counterpart: once a
// resource_info response echoed the exact id (verifiedInstanceEvidenceThisTurn), an
// Agent-inferred pronoun target for it EXISTS, so it reaches the confirmation card
// (the user's confirm is the SelectionProof). This is the only read channel that
// authorizes existence — a Monitor subject would not populate this set.
func TestSameIdVerifiedReadIsExistenceForInferredTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "停止它"
	eng.verifiedInstanceEvidenceThisTurn = map[string]struct{}{"uhost-1": {}}
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-verified-read", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-verified-read", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	})

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-1", resolved.action.Arguments["UHostId"])
}

// TestAmbiguousInferredInstanceTargetAsksInsteadOfConfirming reproduces the live
// "关闭当前我租界的卡" bug (terra, 16 running instances): the user names no instance,
// so the Agent lists them and then proposes the FIRST as the stop target. That id
// exists, so under the old existence-only rule it reached the confirmation card —
// and a reflexive confirm would stop an instance the user never chose. This turn's
// own evidence names MORE THAN ONE instance, so an Agent-inferred target is a pick
// among many and must ask "请明确指定要操作的实例" (a Conflict), never confirm a guess.
// It is the exact counterpart of the single-verified pronoun case above, which
// still reaches the card.
func TestAmbiguousInferredInstanceTargetAsksInsteadOfConfirming(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "关掉我的实例"
	// The turn's reads surfaced a LISTING, not one referent — more than one
	// instance was verified this turn.
	eng.verifiedInstanceEvidenceThisTurn = map[string]struct{}{"uhost-1": {}, "uhost-2": {}, "uhost-3": {}}
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-ambig", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-ambig", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1", "source": "agent_inference"}},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "an arbitrary pick among many must not reach the confirmation card")
	require.NotEmpty(t, resolved.action.Conflicts, "it must ASK which instance (a conflict), not silently reject the id as nonexistent")
	require.Empty(t, resolved.action.Arguments["UHostId"], "the guessed id must not survive as a resolved argument")
}

// TestUserExplicitTargetTrustedByPointQueryWhenRegistryCold: the user typed the
// exact id (SelectionProof), the registry is cold, and a this-turn point Describe
// returns that same id (ExistenceProof) — the write is authorized (acceptance #1),
// even though the registry never confirmed it.
func TestUserExplicitTargetTrustedByPointQueryWhenRegistryCold(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "train-a", "State": "Running"}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "停止 uhost-1"
	// registry never synced -> cold, cannot assert absence.
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cold", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-cold", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	})

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Contains(t, executor.calls, "DescribeCompShareInstance", "a cold selected target must be existence-verified by a point-query")
}

// TestPointQueryRejectsTargetTheResponseDoesNotContain: the user typed an id and
// the cold-registry point Describe returns a DIFFERENT id — no ExistenceProof, so
// the write is refused (acceptance #2). Existence comes only from the response
// echoing the same id, never from having asked for it.
func TestPointQueryRejectsTargetTheResponseDoesNotContain(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-other"}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "停止 uhost-1"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-mismatch", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-mismatch", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "a point-query that does not echo the id is not existence")
}

// TestBareContextNameIsNotAutoResolvedToInstanceID: the server verifies the EXACT
// value the Agent proposes as the id — it never canonicalizes a bare context name to
// an instance id (only an explicit in-message id/name reference is bound). So an Agent
// that puts an instance NAME in the id slot, against a complete registry that has no
// instance whose UHostId literally equals that name, is refused as NotFound. The Agent
// must propose the resolved id (which then existence-verifies and reaches the card);
// a context name is not a substitute for that.
func TestBareContextNameIsNotAutoResolvedToInstanceID(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	// Two instances, so the account-single completion does not apply.
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(2), "UHostSet": []any{
		map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running"},
		map[string]any{"UHostId": "uhost-2", "Name": "other", "State": "Running"},
	}}, "test"))
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "host",
		SelectedInstanceSource: SelectedInstanceSourceObserved,
		SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	eng.lastUserMsg = "确认关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-observed", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"operation": "StopInstanceWorkflow",
		"slots":     []any{map[string]any{"name": "UHostId", "value": "host"}},
	})
	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "a bare context name in the id slot is not auto-resolved to an id")
}

// TestAuthoritativeRegistryAbsentTargetRejectedWithoutPointQuery: the user typed
// an id not in a fresh, complete registry. Absence is authoritative, so the write
// is refused WITHOUT a wasted point-query (acceptance #5, write path).
func TestAuthoritativeRegistryAbsentTargetRejectedWithoutPointQuery(t *testing.T) {
	describeCalls := 0
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			describeCalls++
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running"}}}, "test"))
	eng.lastUserMsg = "停止 uhost-ghost"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-absent", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-absent", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-ghost"}},
	})
	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation)
	require.Zero(t, describeCalls, "an authoritative registry that can assert absence must not point-query")
}

// TestAuthorizedTargetIsRecordedAsUserSelection: once a write target passes the
// dual proof this turn, it is persisted as a genuine user selection so a later
// "关掉它" can inherit it — the writer that keeps the confirm-follow-up fixture
// non-vacuous. The workflow's own re-observation must not downgrade it.
func TestAuthorizedTargetIsRecordedAsUserSelection(t *testing.T) {
	// The stop saga needs the instance's real zone (and the support-zone map) to
	// build its args; a confirmed target is recorded only after the workflow lands.
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false}}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(true)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01"}}}, "test"))
	eng.lastUserMsg = "停止 uhost-1"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-record", time.Now())
	eng.turnContextViewReady = true

	_ = eng.executeActionProposal(context.Background(), map[string]any{
		"turn_id": "turn-record", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	}, noopStep)

	require.Equal(t, "uhost-1", eng.sessionState.SelectedInstanceID)
	require.Equal(t, SelectedInstanceSourceUser, eng.sessionState.SelectedInstanceSource, "an authorized target must persist as a user selection, undowngraded by the workflow's observation")
}

// TestConfirmedTargetRecordedEvenWhenUpstreamWriteFails encodes the B2 invariant:
// the confirmation gate IS the SelectionProof, so a target the user CONFIRMED is
// remembered as a genuine user selection even when the subsequent upstream write
// fails — otherwise a retry ("关掉它") forgets what the user just approved and
// re-asks. A DECLINED confirmation still records nothing. This is distinct from
// whole-workflow success (the old gate): a confirmed-then-failed Stop passed its
// confirm gate (ExecutionAuthorized) but not full success, and must still persist.
func TestConfirmedTargetRecordedEvenWhenUpstreamWriteFails(t *testing.T) {
	newStopEngine := func(confirm bool, stopErr error) *Engine {
		executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
			case "DescribeCompShareSupportZone":
				return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false}}}, nil
			case "StopCompShareInstance":
				if stopErr != nil {
					return nil, stopErr
				}
				return map[string]any{"RetCode": float64(0)}, nil
			default:
				return map[string]any{"RetCode": float64(0)}, nil
			}
		}}
		eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return confirm })
		eng.SetMutatingToolsEnabled(true)
		eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
		require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01"}}}, "test"))
		eng.lastUserMsg = "停止 uhost-1"
		eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-b2", time.Now())
		eng.turnContextViewReady = true
		return eng
	}
	proposal := map[string]any{
		"turn_id": "turn-b2", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	}

	t.Run("confirmed_but_upstream_write_fails_still_records", func(t *testing.T) {
		eng := newStopEngine(true, errors.New("upstream 500"))
		_ = eng.executeActionProposal(context.Background(), proposal, noopStep)
		require.Equal(t, "uhost-1", eng.sessionState.SelectedInstanceID,
			"a confirmed target must persist as a user selection even when the upstream write then fails")
		require.Equal(t, SelectedInstanceSourceUser, eng.sessionState.SelectedInstanceSource)
	})

	t.Run("declined_confirmation_records_nothing", func(t *testing.T) {
		eng := newStopEngine(false, nil)
		_ = eng.executeActionProposal(context.Background(), proposal, noopStep)
		require.Empty(t, eng.sessionState.SelectedInstanceID,
			"a declined target must never be recorded as a user selection")
	})
}

// TestWriteAuthorizationDualProofReachesTrace encodes the B3 invariant: the
// existence proof the resolver establishes for a write target (which oracle, when,
// which account, what verdict) — previously consumed only as a resolver gate and
// then discarded — now reaches the trace as an AuthorizationTrace, paired with
// whether the user's confirmation authorized execution. The target id and account
// are HASHED, never raw.
func TestWriteAuthorizationDualProofReachesTrace(t *testing.T) {
	run := func(confirm bool) []observability.AuthorizationTrace {
		executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "DescribeCompShareInstance":
				return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01"}}}, nil
			case "DescribeCompShareSupportZone":
				return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false}}}, nil
			default:
				return map[string]any{"RetCode": float64(0)}, nil
			}
		}}
		eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return confirm })
		eng.SetMutatingToolsEnabled(true)
		eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
		require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01"}}}, "test"))
		var traces []observability.AuthorizationTrace
		eng.SetAuthorizationTraceObserver(func(tr observability.AuthorizationTrace) { traces = append(traces, tr) })
		eng.lastUserMsg = "停止 uhost-1"
		eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-b3", time.Now())
		eng.turnContextViewReady = true
		// A user context so the account-scoped existence proof (AccountHash) is exercised.
		ctx := tools.WithUser(context.Background(), tools.UserContext{TopOrganizationID: 1, OrganizationID: 2})
		_ = eng.executeActionProposal(ctx, map[string]any{
			"turn_id": "turn-b3", "operation": "StopInstanceWorkflow",
			"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
		}, noopStep)
		return traces
	}

	t.Run("confirmed_write_emits_verified_dual_proof", func(t *testing.T) {
		traces := run(true)
		require.Len(t, traces, 1)
		tr := traces[0]
		require.Equal(t, "StopInstanceWorkflow", tr.Operation)
		require.Equal(t, "instance", tr.TargetKind)
		require.Equal(t, "verified", tr.ExistenceVerdict)
		require.Equal(t, "DescribeCompShareInstance", tr.ExistenceOracle)
		require.True(t, tr.ExecutionAuthorized, "a confirmed write is execution-authorized")
		require.NotZero(t, tr.ObservedUnix)
		// Ids and account are hashed, never raw.
		require.Equal(t, hashTraceValue("uhost-1"), tr.TargetIDHash)
		require.NotContains(t, tr.TargetIDHash, "uhost-1")
		require.Equal(t, hashTraceValue("1/2"), tr.AccountHash)
	})

	t.Run("declined_write_still_records_existence_proof_unauthorized", func(t *testing.T) {
		traces := run(false)
		require.Len(t, traces, 1)
		require.Equal(t, "verified", traces[0].ExistenceVerdict, "existence was proven before the card")
		require.False(t, traces[0].ExecutionAuthorized, "a declined write is not execution-authorized")
	})
}

// recordUserSelectedTargets persists ONLY an instance target as the session's
// SelectedInstanceID (review round-2 finding 2). A CFS or disk id must never land
// there, or a later "关掉它" would resolve to a CfsId/DiskId as if it were an instance.
func TestRecordUserSelectedTargetsPersistsOnlyInstanceKind(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	// A successful CFS resize must not write its CfsId into the instance selection.
	eng.recordUserSelectedTargets(actionresolver.ResolvedAction{
		Operation: "ResizeCFSWorkflow",
		Arguments: map[string]any{"CfsId": "cfs-a", "Size": float64(200)},
	})
	require.Empty(t, eng.sessionState.SelectedInstanceID, "a CFS id must never become the selected instance")

	// A disk resize carries BOTH UHostId and DiskId; only the parent instance is
	// persisted — deterministically, never the disk id (which Go's map iteration order
	// could otherwise pick).
	eng.recordUserSelectedTargets(actionresolver.ResolvedAction{
		Operation: "ResizeDiskWorkflow",
		Arguments: map[string]any{"UHostId": "uhost-a", "DiskId": "disk-1", "Size": float64(120)},
	})
	require.Equal(t, "uhost-a", eng.sessionState.SelectedInstanceID, "a disk resize persists the parent instance, never the disk id")
}

func TestConfirmedFollowUpInheritsOneFreshSelectedTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	// A prior turn recorded a genuine USER selection (not a mere observation), and
	// the instance still exists in a fresh registry — both proofs the dual-proof
	// requires for the "确认关机" follow-up to inherit the target.
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running"}}}, "test"))
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "host",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	eng.lastUserMsg = "确认关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-confirm", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"operation": "StopInstanceWorkflow",
		"slots":     []any{map[string]any{"name": "UHostId", "value": "host"}},
	})

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-1", resolved.action.Arguments["UHostId"])
	require.Equal(t, actionresolver.SourceVerifiedContext, resolved.action.Provenance["UHostId"].Source)
}

func TestConfirmedFollowUpExecutesThroughResolvedTargetAuthority(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "host", "State": "Running", "Zone": "cn-wlcb-01",
			}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(true)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-1",
		SelectedInstanceName:   "host",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	eng.lastUserMsg = "确认关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-confirm-execute", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeActionProposal(context.Background(), map[string]any{
		"operation": "StopInstanceWorkflow",
		"slots":     []any{map[string]any{"name": "UHostId", "value": "host"}},
	}, noopStep)

	require.Contains(t, out, "执行关机")
	require.Contains(t, executor.calls, "StopCompShareInstance")
}

func TestDeterministicReinstallReplyDoesNotInventANewPassword(t *testing.T) {
	withoutPassword, ok := deterministicWorkflowReply("ReinstallInstanceWorkflow", map[string]any{"UHostId": "uhost-1"})
	require.True(t, ok)
	require.Contains(t, withoutPassword, "未设置新密码")
	require.NotContains(t, withoutPassword, "刚设置")

	withPassword, ok := deterministicWorkflowReply("ReinstallInstanceWorkflow", map[string]any{
		"UHostId": "uhost-1", "PasswordConfigured": true,
	})
	require.True(t, ok)
	require.Contains(t, withPassword, "刚设置")
	require.NotContains(t, withPassword, "secret")
}

func TestDeterministicResizeCFSReplyCannotBeRestatedAsReadOnlyEstimate(t *testing.T) {
	reply, ok := deterministicWorkflowReply("ResizeCFSWorkflow", map[string]any{
		"CfsId": "cfs-test", "Size": float64(110),
	})
	require.True(t, ok)
	require.Contains(t, reply, "已将 CFS cfs-test 扩容到 110GB")
	require.NotContains(t, reply, "估算")
	require.NotContains(t, reply, "不会直接扩容")
}

func TestSecondWriteProposalInOneTurnIsRejected(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.actionProposalRanThisTurn = true

	reply := eng.executeActionProposal(context.Background(), map[string]any{
		"operation": "StartInstanceWorkflow",
	}, noopStep)

	require.True(t, strings.HasPrefix(reply, finalReplyPrefix))
	require.Contains(t, reply, "本轮已经处理过一个写操作请求")
	require.Equal(t, "duplicate_write_proposal", eng.actionProposalDispositionThisTurn)
}

func TestCurrentTurnCapacityQuoteIsVerifiedAndConvertedBySharedCodec(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "给 uhost-1 加200G数据盘"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1"}}}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-capacity", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"operation": "CreateDiskWorkflow",
		"slots": []any{
			map[string]any{"name": "UHostId", "value": "uhost-1", "source": "user_explicit", "evidence": map[string]any{"quote": "uhost-1"}},
			map[string]any{"name": "Size", "value": "200G", "source": "user_explicit", "evidence": map[string]any{"quote": "200G"}},
		},
	})
	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, float64(200), resolved.action.Arguments["Size"])
}

func TestCompleteCurrentTurnEvidenceAcceptsExactCustomImageName(t *testing.T) {
	catalog, err := actionresolver.BuildCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("CreateCustomImageWorkflow")
	require.True(t, ok)
	name := "codex-agent-fixed-1784566477"
	question := "请立即调用 ProposeAction_CreateCustomImageWorkflow，把实例 uhost-1szs4kk4wmjj 制作为自制镜像，镜像名称 " + name + "。不要用文字模拟确认，必须发出真实确认卡。"
	proposal := actionresolver.ActionProposal{
		TurnID:    "turn-custom-image",
		Operation: "CreateCustomImageWorkflow",
		Slots: []actionresolver.SlotCandidate{{
			Name: "Name", Value: name, Source: actionresolver.SourceUserExplicit,
		}},
	}
	view := AgentContext{TurnID: "turn-custom-image", CurrentQuestion: question}

	completed := completeCurrentTurnEvidence(proposal, view, spec)
	require.NotNil(t, completed.Slots[0].Evidence)
	require.Equal(t, name, completed.Slots[0].Evidence.Quote)
	require.True(t, verifyCurrentQuestionEvidence(view, completed.Slots[0], spec.Fields["Name"].Codec))
}

func TestProposalRejectsDifferentTurnEvidence(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, "停止 uhost-1", "active-turn", time.Now())
	eng.turnContextViewReady = true
	_, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{"turn_id": "old-turn", "operation": "StopInstanceWorkflow", "slots": []any{}})
	require.ErrorContains(t, err, "does not match")
}

func TestCentralAgentProposalSchemaComesFromWorkflowCatalog(t *testing.T) {
	window := centralAgentToolWindow(true, false)
	var stopTool, cfsTool map[string]any
	for _, tool := range window {
		if tool.Function == nil {
			continue
		}
		switch tool.Function.Name {
		case proposalToolName("StopInstanceWorkflow"):
			stopTool, _ = tool.Function.Parameters.(map[string]any)
		case proposalToolName("CreateCFSWorkflow"):
			cfsTool, _ = tool.Function.Parameters.(map[string]any)
		}
	}
	require.NotNil(t, stopTool)
	require.NotNil(t, cfsTool)
	properties := stopTool["properties"].(map[string]any)
	require.NotContains(t, properties, "operation", "the selected proposal tool fixes the operation server-side")
	require.Contains(t, properties, "UHostId")
	require.NotContains(t, properties, "Size")
	require.NotContains(t, properties, "slots")
	require.Empty(t, stopTool["required"], "an incomplete proposal must still be callable")
	cfsFields := cfsTool["properties"].(map[string]any)
	require.Contains(t, cfsFields, "Size")
	require.NotContains(t, cfsFields, "slots")
}

func TestCentralAgentReadSchemaComesFromCapabilityRegistry(t *testing.T) {
	window := centralAgentToolWindow(false, false)
	var priceTool, imageTool map[string]any
	for _, tool := range window {
		if tool.Function == nil {
			continue
		}
		switch tool.Function.Name {
		case capability.ReadToolName(intent.IntentPricingQuery):
			priceTool, _ = tool.Function.Parameters.(map[string]any)
		case capability.ReadToolName(intent.IntentImageList):
			imageTool, _ = tool.Function.Parameters.(map[string]any)
		}
	}
	require.NotNil(t, priceTool)
	require.NotNil(t, imageTool)
	priceFields := priceTool["properties"].(map[string]any)
	require.Contains(t, priceFields, "gpu_type")
	require.Contains(t, priceFields, "price_kind")
	require.Contains(t, priceFields, "gpu_count")
	require.NotContains(t, priceFields, "source")
	require.NotContains(t, priceFields, "slots")
	imageFields := imageTool["properties"].(map[string]any)
	require.Contains(t, imageFields, "source")
	require.NotContains(t, imageFields, "price_kind")
	require.NotContains(t, imageFields, "slots")
}

func TestSealedPasswordIsInjectedWithoutEnteringModelArguments(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.secretInputsThisTurn = map[string]string{"Password": "SecurePass123!"}
	eng.verifiedInstanceEvidenceThisTurn = map[string]struct{}{"uhost-1": {}}
	// The user names the instance (span SelectionProof); a same-id-verified read this
	// turn supplies the ExistenceProof — a valid target, so the sealed-password path runs.
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, "给 uhost-1 重置密码为[已脱敏:凭据]", "turn-secret", time.Now())
	eng.turnContextViewReady = true
	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-secret", "operation": "ResetPasswordWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	})
	require.NoError(t, err)
	require.Equal(t, "SecurePass123!", resolved.action.Arguments["Password"])
	require.Equal(t, "[REDACTED]", resolved.action.Confirmation.Arguments["Password"])
}

// TestResolvedProposalDisposition pins the value-free classification the acceptance
// measurement reads to attribute why a write proposal did or did not card. A card
// path (confirmation / intake_form) wins; a server outage (dependency_failure) is
// reported before a user-facing rejection; only field names + typed kinds appear
// (never slot VALUES), so it is safe to persist in the trace.
// A write TARGET the Agent left blank must not get easier to authorize by being
// pruned. Pruning moves it from Rejected to Missing — a different channel, the
// same refusal: no confirmation card, and no guided form either (stop declares
// none). This is the gate that keeps the blank-slot prune from widening write
// authority; it must stay red if pruning ever starts letting a target through.
func TestBlankWriteTargetStillBlocksTheCard(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "帮我关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-blank-target", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-blank-target","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"","source":"user_explicit"}]}`), noopStep)

	var resolved actionresolver.ResolvedAction
	require.NoError(t, json.Unmarshal([]byte(out), &resolved))
	require.False(t, resolved.ReadyForConfirmation, "a blank target must never reach the confirmation card")
	require.False(t, resolved.ReadyForIntake, "stop declares no guided form; a blank target must not open one")
	require.Equal(t, []string{"UHostId"}, resolved.Missing, "unsaid, not invalid — but still refused")
	require.NotContains(t, resolved.Arguments, "UHostId")
}

// The create regression this prune fixes. The system prompt tells the Agent to
// submit as soon as the operation is clear and leave the rest blank, so terra
// emitted an empty Name; a blank optional field was adjudicated as an invalid
// VALUE, and any rejection blocks the guided form outright — so one empty Name
// suppressed the entire create card (0/20 on a live four-arm WS run) while the
// model narrated that a draft had been submitted.
func TestBlankOptionalSlotDoesNotSuppressTheCreateCard(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{map[string]any{"Name": "4090"}}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "创建一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-blank-name", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-blank-name", "operation": "CreateInstanceWorkflow",
		"slots": []any{
			map[string]any{"name": "GpuType", "value": "4090", "source": "user_explicit",
				"evidence": map[string]any{"quote": "4090"}},
			map[string]any{"name": "Name", "value": "", "source": "agent_inference"},
			map[string]any{"name": "CompShareImageId", "value": nil, "source": "agent_inference"},
		},
	})

	require.NoError(t, err)
	require.Empty(t, resolved.action.Rejected,
		"a blank slot is the Agent saying nothing, not a value the user got wrong")
	require.True(t, resolved.action.ReadyForConfirmation, "the create must still reach a card")
	require.NotContains(t, resolved.action.Arguments, "Name", "a blank name is dropped, never invented")
	require.NotContains(t, resolved.action.Arguments, "CompShareImageId")
}

// The prune is deliberately narrow: only JSON null and whitespace-only strings.
// Zero, false and empty collections are real values for their codecs and must
// keep reaching adjudication — silently dropping a 0 would turn "no GPUs" into
// "unspecified" and let a default fill it in.
func TestPruneBlankSlotsKeepsMeaningfulZeroValues(t *testing.T) {
	kept := pruneBlankSlots([]actionresolver.SlotCandidate{
		{Name: "Null", Value: nil},
		{Name: "Empty", Value: ""},
		{Name: "Whitespace", Value: "  \t "},
		{Name: "Zero", Value: float64(0)},
		{Name: "False", Value: false},
		{Name: "EmptyList", Value: []any{}},
		{Name: "Text", Value: "x"},
	})
	names := make([]string, 0, len(kept))
	for _, candidate := range kept {
		names = append(names, candidate.Name)
	}
	require.Equal(t, []string{"Zero", "False", "EmptyList", "Text"}, names)
}

func TestResolvedProposalDisposition(t *testing.T) {
	cases := []struct {
		name   string
		a      actionresolver.ResolvedAction
		guided bool
		want   string
	}{
		{"confirmation", actionresolver.ResolvedAction{ReadyForConfirmation: true}, true, "confirmation"},
		{"confirmation_wins_over_intake", actionresolver.ResolvedAction{ReadyForConfirmation: true, ReadyForIntake: true}, true, "confirmation"},
		{"intake_form", actionresolver.ResolvedAction{ReadyForIntake: true}, true, "intake_form"},
		{"intake_form_unavailable", actionresolver.ResolvedAction{ReadyForIntake: true}, false, "intake_form_unavailable"},
		{"dependency_failure_wins_over_reject", actionresolver.ResolvedAction{
			DependencyFailures: []string{"zone_catalog"},
			RejectedProblems:   []actionresolver.RejectedProblem{{Slot: "Zone", Kind: actionresolver.RejectInvalidValue}},
		}, true, "dependency_failure"},
		{"rejected_invalid_zone_no_value_leak", actionresolver.ResolvedAction{
			// The human-readable Rejected string carries a value; the disposition must
			// NOT — it reads the typed twin (slot + kind) only.
			Rejected:         []string{"Zone: 华北九九九 is not a live zone"},
			RejectedProblems: []actionresolver.RejectedProblem{{Slot: "Zone", Kind: actionresolver.RejectInvalidValue}},
		}, true, "rejected:Zone=invalid_value"},
		{"rejected_sorted_and_deduped", actionresolver.ResolvedAction{
			RejectedProblems: []actionresolver.RejectedProblem{
				{Slot: "Zone", Kind: actionresolver.RejectInvalidValue},
				{Slot: "Cpu", Kind: actionresolver.RejectInvalidValue},
				{Slot: "Zone", Kind: actionresolver.RejectInvalidValue}, // dup
			},
		}, true, "rejected:Cpu=invalid_value,Zone=invalid_value"},
		{"rejected_op_level", actionresolver.ResolvedAction{
			RejectedProblems: []actionresolver.RejectedProblem{{Slot: "", Kind: actionresolver.RejectOperationContract}},
		}, true, "rejected:_op=operation_contract"},
		{"rejected_untyped_defensive", actionresolver.ResolvedAction{Rejected: []string{"something"}}, true, "rejected"},
		{"conflict", actionresolver.ResolvedAction{Conflicts: []actionresolver.Conflict{{Slot: "UHostId"}}}, true, "conflict:UHostId"},
		{"missing", actionresolver.ResolvedAction{Missing: []string{"GpuType"}}, true, "missing:GpuType"},
		{"unresolved", actionresolver.ResolvedAction{}, true, "unresolved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvedProposalDisposition(tc.a, tc.guided)
			if got != tc.want {
				t.Errorf("disposition = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "华北九九九") {
				t.Errorf("disposition leaked a slot value: %q", got)
			}
		})
	}
}
