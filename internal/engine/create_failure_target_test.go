package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// soldOutExecutor answers the create workflow's reads the way the platform does,
// except that capacity reports the spec sold out. RetCode is 0 throughout: a
// shortage is a SUCCESSFUL call whose body says no, which is why it surfaces at
// the capacity gate — before any confirmation.
type soldOutExecutor struct{}

func (soldOutExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu 22.04 CUDA 12", "Size": float64(102400)},
		}}, nil
	case "DescribeAvailableCompShareInstanceTypes":
		// 4090 lives ONLY in cn-sh2-02, and the user never said so — the workflow
		// derives it. That is the whole point: this zone exists nowhere in params.
		return map[string]any{"AvailableInstanceTypes": []any{
			map[string]any{
				"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal",
				"MachineSizes": []any{map[string]any{
					"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					},
				}},
				"CpuPlatforms": map[string]any{"Amd": map[string]any{}},
				"Disks":        []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
			},
		}}, nil
	case "CheckCompShareResourceCapacity":
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": false},
		}}, nil
	}
	return map[string]any{"RetCode": float64(0)}, nil
}

// TestTheSoldOutReplyReadsTheZoneTheWorkflowResolved connects the real producer to
// the real consumer: a genuine CreateInstanceWorkflow run produces the failure
// record, and the function the reply uses reads it.
//
// The user asked for a 4090 and named no zone. Alternatives must be searched in
// cn-sh2-02 — the zone the workflow resolved and capacity actually rejected. The
// caller used to read Zone out of top-level params, where it had never been, and
// an empty zone makes ParseAvailableGPUs search every zone at once: the user was
// offered cards that need not exist where they are buying.
func TestTheSoldOutReplyReadsTheZoneTheWorkflowResolved(t *testing.T) {
	args := map[string]any{"GpuType": "4090"}
	// The premise, stated rather than assumed: params cannot answer this.
	require.NotContains(t, args, "Zone")

	wfEng := workflow.NewEngine(soldOutExecutor{}, func(_ string, _ map[string]any) bool { return true }, nil)
	result, err := wfEng.Run(context.Background(), workflow.CreateInstanceDef(), args)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Message, "库存不足", "this must be the sold-out path, not some other failure")

	gpuType, zone := createFailureTarget(result.Failure)

	assert.Equal(t, "4090", gpuType)
	assert.Equal(t, "cn-sh2-02", zone,
		"the reply must look for alternatives where the shortage was found — the resolved "+
			"zone, which lives only in the draft")
}

// TestTheSoldOutReplyDoesNotTrustASelectionCard: on the guided path a contract
// exists at this point, and reading it as the confirmed create is how a selection
// card gets mistaken for consent. The record must still yield the resolved zone,
// and must still say nothing was authorised.
func TestTheSoldOutReplyDoesNotTrustASelectionCard(t *testing.T) {
	wfEng := workflow.NewEngine(soldOutExecutor{}, func(_ string, _ map[string]any) bool { return true }, nil)
	result, err := wfEng.Run(context.Background(), workflow.CreateInstanceGuidedDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Message, "库存不足")

	require.NotNil(t, result.Contract, "the guided flow has sealed its selection cards by now")
	require.NotNil(t, result.Failure)
	require.False(t, result.Failure.ExecutionAuthorized,
		"…but none of them authorised a create, and the record says so")

	gpuType, zone := createFailureTarget(result.Failure)
	assert.Equal(t, "4090", gpuType)
	assert.Equal(t, "cn-sh2-02", zone)
}

// TestSoldOutAlternativesStayInTheUsersZone is the user-visible symptom, through
// the real reply function rather than the helper underneath it.
//
// 4090 is sold out in cn-sh2-02, the zone the workflow resolved. A100 is offered
// there. H100 exists only in cn-bj2-04, where this user is not buying. Suggesting
// the H100 sends them to try something that cannot work.
//
// ParseAvailableGPUs treats an empty zone as "no filter" (gpu_live.go:63), so
// reading the zone out of params — where a user who named no zone never put one —
// silently searched every zone at once. This test would have passed on the old
// code only if the fixture had a single zone, which is why it has two.
func TestSoldOutAlternativesStayInTheUsersZone(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeAvailableCompShareInstanceTypes" {
			return map[string]any{"AvailableInstanceTypes": []any{
				gpuOffer("4090", "cn-sh2-02", 24),
				gpuOffer("A100", "cn-sh2-02", 80),
				gpuOffer("H100", "cn-bj2-04", 80), // a different zone entirely
			}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	confirm := func(string, map[string]any) bool { return true }
	eng := NewWithDeps(&mockLLM{}, executor, confirm)
	eng.safeExecutor = newSafeToolExecutor(executor, confirm, nil, true)

	// A real record from a real run: 4090 sold out in the zone the workflow chose.
	wfEng := workflow.NewEngine(soldOutExecutor{}, confirm, nil)
	result, err := wfEng.Run(context.Background(), workflow.CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.Contains(t, result.Message, "库存不足")

	reply := eng.createFailureReplyWithAlternatives(context.Background(), result.Message, result.Err, result.Failure)

	assert.Contains(t, reply, "A100", "the alternative that exists in the zone they are buying in")
	assert.NotContains(t, reply, "H100",
		"H100 is only in cn-bj2-04 — offering it sends the user to a zone they never chose")
	assert.NotContains(t, reply, "4090(", "the sold-out card is not an alternative to itself")
}

// TestSoldOutAlternativesStayInTheUsersPodZone is the case that decides Draft over
// Args, and it is the only one that can.
//
// For an ordinary zone the capacity request keeps Zone, so reading the request and
// reading the draft agree and no test can tell them apart. For a POD zone
// ApplyCapacityPlacementArgs strips Zone/Region/az_group and sends internal ids
// instead — so the call that reported the shortage genuinely cannot say where it
// looked. Read the request there and the zone is empty again, which is the
// every-zone bug back under a new name.
func TestSoldOutAlternativesStayInTheUsersPodZone(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeAvailableCompShareInstanceTypes" {
			return map[string]any{"AvailableInstanceTypes": []any{
				gpuOffer("4090", "cn-sh2-02", 24),
				gpuOffer("A100", "cn-sh2-02", 80),
				gpuOffer("H100", "cn-bj2-04", 80),
			}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	confirm := func(string, map[string]any) bool { return true }
	eng := NewWithDeps(&mockLLM{}, executor, confirm)
	eng.safeExecutor = newSafeToolExecutor(executor, confirm, nil, true)

	wfEng := workflow.NewEngine(podSoldOutExecutor{}, confirm, nil)
	result, err := wfEng.Run(context.Background(), workflow.CreateInstanceDef(), map[string]any{
		"GpuType":       "4090",
		"Zone":          "cn-sh2-02",
		"ZoneIsPods":    map[string]any{"cn-sh2-02": true},
		"ZoneIds":       map[string]any{"cn-sh2-02": float64(2002)},
		"ZoneRegionIds": map[string]any{"cn-sh2-02": float64(3002)},
	})
	require.NoError(t, err)
	require.Contains(t, result.Message, "库存不足", "must reach the capacity gate, not fail placement first")
	require.NotNil(t, result.Failure)
	require.NotContains(t, result.Failure.Args, "Zone",
		"the premise: a pod zone's capacity request carries no Zone at all")

	reply := eng.createFailureReplyWithAlternatives(context.Background(), result.Message, result.Err, result.Failure)

	assert.Contains(t, reply, "A100")
	assert.NotContains(t, reply, "H100",
		"the draft names cn-sh2-02 even though the request it produced does not — "+
			"reading the request here would search every zone again")
}

// podSoldOutExecutor is soldOutExecutor with a container image, which a pod zone
// requires (validateSelectedImageCompatibility rejects anything else).
type podSoldOutExecutor struct{}

func (podSoldOutExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if action == "DescribeCompShareImages" {
		return map[string]any{"ImageSet": []any{
			map[string]any{
				"CompShareImageId": "img-001",
				"Name":             "PyTorch 容器镜像",
				"Size":             float64(102400),
				"Container":        true,
			},
		}}, nil
	}
	return soldOutExecutor{}.Execute(ctx, action, args)
}

// gpuOffer builds one AvailableInstanceTypes entry. GraphicsMemory.Value is
// required: ParseAvailableGPUs drops any card without a VRAM figure.
func gpuOffer(name, zone string, vramGB int) map[string]any {
	return map[string]any{
		"Name": name, "Zone": zone, "Status": "Normal",
		"GraphicsMemory": map[string]any{"Value": float64(vramGB)},
	}
}

// TestWorkflowFinalParamsNeedsAYesNotJustAContract pins the source-selection rule,
// including the case that is unreachable today and must stay safe anyway.
func TestWorkflowFinalParamsNeedsAYesNotJustAContract(t *testing.T) {
	args := map[string]any{"GpuType": "4090"}
	sealed := &workflow.SealedActionContract{
		Operation:      "CreateInstanceWorkflow",
		BusinessParams: map[string]any{"GpuType": "A100"},
	}

	for _, tc := range []struct {
		name   string
		result *workflow.Result
		want   map[string]any
		why    string
	}{
		{
			name:   "no contract at all",
			result: &workflow.Result{Success: false, Failure: &workflow.StepFailure{Step: "检查库存"}},
			want:   args,
			why:    "nothing was ever sealed",
		},
		{
			name:   "success",
			result: &workflow.Result{Success: true, Contract: sealed},
			want:   sealed.BusinessParams,
			why:    "a completed workflow's contract is the one that gated its mutating step",
		},
		{
			name: "failed, authorised",
			result: &workflow.Result{
				Success:  false,
				Contract: sealed,
				Failure:  &workflow.StepFailure{Step: "创建实例", ExecutionAuthorized: true},
			},
			want: sealed.BusinessParams,
			why:  "the create was approved and then failed — the contract is what it ran on",
		},
		{
			name: "failed, NOT authorised",
			result: &workflow.Result{
				Success:  false,
				Contract: sealed,
				Failure:  &workflow.StepFailure{Step: "检查库存", ExecutionAuthorized: false},
			},
			want: args,
			why:  "a selection card's seal is not consent to create",
		},
		{
			name:   "failed with NO record — must not be read as yes",
			result: &workflow.Result{Success: false, Contract: sealed},
			want:   args,
			why: "silence is not consent: a path that forgets to record must lose a " +
				"narration, not gain an authorisation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, workflowFinalParams(tc.result, args), tc.why)
		})
	}
}

// TestOnlyASoldOutOffersAlternatives is the reply-level counterpart to the
// workflow's reason test: a create failure that is NOT a sold-out must not list
// alternatives, even when the draft is perfectly readable.
//
// This is the case an unclassified check ("any failure lists alternatives") gets
// wrong. A spec-not-found failure carries a full draft — GPU and zone both present
// — so the target lookup succeeds; the only thing standing between it and a wrong
// "try these instead" is that its reason is not capacity_sold_out.
func TestOnlyASoldOutOffersAlternatives(t *testing.T) {
	queried := false
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeAvailableCompShareInstanceTypes" {
			queried = true
			return map[string]any{"AvailableInstanceTypes": []any{gpuOffer("A100", "cn-sh2-02", 80)}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	confirm := func(string, map[string]any) bool { return true }
	eng := NewWithDeps(&mockLLM{}, executor, confirm)
	eng.safeExecutor = newSafeToolExecutor(executor, confirm, nil, true)

	// A real record from a real run whose capacity failed "spec not found" — a full
	// draft (GpuType + Zone both resolved), but no sold-out reason.
	wfEng := workflow.NewEngine(specNotFoundExecutor{}, confirm, nil)
	result, err := wfEng.Run(context.Background(), workflow.CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.Contains(t, result.Message, "未找到", "premise: the not-found branch, not sold-out")
	require.NotNil(t, result.Failure)
	require.Empty(t, result.Failure.Reason)
	gpuType, zone := createFailureTarget(result.Failure)
	require.NotEmpty(t, gpuType, "premise: the draft is fully readable, so a target exists")
	require.NotEmpty(t, zone)

	reply := eng.createFailureReplyWithAlternatives(context.Background(), result.Message, result.Err, result.Failure)

	assert.NotContains(t, reply, "当前可创建的其他机型",
		"a spec that does not exist is a configuration problem, not a shortage — no substitutes")
	assert.False(t, queried, "and the reply must not even query availability for a non-shortage")
}

// specNotFoundExecutor is soldOutExecutor whose capacity returns a spec the draft
// cannot match, so the gate takes its "库存中未找到…的规格组合" branch.
type specNotFoundExecutor struct{}

func (specNotFoundExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if action == "CheckCompShareResourceCapacity" {
		return map[string]any{"Specs": []any{
			map[string]any{"Gpu": float64(8), "Cpu": float64(128), "Mem": float64(512), "ResourceEnough": true},
		}}, nil
	}
	return soldOutExecutor{}.Execute(ctx, action, args)
}

// TestNoTargetMeansNoSuggestionAtAll: when the workflow could not say what it was
// building, the reply explains the failure and stops there.
//
// Returning an empty zone and carrying on was the old bug wearing a new hat: an
// empty zone is not a looser filter, it is no filter (gpu_live.go:63), so
// improvising produces a CONFIDENT recommendation drawn from regions the user is
// not buying in. Withholding the suggestion is the only honest option — the
// failure itself is still explained.
func TestNoTargetMeansNoSuggestionAtAll(t *testing.T) {
	queried := false
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeAvailableCompShareInstanceTypes" {
			queried = true
			return map[string]any{"AvailableInstanceTypes": []any{
				gpuOffer("A100", "cn-sh2-02", 80),
				gpuOffer("H100", "cn-bj2-04", 80),
			}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	confirm := func(string, map[string]any) bool { return true }
	eng := NewWithDeps(&mockLLM{}, executor, confirm)
	eng.safeExecutor = newSafeToolExecutor(executor, confirm, nil, true)

	msg := "4090 1 卡 / 16C / 64GB 当前库存不足（售罄），请换一个规格或稍后再试。"

	for _, tc := range []struct {
		name    string
		failure *workflow.StepFailure
	}{
		{"no record at all", nil},
		{"no draft resolved yet", &workflow.StepFailure{Step: "查询镜像"}},
		{"draft will not decode", &workflow.StepFailure{Step: "检查库存", Draft: map[string]any{"args": "not a draft"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queried = false
			reply := eng.createFailureReplyWithAlternatives(context.Background(), msg, nil, tc.failure)

			assert.Contains(t, reply, "库存不足", "the failure itself is still explained")
			assert.NotContains(t, reply, "当前可创建的其他机型",
				"no target, no suggestion — an every-zone list is worse than none")
			assert.NotContains(t, reply, "A100")
			assert.NotContains(t, reply, "H100")
			assert.False(t, queried, "and it must not even ask: there is nothing to filter by")
		})
	}
}

// TestAFailureWithNoDraftYieldsNothingRatherThanAGuess: an early failure resolved
// no candidate. Returning "" then is honest — and it is now the ONLY route to the
// old every-zone search, instead of that being what happened whenever the user
// did not type a zone.
func TestAFailureWithNoDraftYieldsNothingRatherThanAGuess(t *testing.T) {
	gpuType, zone := createFailureTarget(nil)
	assert.Empty(t, gpuType)
	assert.Empty(t, zone)

	gpuType, zone = createFailureTarget(&workflow.StepFailure{Step: "查询镜像"})
	assert.Empty(t, gpuType)
	assert.Empty(t, zone)

	// A draft that is present but not decodable is corruption, not a licence to
	// improvise a spec.
	gpuType, zone = createFailureTarget(&workflow.StepFailure{
		Step:  "检查库存",
		Draft: map[string]any{"args": "not a draft"},
	})
	assert.Empty(t, gpuType)
	assert.Empty(t, zone)
}
