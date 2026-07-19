package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowRequiresInstanceTarget(t *testing.T) {
	for _, action := range []string{
		"CreateInstanceWorkflow",
		"EnableNetOptimizerWorkflow",
		"CreateCFSWorkflow",
		"ResizeCFSWorkflow",
	} {
		assert.False(t, workflowRequiresInstanceTarget(action), action)
	}

	for _, action := range []string{
		"StopInstanceWorkflow",
		"StartInstanceWorkflow",
		"RebootInstanceWorkflow",
		"ResizeDiskWorkflow",
		"CreateDiskWorkflow",
		"CreateCustomImageWorkflow",
	} {
		assert.True(t, workflowRequiresInstanceTarget(action), action)
	}
}

// Write-target authority no longer lives in a workflow-layer trust gate — the
// second authorization center (workflowTargetIsTrusted) was deleted in the P9
// cutover. It lives ONLY in the Resolver's dual proof: a target is authorized
// exactly when the server can independently show the user SELECTED it AND that it
// EXISTS in this account this turn. Every case below therefore enters through
// resolveActionProposalShadow — the real production path — and asserts the
// resolver's ReadyForConfirmation verdict. A refused target is never made ready,
// so it can never reach executeResolvedWorkflow.

// stopInstanceProposal is a minimal StopInstanceWorkflow proposal naming a target
// by id, with no source label — the server re-derives provenance, so the model's
// label is irrelevant to authority.
func stopInstanceProposal(turnID, uHostID string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "operation": "StopInstanceWorkflow",
		"slots": []any{map[string]any{"name": "UHostId", "value": uHostID}},
	}
}

func syncTwoInstances(t *testing.T, eng *Engine) {
	t.Helper()
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "beta", "State": "Running"},
		},
	}, "test"))
}

// An Agent-inferred target with NO deterministic binding and NO active same-id
// verification this turn (only a bare fresh-registry hit) is refused, not carded.
// Existence for an inferred id must be actively verified ("已核实") — a background
// registry hit is exactly how a model would self-elect an arbitrary existing id.
func TestProposalRejectsExistentButUnselectedTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "怎么关机"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-unselected", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-unselected", "uhost-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"an existent instance the model picked without a user reference or active verification must not be authorized")
}

// Read-only observation cannot authorize: a SelectedInstanceID recorded from a read
// (observed) is not a user selection and populates no same-id-verified evidence, so
// a pronoun ("关机它") that would inherit it is refused on a multi-instance account.
func TestProposalRejectsObservedSelectedInstanceTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceObserved,
		SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	eng.lastUserMsg = "帮我关机它"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-observed-pronoun", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-observed-pronoun", "uhost-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"a tool-observed selected instance is not a user selection, even under a pronoun")
}

// Positive control — an explicit id the user typed this turn is a SelectionProof,
// and a registry hit is its ExistenceProof: the write is authorized.
func TestProposalAuthorizesExplicitIDTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "帮我关机 uhost-a"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-explicit", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-explicit", "uhost-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
}

// primeOrdinalSelection sets up a two-candidate pending list ([1]uhost-a alpha,
// [2]uhost-b beta) plus a matching fresh registry, so ordinal / name references
// resolve deterministically.
func primeOrdinalSelection(t *testing.T, eng *Engine, userMsg, turnID string) {
	t.Helper()
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = userMsg
	eng.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "alpha", "Running"),
		testInstance("uhost-b", "beta", "Running"),
	}, intent.IntentResourceInfo, "我有哪些实例", 2, false)
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, userMsg, turnID, time.Now())
	eng.turnContextViewReady = true
}

// COUNTEREXAMPLE (the bug the old vacuous test hid): the user selects the 2nd
// candidate but the Agent submits the 1st candidate's id. The SelectionBinder binds
// the ordinal to the 2nd instance and OVERRIDES the model's id, so the wrong
// instance (uhost-a) is never the authorized target — the 2nd (uhost-b) is.
func TestOrdinalBindsToChosenCandidateNotTheSubmittedOne(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	primeOrdinalSelection(t, eng, "帮我关机第2台", "turn-ord-wrong")

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-ord-wrong", "uhost-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-b", resolved.action.Arguments["UHostId"],
		"第2台 binds to the 2nd candidate; the model's 第1台 id must not be operated")
	require.NotEqual(t, "uhost-a", resolved.action.Arguments["UHostId"])
}

// Positive control — an ordinal against the displayed list, with the model
// submitting the matching id, authorizes that exact instance.
func TestCorrectOrdinalAuthorizesChosenCandidate(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	primeOrdinalSelection(t, eng, "帮我关机第2台", "turn-ord-right")

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-ord-right", "uhost-b"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-b", resolved.action.Arguments["UHostId"])
}

// A user reference giving BOTH an id and an ordinal that point at DIFFERENT
// instances is a conflict the binder refuses to resolve — the agent must ask, never
// pick one.
func TestIdAndOrdinalConflictIsRefused(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	primeOrdinalSelection(t, eng, "停止 uhost-a 第2台", "turn-id-ord-conflict")

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-id-ord-conflict", "uhost-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"an id (uhost-a) and an ordinal (第2台 -> uhost-b) naming different instances must not authorize either")
}

// The same id+ordinal conflict must be caught when the LIVE registry is COLD — the
// default state of a rehydrated HTTP session, where the pending list is restored
// from the DB but the in-memory EntityRegistry is empty (FreshAndCompleteAt=false),
// also produced by a post-mutation invalidation or TTL staleness. The binder must
// resolve the typed id against the pending candidates' own snapshot, so the ordinal
// cannot silently win. (Regression pin for the audit finding.)
func TestIdAndOrdinalConflictRefusedOnColdRegistry(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "停止 uhost-a 第2台"
	eng.recordPendingInstanceSelection([]entity.InstanceSnapshot{
		testInstance("uhost-a", "alpha", "Running"),
		testInstance("uhost-b", "beta", "Running"),
	}, intent.IntentResourceInfo, "我有哪些实例", 2, false)
	// Registry deliberately NOT synced -> cold, so id resolution can only come from
	// the pending candidates' snapshot.
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cold-conflict", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-cold-conflict", "uhost-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"a cold registry must not let the ordinal silently win over a contradicting typed id")
	require.NotEmpty(t, resolved.action.Conflicts, "the two-reference disagreement must surface as a conflict to ask about")
}

// A unique exact instance NAME in the message binds to its id — even when the model
// submitted the name itself, the binder canonicalizes it to the id and authorizes.
func TestUniqueExactNameBindsAndAuthorizes(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关闭 alpha"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-name", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-name", "alpha"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-a", resolved.action.Arguments["UHostId"],
		"the exact name alpha uniquely resolves to uhost-a")
}

// A DUPLICATE name (two instances both named "alpha") cannot bind to one id — the
// binder reports a conflict and the write is refused until the user disambiguates.
func TestDuplicateNameIsRefused(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关闭 alpha"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "alpha", "State": "Running"},
			map[string]any{"UHostId": "uhost-c", "Name": "alpha", "State": "Running"},
		},
	}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-dupname", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-dupname", "uhost-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"a duplicate instance name must be disambiguated, never bound to one id")
}

// A point-query that FAILS (upstream error) is a DependencyFailure — our outage,
// not the user's target being invalid. The registry is cold and the user typed an
// exact id (a SelectionProof, so the point-query runs), but the query errors: the
// target must land in DependencyFailures, never a plain rejection.
func TestPointQueryFailureIsDependencyFailure(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return nil, fmt.Errorf("upstream 503")
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "停止 uhost-1"
	// registry never synced -> cold, so a typed id is point-queried.
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-depfail", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-depfail", "uhost-1"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation)
	require.NotEmpty(t, resolved.action.DependencyFailures,
		"an existence-check outage is a dependency failure, not a rejected target")
	require.Empty(t, resolved.action.Rejected)
}

// Single-instance completion is subject to registry completeness (I): the sole
// fresh instance in a complete registry is auto-filled from context and
// authorized even when the user names no id ("关机").
func TestProposalCompletesSoleFreshInstanceTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关机"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet":   []any{map[string]any{"UHostId": "uhost-1", "Name": "solo", "State": "Running"}},
	}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-solo", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-solo", "operation": "StopInstanceWorkflow", "slots": []any{},
	})

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-1", resolved.action.Arguments["UHostId"],
		"the account's sole fresh instance is completed from context, not guessed")
}

// Single-instance completion is subject to registry completeness (II): an
// ambiguous (multi-instance) account never auto-fills a bare "关机"; the resolver
// leaves the target missing rather than pick one.
func TestProposalDoesNotCompleteTargetOnAmbiguousAccount(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关机"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-ambiguous", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), map[string]any{
		"turn_id": "turn-ambiguous", "operation": "StopInstanceWorkflow", "slots": []any{},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"a multi-instance account must not silently complete a bare stop to one host")
}

// Single-instance completion is subject to registry completeness (III): the sole
// instance never OVERRIDES a different id the user explicitly typed. A user-typed
// uhost-x that does not exist is refused (absence is authoritative), and it is
// never swapped for the account's only host.
func TestProposalSoleInstanceDoesNotOverrideExplicitDifferentTarget(t *testing.T) {
	describeCalls := 0
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			describeCalls++
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关闭 uhost-x"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet":   []any{map[string]any{"UHostId": "uhost-1", "Name": "solo", "State": "Running"}},
	}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-explicit-other", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposalShadow(context.Background(), stopInstanceProposal("turn-explicit-other", "uhost-x"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"a user-typed id absent from a complete registry is refused, not swapped for the sole host")
	require.NotEqual(t, "uhost-1", resolved.action.Arguments["UHostId"],
		"the account's only instance must never override the explicit target the user named")
	require.Zero(t, describeCalls,
		"a complete registry can assert absence, so no point-query is spent on the phantom id")
}

func TestExecuteWorkflowCreateCFSResolvesPodZone(t *testing.T) {
	var priceArgs map[string]any
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "GetCompShareCFSPrice":
			priceArgs = args
			return map[string]any{
				"PriceDetails": []any{
					map[string]any{"ChargeType": "Month", "Disks": float64(99)},
				},
			}, nil
		case "CreateCFS":
			createArgs = args
			return map[string]any{"CfsId": "cfs-new"}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "原始文本中的可用区不得参与执行"

	// Zone is already the canonical id: display-name → id resolution now lives in the
	// action resolver's CodecZone (tested in internal/actionresolver), not in the
	// workflow. executeResolvedWorkflow receives a canonical zone and resolves its
	// Pod-zone internal ids (zone_id/az_group) from the turn's catalog snapshot.
	_ = eng.executeResolvedWorkflow(zoneUserCtx(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-bj2-03",
	}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx())))

	require.NotNil(t, priceArgs)
	require.NotNil(t, createArgs)
	assert.Equal(t, uint32(5001), priceArgs["zone_id"])
	assert.Equal(t, uint32(5001), createArgs["zone_id"])
	assert.Equal(t, uint32(3003), priceArgs["az_group"])
	assert.Equal(t, uint32(3003), createArgs["az_group"])
}

func TestExecuteWorkflowCreateCFSRejectsNonPodZoneDeterministically(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我在 cn-wlcb-01 创建一个 50GB 的 CFS"

	reply := eng.executeResolvedWorkflow(zoneUserCtx(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-wlcb-01",
	}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx())))

	assert.Contains(t, reply, "CFS 创建没有成功")
	assert.Contains(t, reply, "只支持 Pod")
	assert.NotContains(t, reply, "cn-pod-01")
}

func TestExecuteWorkflowEnableNetOptimizerResolvesAzGroup(t *testing.T) {
	var syncArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "CheckCompShareNetOptimizer":
			return map[string]any{"Optimized": false}, nil
		case "SyncCompShareNetOptimizer":
			syncArgs = args
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "帮我开启华北一C网络加速"

	_ = eng.executeResolvedWorkflow(zoneUserCtx(), "EnableNetOptimizerWorkflow", map[string]any{
		"Zone": "cn-bj2-03",
	}, noopStep, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx())))

	require.NotNil(t, syncArgs)
	assert.Equal(t, uint32(3003), syncArgs["az_group"])
	assert.Equal(t, uint32(1), syncArgs["top_organization_id"])
	assert.Equal(t, uint32(2), syncArgs["organization_id"])
}
