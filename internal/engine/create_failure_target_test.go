package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// soldOutZoneCatalog is the turn's zone catalog for these sold-out create runs: the
// workflow resolves to cn-sh2-02 (where the fixtures home 4090) and reads its
// placement from this snapshot, not a per-zone param map (removed in the convergence).
func soldOutZoneCatalog() workflow.RunOption {
	return workflow.WithReferenceData(workflow.ReferenceData{ZoneCatalog: deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 8200, IsPod: false}, DisplayName: "上海二B"},
	})})
}

// soldOutPodZoneCatalog carries cn-sh2-02 as a Pod zone, for the pod-zone sold-out case.
func soldOutPodZoneCatalog() workflow.RunOption {
	return workflow.WithReferenceData(workflow.ReferenceData{ZoneCatalog: deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 2002, AzGroup: 3002, IsPod: true}},
	})})
}

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

func TestCreateFailureRetainsTheValidatedDraft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		guided bool
		pod    bool
		charge string
	}{
		{name: "ordinary", charge: "Postpay"},
		{name: "guided", guided: true, charge: "Postpay"},
		{name: "pod", pod: true, charge: "Postpay"},
		{name: "spot", charge: "Spot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := workflow.CreateInstanceDef()
			if tc.guided {
				def = workflow.CreateInstanceGuidedDef()
			}
			var executor tools.ToolExecutor = soldOutExecutor{}
			catalog := soldOutZoneCatalog()
			if tc.pod {
				executor = podSoldOutExecutor{}
				catalog = soldOutPodZoneCatalog()
			}
			wfEng := workflow.NewEngine(executor, func(string, map[string]any) bool { return true }, nil)
			result, err := wfEng.Run(context.Background(), def, map[string]any{"GpuType": "4090", "ChargeType": tc.charge}, catalog)
			require.NoError(t, err)
			require.False(t, result.Success)
			require.Contains(t, result.Message, "库存不足")
			require.NotNil(t, result.Failure)
			require.False(t, result.Failure.ExecutionAuthorized)
			draft, err := workflow.ParseCreateExecutionDraft(result.Failure.Draft)
			require.NoError(t, err)
			assert.Equal(t, "4090", draft.Args.GpuType)
			assert.Equal(t, "cn-sh2-02", draft.Placement.Zone)
			assert.Equal(t, tc.charge, draft.Args.ChargeType)
			if tc.guided {
				require.NotNil(t, result.Contract)
			}
			if tc.pod {
				assert.NotContains(t, result.Failure.Args, "Zone")
				assert.True(t, draft.Placement.IsPod)
			}
		})
	}
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
