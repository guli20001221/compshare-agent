package workflow

import (
	"context"
	"fmt"
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

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
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

	assert.False(t, result.Failure.ExecutionAuthorized,
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

	result, err := eng.runCreateTest(CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
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
	assert.False(t, result.Failure.ExecutionAuthorized,
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

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-sh2-02",
	}, withPodZone("cn-sh2-02", "cn-sh2", 2002, 3002))
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

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
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

// TestOnlyTheSoldOutBranchClassifiesTheFailure pins that the reason is set on the
// one capacity rejection that means it, and on no other.
//
// The capacity gate rejects three ways: the spec is sold out, the spec does not
// exist in the returned combinations, and the response is empty. Only the first is
// capacity_sold_out — the other two are a user who needs their configuration
// corrected, not substituted, and classifying them the same would offer
// alternatives to a question nobody asked. The old substring on "库存不足" happened
// to separate them; a reason declared per-branch is why it still does.
func TestOnlyTheSoldOutBranchClassifiesTheFailure(t *testing.T) {
	t.Run("sold out -> classified", func(t *testing.T) {
		executor := shortageExecutor("cn-sh2-02") // ResourceEnough:false for the matched spec
		eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

		result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
		require.NoError(t, err)
		require.NotNil(t, result.Failure)
		assert.Equal(t, ReasonCapacitySoldOut, result.Failure.Reason,
			"a matched spec upstream has no stock is the one branch alternatives answer")
	})

	t.Run("spec not found -> NOT classified", func(t *testing.T) {
		executor := draftMockExecutor("cn-sh2-02")
		// A spec the draft will not match: the gate takes its "not found" branch,
		// which is a configuration problem, not a shortage.
		executor.results["CheckCompShareResourceCapacity"] = map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(8), "Cpu": float64(128), "Mem": float64(512), "ResourceEnough": true},
		}}
		eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

		result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
		require.NoError(t, err)
		require.NotNil(t, result.Failure)
		require.Contains(t, result.Message, "未找到", "premise: this is the not-found branch, not sold-out")
		assert.Empty(t, result.Failure.Reason,
			"a spec that does not exist is not a shortage — offering substitutes would answer the wrong question")
	})
}

// TestACancelledWorkflowStillDescribesItsFailure closes the hole that made the
// record optional.
//
// A cancellation returned "not Success" with no record at all, and a guided run
// cancelled after its first selection card ends holding a contract. A caller then
// had one fact (a contract exists) and no answer to the question that qualifies it
// — which is precisely the state this record was built to remove. Silence is not
// "no": a reader has to be told no.
func TestACancelledWorkflowStillDescribesItsFailure(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	ctx, cancel := context.WithCancel(context.Background())
	confirms := 0
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool {
		confirms++
		if confirms == 1 {
			cancel() // the user walks away right after the first selection card
		}
		return true
	}, nil)

	result, err := eng.Run(ctx, CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Message, "已取消")

	// The trap: a real contract survives the cancellation.
	require.NotNil(t, result.Contract, "the first selection card sealed before the cancel")

	require.NotNil(t, result.Failure, "a cancelled workflow is a failed workflow and needs a record")
	assert.False(t, result.Failure.ExecutionAuthorized,
		"the user walked away six gates before authorising a create")
}

// TestAFailingFinalGateAuthorisesNothing: the failed step is a gate, so that gate
// did not pass — and a run that never got through its final confirmation has
// authorised nothing, whatever earlier cards left sealed.
//
// runConfirmStep unseals on entry, which hides this for a gate that fails inside
// itself. A SkipIf error is raised by the Run loop BEFORE runConfirmStep is
// called, so the previous card's seal is still live at that moment. Scanning for
// remaining gates from i+1 then skipped the very gate that had just failed and
// called a selection card a create authorisation.
func TestAFailingFinalGateAuthorisesNothing(t *testing.T) {
	def := &Definition{
		Name: "CreateInstanceWorkflow",
		Steps: []Step{
			{Name: "选择镜像", Type: StepConfirm, BuildArgs: func(*Context) (map[string]any, error) {
				return map[string]any{"ImageId": "img-001"}, nil
			}},
			{
				Name:      "确认创建",
				Type:      StepConfirm,
				SkipIf:    func(*Context) (bool, error) { return false, fmt.Errorf("目录读取失败") },
				BuildArgs: func(*Context) (map[string]any, error) { return map[string]any{}, nil },
			},
		},
	}
	eng := NewEngine(&mockExecutor{results: map[string]map[string]any{}},
		func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(def, map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "确认创建", result.StoppedAt)

	require.NotNil(t, result.Contract, "选择镜像 sealed, and nothing unsealed it — that is the trap")
	require.NotNil(t, result.Failure)
	assert.False(t, result.Failure.ExecutionAuthorized,
		"确认创建 never even ran: the seal in force is 选择镜像's, and it authorises no create")
}

// TestTheFailureDraftDoesNotAliasWhatTheHookReturned: the record is evidence, and
// the obvious FailureDraft implementation hands back the definition's own live
// state — the create's returns wfCtx.Result(...) verbatim, which IS the workflow's
// StepResults entry. So the copy is taken where the record is built. Every hook is
// then safe by construction rather than each having to remember, the same way Args
// is copied rather than trusted from runToolStep.
func TestTheFailureDraftDoesNotAliasWhatTheHookReturned(t *testing.T) {
	// Stands in for the workflow's own live state, exactly as createFailureDraft
	// returns it.
	live := map[string]any{"args": map[string]any{"Zone": "cn-sh2-02"}}
	def := &Definition{
		Name: "AliasProbe",
		Steps: []Step{{
			Name:    "会失败的步骤",
			Type:    StepResolve,
			Resolve: func(*Context) (map[string]any, error) { return nil, fmt.Errorf("boom") },
		}},
		FailureDraft: func(*Context) map[string]any { return live },
	}
	eng := NewEngine(&mockExecutor{results: map[string]map[string]any{}},
		func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(def, map[string]any{})
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.NotNil(t, result.Failure.Draft)

	// A write through the record must not reach the workflow's state — and the
	// nested map is the only way to tell, exactly as with the draft's disks: a
	// top-level key write would land on a fresh outer map either way.
	result.Failure.Draft["args"].(map[string]any)["Zone"] = "TAMPERED"

	assert.Equal(t, "cn-sh2-02", live["args"].(map[string]any)["Zone"],
		"the record must be a copy: evidence that moves with the workflow is not evidence")
}

// TestTheCreateFailureDraftReallyIsTheLiveEntry pins the premise of the test above
// — that the copy is load-bearing because the create's hook does hand back live
// state. If this ever stopped being true the copy would look gratuitous.
func TestTheCreateFailureDraftReallyIsTheLiveEntry(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	runDraftStep(t, wfCtx)

	got := createFailureDraft(wfCtx)
	require.NotNil(t, got)

	got[draftKeyArgs].(map[string]any)[argsKeyZone] = "TAMPERED"
	assert.Equal(t, "TAMPERED",
		wfCtx.StepResults[createDraftStepName][draftKeyArgs].(map[string]any)[argsKeyZone],
		"the hook returns the live StepResults entry — which is why recordStepFailure copies it")
}

// TestASealedFailureSaysSo is the other side of ExecutionAuthorized: a failure
// AFTER the gate must report that a contract authorised it, or the field would be
// a constant false dressed up as a fact.
func TestASealedFailureSaysSo(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	executor.failOn = "CreateCompShareInstance"
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "创建实例", result.StoppedAt)

	require.NotNil(t, result.Failure)
	assert.Equal(t, "创建实例", result.Failure.Step)
	assert.True(t, result.Failure.ExecutionAuthorized,
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

		result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
		require.NoError(t, err)
		require.False(t, result.Success)
		require.NotNil(t, result.Failure, "a resolve failure is a failure")
		assert.Equal(t, result.StoppedAt, result.Failure.Step)
		assert.Nil(t, result.Failure.Draft,
			"no candidate was resolved, and saying so beats returning a half-built one")
		assert.False(t, result.Failure.ExecutionAuthorized)
	})

	t.Run("cancelled at the gate", func(t *testing.T) {
		executor := draftMockExecutor("cn-sh2-02")
		eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return false }, nil)

		result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
		require.NoError(t, err)
		require.False(t, result.Success)
		require.NotNil(t, result.Failure, "a declined confirmation is a failure with a record")
		assert.Equal(t, "确认创建", result.Failure.Step)
		assert.False(t, result.Failure.ExecutionAuthorized,
			"a gate that was declined authorised nothing")
	})
}

// TestASuccessfulWorkflowHasNoFailureRecord: the record must not become a field
// that is always populated and therefore says nothing.
func TestASuccessfulWorkflowHasNoFailureRecord(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(_ string, _ map[string]any) bool { return true }, nil)

	result, err := eng.runCreateTest(CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Nil(t, result.Failure)
}
