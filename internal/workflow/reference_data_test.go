package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

func zoneCatalogFixture() *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, DisplayName: "华北一C"},
	})
}

func TestRun_WithReferenceData_ThreadsZoneCatalogToSteps(t *testing.T) {
	cat := zoneCatalogFixture()
	var sawCatalog bool
	var seen deployment.ZonePlacement

	def := &Definition{
		Name: "probe",
		Steps: []Step{{
			Name: "read", Type: StepToolCall, Tool: "Noop",
			BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				sawCatalog = wfCtx.ZoneCatalog().Available()
				seen, _ = wfCtx.ZoneCatalog().Placement("cn-bj2-03")
				return map[string]any{}, nil
			},
		}},
	}

	eng := NewEngine(&mockExecutor{}, nil, func(StepEvent) {})
	result, err := eng.Run(context.Background(), def, map[string]any{},
		WithReferenceData(ReferenceData{ZoneCatalog: cat}))

	require.NoError(t, err)
	require.True(t, result.Success)
	assert.True(t, sawCatalog, "a run's steps see the catalog attached via WithReferenceData")
	assert.Equal(t, uint32(6003), seen.ZoneID, "and resolve a placement from it")
	assert.True(t, seen.IsPod)
}

func TestRun_WithoutReferenceData_ZoneCatalogIsNilSafe(t *testing.T) {
	var available, resolved bool

	def := &Definition{
		Name: "probe",
		Steps: []Step{{
			Name: "read", Type: StepToolCall, Tool: "Noop",
			BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				// No option was passed: the accessor returns nil, and every method on
				// it must behave as "unavailable" rather than panic.
				available = wfCtx.ZoneCatalog().Available()
				_, resolved = wfCtx.ZoneCatalog().Placement("cn-bj2-03")
				return map[string]any{}, nil
			},
		}},
	}

	eng := NewEngine(&mockExecutor{}, nil, func(StepEvent) {})
	result, err := eng.Run(context.Background(), def, map[string]any{})

	require.NoError(t, err)
	require.True(t, result.Success)
	assert.False(t, available, "no reference data → nil catalog → unavailable, no panic")
	assert.False(t, resolved)
}

func TestRun_ReferenceDataPersistsAcrossSealButIsNeverSealed(t *testing.T) {
	cat := zoneCatalogFixture()
	var afterSeal *deployment.ZoneCatalogSnapshot
	confirmFn := func(string, map[string]any) bool { return true }

	def := &Definition{
		Name: "CreateInstanceWorkflow",
		Steps: []Step{
			{Name: "confirm", Type: StepConfirm, BuildArgs: func(*Context) (map[string]any, error) { return map[string]any{}, nil }},
			{Name: "read-after-seal", Type: StepToolCall, Tool: "Noop", BuildArgs: func(wfCtx *Context) (map[string]any, error) {
				afterSeal = wfCtx.ZoneCatalog()
				return map[string]any{}, nil
			}},
		},
	}

	eng := NewEngine(&mockExecutor{}, confirmFn, func(StepEvent) {})
	result, err := eng.Run(context.Background(), def, map[string]any{"GpuType": "4090"},
		WithReferenceData(ReferenceData{ZoneCatalog: cat}))

	require.NoError(t, err)
	require.True(t, result.Success)
	require.NotNil(t, result.Contract, "the confirm gate sealed a contract")

	// The same snapshot is still available to steps that run AFTER the seal, so a
	// confirm-form re-run reuses it and never re-fetches (gate #4).
	assert.True(t, afterSeal.Available())

	// But it is not IN the seal: the sealed business params are exactly what the
	// user confirmed, with no zone catalog smuggled in (gate #12).
	assert.NotContains(t, result.Contract.BusinessParams, "ZoneCatalog")
	assert.NotContains(t, result.Contract.BusinessParams, "ReferenceData")
	assert.Equal(t, "4090", result.Contract.BusinessParams["GpuType"])
}
