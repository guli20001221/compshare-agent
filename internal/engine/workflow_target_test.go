package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowRequiresInstanceTarget(t *testing.T) {
	for _, action := range []string{
		"CreateInstanceWorkflow",
		"CloneCustomImageWorkflow",
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
		"UpdateInstancePortsWorkflow",
	} {
		assert.True(t, workflowRequiresInstanceTarget(action), action)
	}
}

// Write targets are existence-verified before the operation's confirmation card.
// The Agent resolves conversation references and supplies the exact target.

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

// Under the uniform model, a concrete target the Agent proposes is existence-verified
// and — if it exists — reaches the confirmation card, whether or not the user
// referenced it deterministically. The card is the SelectionProof: the human confirms
// the exact id shown. (Whether the Agent SHOULD propose a stop for an info question
// like "怎么关机" is a P7 behavioral concern — the Agent ought to answer, not tool-call —
// not the target-authorization layer's job; nothing executes without the confirm.)
func TestInferredExistentTargetReachesConfirmationCard(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "怎么关机"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-unselected", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-unselected", "uhost-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-a", resolved.action.Arguments["UHostId"],
		"an existent inferred target reaches the card; the user's confirm is the SelectionProof")
}

// An OBSERVED referent (recorded from a read) is no longer refused: with a genuine
// human confirm gate, the Agent proposing the observed instance's id is existence-
// verified and reaches the card. "Observed vs chosen" collapses at the card — the
// distinction that mattered when card DISPLAY was treated as authorization is moot now
// that the user's confirm is what authorizes.
func TestObservedReferentTargetReachesConfirmationCard(t *testing.T) {
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

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-observed-pronoun", "uhost-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-a", resolved.action.Arguments["UHostId"])
}

// Positive control — an explicit id the user typed this turn is a SelectionProof,
// and a registry hit is its ExistenceProof: the write is authorized.
func TestProposalAuthorizesExplicitIDTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "帮我关机 uhost-a"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-explicit", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-explicit", "uhost-a"))

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
	})
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, userMsg, turnID, time.Now())
	eng.turnContextViewReady = true
}

// Positive control — an ordinal against the displayed list, with the model
// submitting the matching id, authorizes that exact instance.
func TestCorrectOrdinalAuthorizesChosenCandidate(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	primeOrdinalSelection(t, eng, "帮我关机第2台", "turn-ord-right")

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-ord-right", "uhost-b"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-b", resolved.action.Arguments["UHostId"])
}

// The Agent maps the name to an ID before calling the write tool.
func TestAgentResolvesAnInstanceNameBeforeProposingTheWrite(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关闭 alpha"
	syncTwoInstances(t, eng)
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-name", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-name", "uhost-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-a", resolved.action.Arguments["UHostId"],
		"the Agent's concrete ID is preserved for existence checking and confirmation")
}

func TestAnotherTasksExplicitIDDoesNotReplaceTheProposedTarget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent, SelectedInstanceID: "uhost-b",
		SelectedInstanceSource: SelectedInstanceSourceUser, SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	syncTwoInstances(t, eng)
	eng.lastUserMsg = "查询 uhost-a 的状态，然后关闭刚才那台"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-two-tasks", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-two-tasks", "uhost-b"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "uhost-b", resolved.action.Arguments["UHostId"],
		"the read task's explicit ID must not overwrite the write task's model-resolved target")
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

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-depfail", "uhost-1"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation)
	require.NotEmpty(t, resolved.action.DependencyFailures,
		"an existence-check outage is a dependency failure, not a rejected target")
	require.Empty(t, resolved.action.Rejected)
}

// --- Cross-resource-kind existence verification (review finding 3) ---
// A write target is verified by the ExactTargetVerifier for its KIND, never a single
// instance verifier. A CFS is verified against DescribeCFS echoing the same CfsId; a
// disk against its already-verified parent instance's DiskSet — both BEFORE the card.

func resizeCFSProposal(turnID, cfsID string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "operation": "ResizeCFSWorkflow",
		"slots": []any{
			map[string]any{"name": "CfsId", "value": cfsID},
			map[string]any{"name": "Size", "value": float64(200)},
		},
	}
}

// A CFS target is verified against DescribeCFS echoing the same CfsId — the instance
// registry never carries a CfsId, so the pre-fix instance verifier could only ever
// (falsely) refuse it. A DescribeCFS response echoing the id cards the resize.
func TestResizeCFSTargetVerifiedByDescribeCFS(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCFS" {
			return map[string]any{"CFSSet": []any{map[string]any{"CfsId": "cfs-a", "Name": "shared", "Size": float64(100)}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.lastUserMsg = "把 cfs-a 扩到 200G"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cfs", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), resizeCFSProposal("turn-cfs", "cfs-a"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "cfs-a", resolved.action.Arguments["CfsId"])
	require.Contains(t, exec.calls, "DescribeCFS", "a CFS target must be verified via DescribeCFS")
	require.NotContains(t, exec.calls, "DescribeCompShareInstance", "a CFS target must not be verified against the instance registry")
}

// A DescribeCFS response that does NOT echo the requested CfsId is not existence.
func TestResizeCFSRefusedWhenDescribeCFSDoesNotEcho(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCFS" {
			return map[string]any{"CFSSet": []any{map[string]any{"CfsId": "cfs-other"}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.lastUserMsg = "把 cfs-a 扩到 200G"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cfs-miss", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), resizeCFSProposal("turn-cfs-miss", "cfs-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "a DescribeCFS response that does not echo the id is not existence")
}

// A failed DescribeCFS is a dependency failure (our outage), never a false NotFound.
func TestResizeCFSDependencyFailureOnDescribeCFSError(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCFS" {
			return nil, fmt.Errorf("upstream 503")
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.lastUserMsg = "把 cfs-a 扩到 200G"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-cfs-fail", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), resizeCFSProposal("turn-cfs-fail", "cfs-a"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation)
	require.NotEmpty(t, resolved.action.DependencyFailures, "a DescribeCFS outage is a dependency failure, not a rejected target")
	require.Empty(t, resolved.action.Rejected)
}

func resizeDiskProposal(turnID, uHostID, diskID string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "operation": "ResizeDiskWorkflow",
		"slots": []any{
			map[string]any{"name": "UHostId", "value": uHostID},
			map[string]any{"name": "DiskId", "value": diskID},
			map[string]any{"name": "Size", "value": float64(120)},
		},
	}
}

// A disk has no standalone describe; existence = the exact disk id is present in its
// (already-verified) parent instance's DiskSet, checked HERE before the card — not
// deferred to the workflow's execution stage (which would be a second verifier).
func TestResizeDiskTargetVerifiedAgainstParentDiskSet(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-a", "Name": "alpha", "State": "Running",
				"DiskSet": []any{map[string]any{"DiskId": "disk-1", "Size": float64(60)}},
			}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.lastUserMsg = "把 uhost-a 的 disk-1 扩到 120G"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-disk", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), resizeDiskProposal("turn-disk", "uhost-a", "disk-1"))

	require.NoError(t, err)
	require.True(t, resolved.action.ReadyForConfirmation, resolved.action.Rejected)
	require.Equal(t, "disk-1", resolved.action.Arguments["DiskId"])
}

// A disk id absent from the parent instance's DiskSet is refused before the card.
func TestResizeDiskRefusedWhenDiskNotInParentDiskSet(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-a", "Name": "alpha", "State": "Running",
				"DiskSet": []any{map[string]any{"DiskId": "disk-other"}},
			}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.lastUserMsg = "把 uhost-a 的 disk-1 扩到 120G"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-disk-miss", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), resizeDiskProposal("turn-disk-miss", "uhost-a", "disk-1"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation, "a disk id absent from the parent's DiskSet is not existence")
}

// An instance existence proof must NOT authorize a disk that happens to share the same
// id STRING (review round-2 finding 1): evidence is keyed by (field, kind, id), never
// the bare value. Here an instance "same-id" exists but its DiskSet lacks "same-id", so
// the disk resize is refused — the instance's Verified verdict is never reused.
func TestSameIdInstanceEvidenceDoesNotAuthorizeDisk(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareInstance" {
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "same-id", "Name": "alpha", "State": "Running",
				"DiskSet": []any{map[string]any{"DiskId": "disk-other"}},
			}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.lastUserMsg = "扩盘"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-same-id", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), resizeDiskProposal("turn-same-id", "same-id", "same-id"))

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation,
		"an instance proof must not authorize a same-id disk that is absent from the DiskSet")
}

// An unknown target kind has no verifier and must be refused, never silently verified
// as an instance (which would wrongly accept any existing instance id).
func TestUnknownTargetKindIsRefusedNotVerifiedAsInstance(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	ev := eng.verifyTargetExistence(context.Background(), "network", "net-1", "")

	require.Equal(t, entity.ExistenceNotFound, ev.Verdict, "an unknown kind cannot be confirmed to exist")
	require.False(t, ev.confirmed())
}

// A missing tool argument stays missing; context is supplied to the Agent,
// not silently converted into execution parameters.
func TestProposalDoesNotInventAMissingTargetFromSoleInstance(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.lastUserMsg = "关机"
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet":   []any{map[string]any{"UHostId": "uhost-1", "Name": "solo", "State": "Running"}},
	}, "test"))
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-solo", time.Now())
	eng.turnContextViewReady = true

	resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
		"turn_id": "turn-solo", "operation": "StopInstanceWorkflow", "slots": []any{},
	})

	require.NoError(t, err)
	require.False(t, resolved.action.ReadyForConfirmation)
	require.Empty(t, resolved.action.Arguments["UHostId"])
	require.Contains(t, resolved.action.Missing, "UHostId",
		"the Agent must supply the target, even when the account has one instance")
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

	resolved, err := eng.resolveActionProposal(context.Background(), map[string]any{
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

	resolved, err := eng.resolveActionProposal(context.Background(), stopInstanceProposal("turn-explicit-other", "uhost-x"))

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
	_ = eng.executeResolvedWorkflow(zoneUserCtx(), mustConfirmable("CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-bj2-03",
	}, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx()))), noopStep)

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

	reply := eng.executeResolvedWorkflow(zoneUserCtx(), mustConfirmable("CreateCFSWorkflow", map[string]any{
		"Name": "shared-train",
		"Size": float64(50),
		"Zone": "cn-wlcb-01",
	}, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx()))), noopStep)

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

	_ = eng.executeResolvedWorkflow(zoneUserCtx(), mustConfirmable("EnableNetOptimizerWorkflow", map[string]any{
		"Zone": "cn-bj2-03",
	}, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx()))), noopStep)

	require.NotNil(t, syncArgs)
	assert.Equal(t, uint32(3003), syncArgs["az_group"])
	assert.Equal(t, uint32(1), syncArgs["top_organization_id"])
	assert.Equal(t, uint32(2), syncArgs["organization_id"])
}
