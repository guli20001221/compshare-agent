package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortageExecutor answers the capacity gate with a sold-out verdict, which is
// how a real 库存不足 arises: a SUCCESSFUL upstream call whose body says the spec
// is not available. It is therefore always reported before any create is
// authorised — the fact the whole failure record turns on.
func shortageExecutor(zone string) *mockExecutor {
	executor := draftMockExecutor(zone)
	executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
	}}
	return executor
}

// TestAPlainCreateRecordsTheZoneItResolved is the bug in one assertion.
//
// The user asked for a 4090 and named no zone. The workflow resolved cn-sh2-02
// from the catalog, asked capacity about cn-sh2-02, and was told it is sold out.
// Every part of that happened inside the workflow, so params — the user's own
// request — never contained a zone, and the caller that read GpuType/Zone out of
// them found no zone and searched for alternatives across EVERY zone. It then
// offered cards that may not exist where the user is actually buying.
//
// The draft knew. Nobody asked it. That is what the record is for.
func TestAPlainCreateRecordsTheZoneItResolved(t *testing.T) {
	executor := shortageExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Message, "库存不足")

	// The premise: the user named no zone, so params cannot answer.
	require.NotContains(t, map[string]any{"GpuType": "4090"}, "Zone")

	require.NotNil(t, result.Failure, "a failed workflow must describe its own failure")
	assert.Equal(t, "检查库存", result.Failure.Step)

	draft, err := ParseCreateExecutionDraft(result.Failure.Draft)
	require.NoError(t, err, "the record must carry the candidate the failed step used")
	assert.Equal(t, "cn-sh2-02", draft.Args.Zone,
		"the zone the resolver derived — the one capacity was actually asked about")
	assert.Equal(t, "4090", draft.Args.GpuType)

	assert.False(t, result.Failure.Sealed,
		"a sold-out is the capacity gate's verdict, and that gate runs before every "+
			"confirmation — nothing can have been authorised yet")
}

// TestAGuidedCreateDoesNotCallASelectionCardAContract is the other half, and the
// more dangerous one: here a contract DOES exist and means nothing like what a
// reader would assume.
//
// The guided flow seals after each of its seven gates. Stopping at 检查库存 leaves
// the 选择镜像 seal in force — a real contract, with Operation
// "CreateInstanceWorkflow", indistinguishable by inspection from one that
// authorised a create. Reading "Contract != nil" as "the user confirmed this"
// promotes a selection card to consent to spend money.
func TestAGuidedCreateDoesNotCallASelectionCardAContract(t *testing.T) {
	executor := shortageExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Message, "库存不足")

	// The trap, made explicit: a contract exists, and it looks exactly like a real
	// one. Only its provenance differs, and provenance is not visible on it.
	require.NotNil(t, result.Contract, "the guided flow really has sealed something by now")
	require.Equal(t, "CreateInstanceWorkflow", result.Contract.Operation,
		"…and it carries the same Operation a genuine create authorisation would")
	require.NotContains(t, result.Contract.BusinessParams, createDraftKey,
		"but it is a selection card's seal: no promoted draft, because no create was confirmed")

	require.NotNil(t, result.Failure)
	assert.False(t, result.Failure.Sealed,
		"gates remain ahead, so nothing sealed so far is permission to create")

	draft, err := ParseCreateExecutionDraft(result.Failure.Draft)
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", draft.Args.Zone)
	assert.Equal(t, "4090", draft.Args.GpuType)
}

// TestTheFailureRecordSeparatesTheRequestFromTheDecision pins why Args and Draft
// are two fields rather than one.
//
// A pod zone's capacity request has no Zone: ApplyCapacityPlacementArgs strips
// Zone/Region/az_group and sends the internal ids instead. So the very call that
// reports the shortage cannot say where it looked, while the draft behind it can.
// Read Args for what was asked; read Draft for what was decided.
func TestTheFailureRecordSeparatesTheRequestFromTheDecision(t *testing.T) {
	executor := shortageExecutor("cn-sh2-02")
	// A pod zone only accepts a container image — validateSelectedImageCompatibility
	// would otherwise stop this at 形成执行草稿, well before the gate under test.
	executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{
		map[string]any{
			"CompShareImageId": "img-001",
			"Name":             "PyTorch 容器镜像",
			"Size":             float64(102400),
			"Container":        true,
		},
	}}
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{
		"GpuType":       "4090",
		"Zone":          "cn-sh2-02",
		"ZoneIsPods":    map[string]any{"cn-sh2-02": true},
		"ZoneIds":       map[string]any{"cn-sh2-02": float64(2002)},
		"ZoneRegionIds": map[string]any{"cn-sh2-02": float64(3002)},
	})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotNil(t, result.Failure)
	require.Equal(t, "检查库存", result.Failure.Step,
		"the premise: this must reach the capacity gate, not fail placement validation earlier")

	require.NotEmpty(t, result.Failure.Args, "the capacity step built args, so the record must carry them")
	assert.NotContains(t, result.Failure.Args, "Zone",
		"a pod zone's capacity request carries no Zone — this is why the reply cannot read the request")

	draft, err := ParseCreateExecutionDraft(result.Failure.Draft)
	require.NoError(t, err)
	assert.Equal(t, "cn-sh2-02", draft.Args.Zone,
		"…while the decision behind that request names the zone perfectly well")
}

// TestTheFailureRecordDoesNotAliasTheRequest: the record is evidence, and evidence
// that moves when the executor rewrites the map it was handed is not evidence.
// Same rule as the sealed draft's — see cloneDiskList.
func TestTheFailureRecordDoesNotAliasTheRequest(t *testing.T) {
	executor := shortageExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.NotNil(t, result.Failure)

	var sent map[string]any
	for _, c := range executor.calls {
		if c.action == "CheckCompShareResourceCapacity" {
			sent = c.args
		}
	}
	require.NotNil(t, sent)
	require.Equal(t, "4090", result.Failure.Args["GpuType"])

	sent["GpuType"] = "TAMPERED"

	assert.Equal(t, "4090", result.Failure.Args["GpuType"],
		"the record must not share structure with the map handed to the executor")
}

// TestASealedFailureSaysSo is the other side of Sealed: a failure AFTER the gate
// must report that a contract authorised it, or the field would be a constant
// false dressed up as a fact.
func TestASealedFailureSaysSo(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	executor.failOn = "CreateCompShareInstance"
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "创建实例", result.StoppedAt)

	require.NotNil(t, result.Failure)
	assert.Equal(t, "创建实例", result.Failure.Step)
	assert.True(t, result.Failure.Sealed,
		"this failure is past the confirm gate: a contract really did authorise it")
	require.NotNil(t, result.Contract)
}

// TestEveryFailureCarriesARecord: "not Success" and "Failure != nil" must mean the
// same thing, or a caller has to keep a nil check that silently reintroduces
// guessing from params on whichever path was forgotten.
func TestEveryFailureCarriesARecord(t *testing.T) {
	t.Run("resolve step failure", func(t *testing.T) {
		executor := draftMockExecutor("cn-sh2-02")
		// No image in the catalog: 形成执行草稿 fails before any draft exists.
		executor.results["DescribeCompShareImages"] = map[string]any{"ImageSet": []any{}}
		eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

		result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
		require.NoError(t, err)
		require.False(t, result.Success)
		require.NotNil(t, result.Failure, "a resolve failure is a failure")
		assert.Equal(t, result.StoppedAt, result.Failure.Step)
		assert.Nil(t, result.Failure.Draft,
			"no candidate was resolved, and saying so beats returning a half-built one")
		assert.False(t, result.Failure.Sealed)
	})

	t.Run("cancelled at the gate", func(t *testing.T) {
		executor := draftMockExecutor("cn-sh2-02")
		eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return false }, nil)

		result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
		require.NoError(t, err)
		require.False(t, result.Success)
		require.NotNil(t, result.Failure, "a declined confirmation is a failure with a record")
		assert.Equal(t, "确认创建", result.Failure.Step)
		assert.False(t, result.Failure.Sealed,
			"a gate that was declined authorised nothing")
	})
}

// TestASuccessfulWorkflowHasNoFailureRecord: the record must not become a field
// that is always populated and therefore says nothing.
func TestASuccessfulWorkflowHasNoFailureRecord(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Nil(t, result.Failure)
}
