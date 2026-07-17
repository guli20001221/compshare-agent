package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/zones"
)

// TestResolveActionProposal_CarriesZoneSnapshotForZoneOpsOnly pins S5c step 2:
// the action-proposal resolver builds the turn's zone catalog for an operation
// that carries a zone field and hands it back in referenceData for the workflow
// to reuse — and fetches nothing for an operation with no zone field.
func TestResolveActionProposal_CarriesZoneSnapshotForZoneOpsOnly(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "")
	eng.lastUserMsg = "开一台 4090"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-b", time.Now())
	eng.turnContextViewReady = true

	create, err := eng.resolveActionProposalShadow(zoneUserCtx(), map[string]any{
		"turn_id": "turn-b", "operation": "CreateInstanceWorkflow", "slots": []any{},
	})
	require.NoError(t, err)
	require.NotNil(t, create.referenceData.ZoneCatalog, "a zone op must carry the resolver's snapshot for the workflow to reuse")
	assert.True(t, create.referenceData.ZoneCatalog.Available(), "the carried snapshot is built from the live catalog")

	stop, err := eng.resolveActionProposalShadow(zoneUserCtx(), map[string]any{
		"turn_id": "turn-b", "operation": "StopInstanceWorkflow", "slots": []any{},
	})
	require.NoError(t, err)
	assert.Nil(t, stop.referenceData.ZoneCatalog, "a non-zone op must not fetch or carry a zone catalog")
}

func s5cCreateExecutor() *createFlowExecutor {
	return &createFlowExecutor{
		images:    []any{map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch", "ImageType": "App"}},
		available: []any{availableGPU("4090", 24)},
	}
}

// TestExecuteWorkflow_RunsAgainstThreadedSnapshotNotASelfBuild pins S5c step 3 /
// gate 1 (one snapshot per turn): when a snapshot is threaded in, executeWorkflow
// runs against THAT one and never self-builds a second. The counter-example makes
// it unforgeable — an UNAVAILABLE snapshot is threaded while the executor WOULD
// serve a healthy catalog if consulted; the create must refuse on the threaded
// snapshot, before the confirmation gate, proving the self-build was suppressed.
func TestExecuteWorkflow_RunsAgainstThreadedSnapshotNotASelfBuild(t *testing.T) {
	exec := s5cCreateExecutor()
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { confirmCalls++; return true })
	eng.zoneCatalog = zones.NewCatalog(0)

	reply := eng.executeWorkflow(zoneUserCtx(), "CreateInstanceWorkflow",
		map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, noopStep,
		withPrebuiltZoneCatalog(deployment.NewZoneCatalogSnapshot(false, nil)))

	assert.Contains(t, reply, "可用区目录当前不可用",
		"executeWorkflow must run against the THREADED (unavailable) snapshot, not self-build a healthy one")
	assert.Equal(t, 0, confirmCalls, "the create must refuse before the confirmation gate on the threaded snapshot")
}

// TestExecuteWorkflow_SelfBuildsWhenNoSnapshotThreaded pins the other half: a
// direct four-argument caller (no threaded snapshot) still self-builds its
// catalog, so those callers keep working. Here the healthy executor's catalog IS
// consulted, so the create reaches the confirmation gate rather than refusing.
func TestExecuteWorkflow_SelfBuildsWhenNoSnapshotThreaded(t *testing.T) {
	exec := s5cCreateExecutor()
	reachedConfirm := false
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { reachedConfirm = true; return false })
	eng.zoneCatalog = zones.NewCatalog(0)

	_ = eng.executeWorkflow(zoneUserCtx(), "CreateInstanceWorkflow",
		map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, noopStep)

	assert.True(t, reachedConfirm,
		"with no threaded snapshot executeWorkflow must self-build a catalog and reach the confirm gate")
}
