package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the resolve-step contract: what the step may do, what consumes
// its product, and what happens when the definition wiring is wrong.

// TestCreateResolveStepCallsNoTool is the first thing StepResolve promises. The
// promise has teeth because the step's Resolve signature receives neither a
// context.Context nor an executor — but a definition could still have declared
// the draft as a tool step, so the declaration is what this checks.
//
// Note the narrow scope: this says the step calls no TOOL. It does not say Resolve
// is read-only — it holds the live Context and only Params is guarded. See
// Step.Resolve.
func TestCreateResolveStepCallsNoTool(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  *Definition
	}{
		{"plain", CreateInstanceDef()},
		{"guided", CreateInstanceGuidedDef()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var found bool
			for _, s := range tc.def.Steps {
				if s.Name != createDraftStepName {
					continue
				}
				found = true
				assert.Equal(t, StepResolve, s.Type)
				assert.Empty(t, s.Tool, "a resolve step must bind to no tool")
				assert.Nil(t, s.ToolFunc, "a resolve step must not choose a tool at runtime")
				assert.NotNil(t, s.Resolve)
			}
			require.True(t, found, "%s create must declare the draft step", tc.name)
		})
	}
}

// TestCreateResolveStepIsProjectedAsCallingNoTool: the execution contract must
// describe the step honestly. This is what an audit or evaluator reads.
func TestCreateResolveStepIsProjectedAsCallingNoTool(t *testing.T) {
	for _, step := range ExecutionContract(CreateInstanceDef()) {
		if step.Name != createDraftStepName {
			continue
		}
		assert.Equal(t, StepResolve, step.Type)
		assert.Equal(t, ToolBindingNone, step.ToolBinding)
		assert.Empty(t, step.Tool)
		assert.False(t, step.RiskKnown, "a step that calls no tool has no tool risk to state")
		return
	}
	t.Fatal("the draft step must appear in the execution contract")
}

// TestResolveStepProjectsAsNoToolEvenWhenMisdeclaredWithATool is where naming
// StepResolve in projectStep earns its place rather than duplicating the default.
//
// For a well-formed resolve step (no Tool, no ToolFunc) the default arm happens
// to produce the same answer. For a MISDECLARED one it does not: the default
// would fall through to `case step.Tool != ""`, report the binding as static, and
// look the tool's risk up in the security whitelist — describing, to an audit, the
// risk of a call this step will never make. The step type decides that a step
// calls no tool; a leftover Tool string does not get a vote.
func TestResolveStepProjectsAsNoToolEvenWhenMisdeclaredWithATool(t *testing.T) {
	c := projectStep(Step{Name: "误配的解析步骤", Type: StepResolve, Tool: "CreateCompShareInstance"})

	assert.Equal(t, ToolBindingNone, c.ToolBinding)
	assert.Empty(t, c.Tool)
	assert.False(t, c.RiskKnown,
		"a resolve step executes no tool — stating a tool's risk would describe an execution that never happens")
}

// TestResolveStepRejectsWritingParams is the invariant that makes the candidate a
// candidate. A resolve step runs before the gate — on the guided path, while an
// earlier selection card's seal is still live — so writing Params would either
// break that seal's digest or, on the plain path where no seal exists yet, edit
// the question while the user is being asked it.
//
// verifySealedContract cannot cover this: it fails OPEN on a nil seal.
func TestResolveStepRejectsWritingParams(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	result := &Result{}
	step := Step{
		Name: "偷偷改参数",
		Type: StepResolve,
		Resolve: func(c *Context) (map[string]any, error) {
			c.Params["GpuType"] = "A800"
			return map[string]any{"ok": true}, nil
		},
	}

	outcome := (&Engine{}).runResolveStep(step, 0, 1, wfCtx, result)

	assert.Equal(t, toolStepFailed, outcome)
	assert.Equal(t, "偷偷改参数", result.StoppedAt)
	assert.Contains(t, result.Message, "改写了业务参数")
	assert.NotContains(t, wfCtx.StepResults, "偷偷改参数",
		"a step that broke its contract must not also have its result believed")
}

// TestUnhandledStepTypeFailsLoudly: the Run switch used to have no default, so a
// step type it did not know fell through silently — no handler, no event — and
// Run then reported Success. A declared step not happening must never read as
// "the workflow completed".
func TestUnhandledStepTypeFailsLoudly(t *testing.T) {
	def := &Definition{
		Name: "BadWorkflow",
		Steps: []Step{{
			Name: "无法执行的步骤",
			Type: StepType(99),
		}},
	}

	result, err := (&Engine{}).Run(context.Background(), def, map[string]any{})

	require.NoError(t, err)
	assert.False(t, result.Success, "an unexecutable step must not report success")
	assert.Equal(t, "无法执行的步骤", result.StoppedAt)
	assert.Contains(t, result.Message, "无法执行")
}

// TestResolveStepWithoutResolveFuncFailsLoudly: same class — a StepResolve whose
// Resolve is nil is a definition bug, not a no-op.
func TestResolveStepWithoutResolveFuncFailsLoudly(t *testing.T) {
	result := &Result{}
	outcome := (&Engine{}).runResolveStep(
		Step{Name: "空解析步骤", Type: StepResolve}, 0, 1, NewContext(nil), result)

	assert.Equal(t, toolStepFailed, outcome)
	assert.Contains(t, result.Message, "未定义解析函数")
}

// TestCapacityAndPriceConsumeTheDraft is gate ③ from the other side: not "they no
// longer call resolveTargetSpec" (which a grep can claim) but "they cannot
// produce a request at all without the draft".
//
// A step that silently re-derived would still answer here; one that consumes the
// draft must refuse.
func TestCapacityAndPriceConsumeTheDraft(t *testing.T) {
	for _, tc := range []struct {
		name string
		step Step
	}{
		{"检查库存", stepCheckCapacity()},
		{"查询价格", stepGetPrice()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Everything a re-derivation would need is present — catalog, images,
			// GPU — and only the draft is missing.
			wfCtx := draftContext("cn-sh2-02")

			_, err := tc.step.BuildArgs(wfCtx)

			require.Error(t, err, "without a draft there is nothing legitimate to ask about")
			assert.Contains(t, err.Error(), "尚未形成执行草稿")
		})
	}
}

// TestCardPriceUnitFollowsTheDraftNotTheLiveParams closes the last hole in "the
// card reads only the draft".
//
// The price TEXT's charge type was still re-normalised from Params
// (createChargeType(wfCtx.Params)) while every other card value was projected from
// the draft. On every real path the two agree, so nothing was visibly wrong and
// the suite stayed green — which is exactly the shape of the card/create image
// split this whole step exists to remove: agreement by habit, not by contract.
//
// Here Params says Month AFTER the draft resolved Postpay, and the price response
// quotes BOTH. confirmPriceText selects its entry BY charge type, so a card
// reading Params does not merely mislabel the unit — it shows ¥888.00/月 for an
// instance the sealed contract will create as pay-as-you-go at ¥1.58/小时. The
// user approves a monthly commitment they are not making, at a price that is not
// theirs.
//
// Both entries are required for this test to mean anything: with only the Postpay
// entry the lookup misses and the price merely goes blank, which is a different
// (and much more obvious) bug than the one being pinned.
func TestCardPriceUnitFollowsTheDraftNotTheLiveParams(t *testing.T) {
	wfCtx := draftContext("cn-sh2-02")
	// The REAL upstream shape: GetCompShareInstancePriceResponse returns
	// {"ChargeType": ..., "Instance": <amount>}. Every fixture in this repo says
	// "Price" instead, a key that appears in zero live captures — confirmPriceText
	// tries Price first and falls back to Instance, so production has always run
	// the fallback while the tests exercised a branch that never fires.
	wfCtx.StepResults["查询价格"] = map[string]any{"PriceDetails": []any{
		map[string]any{"ChargeType": "Postpay", "Instance": 1.58},
		map[string]any{"ChargeType": "Month", "Instance": 888.0},
	}}
	draft := runToTheGate(t, wfCtx)
	require.Equal(t, "Postpay", draft.Args.ChargeType, "the draft resolved the default charge type")

	// Someone rewrites the request params after the draft was formed.
	wfCtx.Params["ChargeType"] = "Month"

	card, err := buildCreateConfirmArgs(wfCtx)
	require.NoError(t, err)

	assert.Equal(t, "Postpay", card["ChargeType"], "the card's charge type comes from the draft")
	assert.Equal(t, "¥1.58/小时（预估）", card["price"],
		"the price must describe what was drafted and will be created, not a live param that moved")
	assert.NotContains(t, card["price"], "888")
}

// TestCapacityAndPriceAskAboutTheDraftedZone: the draft's zone reaches both
// upstream calls. cn-sh2-02 is nowhere in the request — only the catalog homes
// 4090 there — so a hardcoded default or a second resolution would show.
func TestCapacityAndPriceAskAboutTheDraftedZone(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)

	seen := map[string]map[string]any{}
	for _, c := range executor.calls {
		seen[c.action] = c.args
	}
	require.Contains(t, seen, "CheckCompShareResourceCapacity")
	require.Contains(t, seen, "GetCompShareInstanceUserPrice")
	assert.Equal(t, "cn-sh2-02", seen["CheckCompShareResourceCapacity"]["Zone"])
	assert.Equal(t, "cn-sh2-02", seen["GetCompShareInstanceUserPrice"]["Zone"])
	assert.Equal(t, "cn-sh2-02", seen["CreateCompShareInstance"]["Zone"])
	assert.Equal(t, "img-001", seen["CheckCompShareResourceCapacity"]["CompShareImageId"])
	assert.Equal(t, "img-001", seen["GetCompShareInstanceUserPrice"]["CompShareImageId"])
}

// TestPriceIsNotHandedTheCreateOnlyDraftFields: the draft carries the full create
// request, so "price reads the draft" must not quietly mean "price now posts
// MachineType / LoginMode / MinimalCpuPlatform / Name". Sharing a resolution is
// not sharing a request shape.
func TestPriceIsNotHandedTheCreateOnlyDraftFields(t *testing.T) {
	executor := draftMockExecutor("cn-sh2-02")
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	_, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{
		"GpuType": "4090", "Name": "my-gpu-server",
	})
	require.NoError(t, err)

	var priceArgs, createArgs map[string]any
	for _, c := range executor.calls {
		switch c.action {
		case "GetCompShareInstanceUserPrice":
			priceArgs = c.args
		case "CreateCompShareInstance":
			createArgs = c.args
		}
	}
	require.NotNil(t, priceArgs)
	for _, k := range []string{"MachineType", "LoginMode", "MinimalCpuPlatform", "Name"} {
		assert.NotContains(t, priceArgs, k, "%s is a create argument; price was never given it", k)
		assert.Contains(t, createArgs, k, "...and the create must still send it", k)
	}
}

// TestPodZoneCapacityAndPurchaseFlattenPlacementDifferently is why the draft
// carries a structured placement rather than only the upstream args: the two
// shapes genuinely differ, so capacity cannot be a projection of the create
// request. Recovering one from the other would be lossy.
func TestPodZoneCapacityAndPurchaseFlattenPlacementDifferently(t *testing.T) {
	wfCtx := draftContext("cn-newpod-03")
	wfCtx.Params["Zone"] = "cn-newpod-03"
	wfCtx.Params["ZoneIds"] = map[string]uint32{"cn-newpod-03": 9103}
	wfCtx.Params["ZoneRegionIds"] = map[string]uint32{"cn-newpod-03": 3103}
	wfCtx.Params["ZoneIsPods"] = map[string]bool{"cn-newpod-03": true}
	// A pod zone only takes container images.
	wfCtx.StepResults["查询镜像"] = map[string]any{"ImageSet": []any{
		map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch", "Container": true},
	}}
	runDraftStep(t, wfCtx)

	capacityArgs, err := stepCheckCapacity().BuildArgs(wfCtx)
	require.NoError(t, err)
	priceArgs, err := stepGetPrice().BuildArgs(wfCtx)
	require.NoError(t, err)

	assert.Equal(t, true, capacityArgs["IsPod"])
	assert.NotContains(t, capacityArgs, "Zone", "a pod capacity check drops Zone...")
	assert.NotContains(t, capacityArgs, "az_group")
	assert.Equal(t, uint32(9103), capacityArgs["zone_id"])

	assert.Equal(t, "cn-newpod-03", priceArgs["Zone"], "...where a purchase keeps it")
	assert.Equal(t, uint32(3103), priceArgs["az_group"])
	assert.Equal(t, uint32(9103), priceArgs["zone_id"])
}

// TestCreateInstance_PodZoneWithoutAzGroupRefusedAtDraft pins the refusal that
// MOVED when placement validation converged on the draft.
//
// The capacity step used to validate with purchase=false, which skips the
// az_group check, so a pod zone with no resolved az_group passed capacity and was
// only caught at 查询价格. The draft validates once, with the strictest
// (purchase=true) form, so the refusal now happens before either call. Nothing
// was created on the old path either — but the user was told later, after two
// pointless upstream round-trips.
func TestCreateInstance_PodZoneWithoutAzGroupRefusedAtDraft(t *testing.T) {
	executor := createMockExecutor()
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{
		"GpuType": "4090",
		"Zone":    "cn-newpod-03",
		"ZoneIds": map[string]uint32{"cn-newpod-03": 9103},
		// ZoneRegionIds deliberately absent → AzGroup == 0.
		"ZoneIsPods": map[string]bool{"cn-newpod-03": true},
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, createDraftStepName, result.StoppedAt)
	assert.Contains(t, result.Message, "内部地域编号")
	for _, c := range executor.calls {
		assert.NotEqual(t, "CheckCompShareResourceCapacity", c.action,
			"a placement we cannot safely create in is not worth a capacity call")
		assert.NotEqual(t, "GetCompShareInstanceUserPrice", c.action)
		assert.NotEqual(t, "CreateCompShareInstance", c.action)
	}
}

// ---------------------------------------------------------------------------
// The re-run boundary
// ---------------------------------------------------------------------------

// TestFormEditRerunsFromTheDraftInDefinitionOrder is gate ④: after an edit the
// order must be draft → stock → price, not stock → price on a stale draft.
//
// The old RevalidateSteps list named only 检查库存 and 查询价格, which was correct
// only because the draft was materialized later, inside the confirm's BuildArgs.
// With the draft resolved up front, a list that did not name it would re-check
// stock for the PREVIOUS GPU and then show a card for the new one.
func TestFormEditRerunsFromTheDraftInDefinitionOrder(t *testing.T) {
	executor := formMockExecutor()
	var order []string
	eng := NewEngine(executor, nil, func(ev StepEvent) {
		if ev.Status == "running" {
			order = append(order, ev.StepName)
		}
	})
	rounds := 0
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		rounds++
		if rounds == 1 {
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"GpuType": "A800"}}
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.Run(context.Background(), CreateInstanceDef(), map[string]any{"GpuType": "4090"})
	require.NoError(t, err)
	require.True(t, result.Success)

	// Two passes over the range, each in definition order.
	//
	// 形成确认快照 appears in the re-run without anyone adding it there: the gate
	// names a BOUNDARY, and the engine walks the definition from it. The old
	// RevalidateSteps list would have had to be edited by hand when that step was
	// introduced — and if it had not been, the refreshed card would have shown the
	// price quoted for the PREVIOUS GPU, silently.
	assert.Equal(t,
		[]string{
			"查询镜像", "查询可用配比", createDraftStepName, "检查库存", "查询价格", createConfirmationStepName,
			createDraftStepName, "检查库存", "查询价格", createConfirmationStepName,
			"创建实例", "查看状态",
		}, order,
		"an edit must rebuild the draft BEFORE re-asking stock and price about it, then re-snapshot the quote")

	// And stock was asked about the edited GPU, not the original.
	var capacityGPUs []string
	for _, c := range executor.calls {
		if c.action == "CheckCompShareResourceCapacity" {
			gt, _ := c.args["GpuType"].(string)
			capacityGPUs = append(capacityGPUs, gt)
		}
	}
	assert.Equal(t, []string{"4090", "A800"}, capacityGPUs)
}

// TestFormEditDiscardsTheOldCandidateDraft: the re-run range is cleared before it
// is re-run, so a stale result cannot be mistaken for a fresh one.
func TestFormEditDiscardsTheOldCandidateDraft(t *testing.T) {
	var resolveRuns int
	var sawStaleResult bool
	def := &Definition{
		Name: "DiscardProbe",
		Steps: []Step{
			{Name: "draft", Type: StepResolve, Resolve: func(c *Context) (map[string]any, error) {
				resolveRuns++
				if c.Result("draft") != nil {
					sawStaleResult = true
				}
				return map[string]any{"round": fmt.Sprint(resolveRuns)}, nil
			}},
			{
				Name: "gate", Type: StepConfirm,
				BuildArgs:      func(*Context) (map[string]any, error) { return map[string]any{}, nil },
				BuildForm:      probeForm,
				ApplyOverrides: func(*Context, map[string]string) error { return nil },
				RevalidateFrom: "draft",
			},
		},
	}
	eng := NewEngine(&mockExecutor{}, nil, nil)
	rounds := 0
	eng.SetConfirmEditsFn(func(string, map[string]any, *ConfirmForm) ConfirmResolution {
		rounds++
		if rounds == 1 {
			return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"x": "y"}}
		}
		return ConfirmResolution{Confirmed: true}
	})

	result, err := eng.Run(context.Background(), def, map[string]any{})

	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, 2, resolveRuns, "the edit must have re-run the step — otherwise this proves nothing")
	assert.False(t, sawStaleResult,
		"the previous candidate must be discarded before the step that replaces it re-runs")
}

// probeForm is a minimal one-field editable form for the synthetic definitions
// above: the gate re-validates overrides against the form, so a probe that
// submits an edit needs a field that legitimately accepts it.
func probeForm(*Context) (*ConfirmForm, error) {
	return &ConfirmForm{Version: 1, Fields: []ConfirmFormField{{
		Key: "x", Label: "x", Type: "select", Value: "y", Editable: true,
		Options: []ConfirmFormOption{{Value: "y", Label: "y"}},
	}}}, nil
}

// TestRevalidateFromUnknownStepFailsLoudly is gate ⑤, and it is the defect that
// motivated replacing the name list. findToolStep resolved each name and did
// `if !ok { continue }` — so a typo, a rename, or naming a step of any other type
// meant that step silently did not re-run, and the user re-confirmed a card built
// on results describing params they had just replaced. A gate that can quietly do
// nothing is not a gate.
func TestRevalidateFromUnknownStepFailsLoudly(t *testing.T) {
	def := &Definition{
		Name: "TypoWorkflow",
		Steps: []Step{
			{Name: "真实步骤", Type: StepResolve, Resolve: func(*Context) (map[string]any, error) {
				return map[string]any{}, nil
			}},
			{
				Name: "确认", Type: StepConfirm,
				BuildArgs:      func(*Context) (map[string]any, error) { return map[string]any{}, nil },
				BuildForm:      probeForm,
				ApplyOverrides: func(*Context, map[string]string) error { return nil },
				RevalidateFrom: "拼错的步骤名",
			},
		},
	}
	eng := NewEngine(&mockExecutor{}, nil, nil)
	eng.SetConfirmEditsFn(func(string, map[string]any, *ConfirmForm) ConfirmResolution {
		return ConfirmResolution{Confirmed: true, Overrides: map[string]string{"x": "y"}}
	})

	result, err := eng.Run(context.Background(), def, map[string]any{})

	require.NoError(t, err)
	assert.False(t, result.Success, "an unresolvable re-run boundary must stop the workflow")
	assert.Contains(t, result.Message, "找不到重跑起点")
}

// TestCreateRevalidateBoundaryIsBeforeItsGateAndHasNoConfirmInside: the boundary
// is only meaningful if the range it names is legal. A gate inside it would ask
// the user to confirm something in the middle of them editing it, and Run would
// seal that answer.
func TestCreateRevalidateBoundaryIsBeforeItsGateAndHasNoConfirmInside(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  *Definition
	}{
		{"plain", CreateInstanceDef()},
		{"guided", CreateInstanceGuidedDef()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, step := range tc.def.Steps {
				if step.RevalidateFrom == "" {
					continue
				}
				start := -1
				for j := range tc.def.Steps {
					if tc.def.Steps[j].Name == step.RevalidateFrom {
						start = j
						break
					}
				}
				require.GreaterOrEqual(t, start, 0, "boundary %q names no step", step.RevalidateFrom)
				require.Less(t, start, i, "boundary must precede the gate that re-runs from it")
				for j := start; j < i; j++ {
					assert.NotEqual(t, StepConfirm, tc.def.Steps[j].Type,
						"step %q sits inside a re-run range", tc.def.Steps[j].Name)
				}
			}
		})
	}
}
