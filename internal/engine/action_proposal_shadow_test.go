package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
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

func TestCentralAgentProposalCannotExecuteWithoutVerifiedSource(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool {
		t.Fatal("unverified proposal must not reach confirmation")
		return true
	})
	eng.SetMutatingToolsEnabled(true)
	eng.lastUserMsg = "关机"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-unverified", time.Now())
	eng.turnContextViewReady = true

	out := eng.executeTool(context.Background(), toolCall("proposal", tools.ProposeActionName,
		`{"turn_id":"turn-unverified","operation":"StopInstanceWorkflow","slots":[{"name":"UHostId","value":"uhost-invented","source":"agent_inference"}]}`), noopStep)

	require.Contains(t, out, "not verified")
	require.Empty(t, executor.calls)
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

// TestCurrentTurnReadIsExistenceNotSelection: a current-turn read proves the
// target EXISTS, but existence is not selection. An id the Agent merely read —
// the user only said "停止它", never chose this instance — must not authorize a
// write on read-existence alone (dual-proof acceptance #3). Contrast with a
// genuine selection (typed id / card pick / sole account instance).
func TestCurrentTurnReadIsExistenceNotSelection(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "停止它"
	eng.readCapabilitySubjectsThisTurn = map[string]struct{}{"uhost-1": {}}
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-read-only", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-read-only", "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": "uhost-1"}},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "read existence alone is not user selection")
	require.Contains(t, resolved.action.Rejected, "UHostId: target source is not verified")
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

// TestObservedSelectedInstanceIsNotSelectionProof: a SelectedInstanceID recorded
// from a read (observed) — even fresh and existing — is NOT a selection, so a
// bare "确认关机" that would inherit it is refused. This is the new acceptance:
// a read-written SelectedInstanceID must never become a SelectionProof.
func TestObservedSelectedInstanceIsNotSelectionProof(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	// Two instances, so the account-single selection does not apply.
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
	require.False(t, resolved.action.ReadyForConfirmation, "an observed referent is not a user selection")
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
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": 0, "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running"}}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(true)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{"UHostId": "uhost-1", "Name": "host", "State": "Running"}}}, "test"))
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
		"UHostId": "uhost-1", "Password": "secret",
	})
	require.True(t, ok)
	require.Contains(t, withPassword, "刚设置")
	require.NotContains(t, withPassword, "secret")
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

func TestProposalRejectsDifferentTurnEvidence(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, "停止 uhost-1", "active-turn", time.Now())
	eng.turnContextViewReady = true
	_, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{"turn_id": "old-turn", "operation": "StopInstanceWorkflow", "slots": []any{}})
	require.ErrorContains(t, err, "does not match")
}

func TestCentralAgentProposalSchemaComesFromWorkflowCatalog(t *testing.T) {
	window := centralAgentToolWindow(true)
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
	window := centralAgentToolWindow(false)
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
	eng.readCapabilitySubjectsThisTurn = map[string]struct{}{"uhost-1": {}}
	// The user names the instance (span SelectionProof); the this-turn read supplies
	// the ExistenceProof — a valid target, so the sealed-password path runs.
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
