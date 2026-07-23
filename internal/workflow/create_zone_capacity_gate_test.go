package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zoneCapacityExecutor answers CheckCompShareResourceCapacity per zone, so a test
// can say "cn-wlcb-03 is sold out, the others are not" — the exact live shape
// behind the reported failure (a 4090 create where 华北二C had no capacity and
// 上海二B did). Every other action falls through to the shared form fixture.
type zoneCapacityExecutor struct {
	*mockExecutor
	enoughByZone map[string]bool
	capacityArgs []map[string]any
}

func newZoneCapacityExecutor(enoughByZone map[string]bool) *zoneCapacityExecutor {
	return &zoneCapacityExecutor{mockExecutor: formMockExecutor(), enoughByZone: enoughByZone}
}

func (e *zoneCapacityExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	if action != "CheckCompShareResourceCapacity" {
		return e.mockExecutor.Execute(ctx, action, args)
	}
	e.capacityArgs = append(e.capacityArgs, args)
	e.mockExecutor.calls = append(e.mockExecutor.calls, executorCall{action, args})
	zone, _ := args["Zone"].(string)
	enough := e.enoughByZone[zone]
	return map[string]any{"Specs": []any{
		map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": enough},
	}}, nil
}

// runToZoneCard drives the guided create up to the zone card and returns its
// options. The confirm callback stops the run at that step, so the assertions are
// about what the user is SHOWN, not about what a later step would have said.
func runToZoneCard(t *testing.T, exec *zoneCapacityExecutor, params map[string]any) []ConfirmFormOption {
	t.Helper()
	var zoneOpts []ConfirmFormOption
	confirmEdits := func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if form != nil {
			if f := form.Field("Zone"); f != nil {
				zoneOpts = f.Options
				return ConfirmResolution{Confirmed: false}
			}
		}
		return ConfirmResolution{Confirmed: true}
	}
	eng := NewEngine(exec, func(string, map[string]any) bool { return true }, nil)
	eng.SetConfirmEditsFn(confirmEdits)
	_, err := eng.Run(context.Background(), CreateInstanceGuidedDef(), params,
		func(c *Context) { c.referenceData.ZoneCatalog = createZoneCatalog() })
	require.NoError(t, err)
	return zoneOpts
}

func zoneOptionByValue(t *testing.T, opts []ConfirmFormOption, zone string) ConfirmFormOption {
	t.Helper()
	for _, o := range opts {
		if o.Value == zone {
			return o
		}
	}
	t.Fatalf("zone %s not offered; got %+v", zone, opts)
	return ConfirmFormOption{}
}

// TestZoneCardDisablesAZoneWithNoCapacityBeforeTheUserPicksIt is the reported
// failure, turned into a gate. A user asked for a 4090, the card offered 华北二C,
// they picked it, and the create failed at 检查库存 because that zone had no
// capacity — while another zone did. The capacity API takes (image, GPU, zone) as
// input, so this is answerable at the zone card and nowhere earlier: image and
// GPU are settled by then and only the zone is open.
func TestZoneCardDisablesAZoneWithNoCapacityBeforeTheUserPicksIt(t *testing.T) {
	exec := newZoneCapacityExecutor(map[string]bool{"cn-sh2-02": true, "cn-wlcb-01": false})
	opts := runToZoneCard(t, exec, map[string]any{"GpuType": "4090"})
	require.NotEmpty(t, opts, "the zone card must still be offered")

	soldOut := zoneOptionByValue(t, opts, "cn-wlcb-01")
	assert.True(t, soldOut.Disabled,
		"a zone upstream says is not creatable must be unpickable, not merely annotated")
	assert.NotEmpty(t, soldOut.Reason, "a disabled option must say why")

	available := zoneOptionByValue(t, opts, "cn-sh2-02")
	assert.False(t, available.Disabled, "the zone that IS creatable stays selectable")

	// One call per (model, zone) the catalog offers while the GPU card is still
	// open, and no more: the probe must not re-ask per spec. The GPU card reads the
	// same answers, which is why it covers every model rather than only the current
	// one — an option a user can click has to be one they can buy.
	require.Len(t, exec.capacityArgs, 4)
	asked := map[string]bool{}
	for _, args := range exec.capacityArgs {
		gpu, _ := args["GpuType"].(string)
		zone, _ := args["Zone"].(string)
		asked[gpu+"/"+zone] = true
		assert.NotEmpty(t, args["CompShareImageId"],
			"creatability is per image — a probe without one answers a different question")
	}
	assert.Equal(t, map[string]bool{
		"4090/cn-wlcb-01": true, "4090/cn-sh2-02": true,
		"4090_48G/cn-wlcb-01": true, "A800/cn-wlcb-01": true,
	}, asked)
}

// TestGPUCardDisablesAModelNoZoneCanCreate is the same guarantee one card
// earlier: 4090 survives because 上海二B can create it, while 4090_48G and A800
// are offered only in the zone that cannot. Before the probe covered every
// model, both were selectable and the refusal arrived at 检查库存.
func TestGPUCardDisablesAModelNoZoneCanCreate(t *testing.T) {
	exec := newZoneCapacityExecutor(map[string]bool{"cn-sh2-02": true, "cn-wlcb-01": false})
	var gpuOpts []ConfirmFormOption
	eng := NewEngine(exec, func(string, map[string]any) bool { return true }, nil)
	eng.SetConfirmEditsFn(func(_ string, _ map[string]any, form *ConfirmForm) ConfirmResolution {
		if form != nil {
			if f := form.Field("GpuType"); f != nil {
				gpuOpts = f.Options
				return ConfirmResolution{Confirmed: false}
			}
		}
		return ConfirmResolution{Confirmed: true}
	})
	_, err := eng.Run(context.Background(), CreateInstanceGuidedDef(), map[string]any{},
		func(c *Context) { c.referenceData.ZoneCatalog = createZoneCatalog() })
	require.NoError(t, err)
	require.NotEmpty(t, gpuOpts)

	byValue := map[string]ConfirmFormOption{}
	for _, o := range gpuOpts {
		byValue[o.Value] = o
	}
	assert.False(t, byValue["4090"].Disabled, "上海二B can create a 4090, so the model stays selectable")
	assert.Contains(t, byValue["4090"].Note, "当前可创建",
		"the authoritative answer outranks the snapshot count in the note")
	assert.True(t, byValue["A800"].Disabled, "A800 is offered only where nothing can be created")
	assert.NotEmpty(t, byValue["A800"].Reason)
	assert.NotContains(t, byValue["A800"].Note, "在售",
		"a model that cannot be created must not also be described as on sale")
}

// TestZoneCardLeavesEveryZoneSelectableWhenNoneIsCreatable holds the line that
// separates steering from refusing.
//
// Graying out every zone produces a card that offers nothing, and ensureGuidedZone
// turns "no enabled option" into 暂无可选可用区 — raised at the zone step, which is
// BEFORE 形成执行草稿. The failure record would then carry no draft and no
// ReasonCapacitySoldOut, and the sold-out reply that offers alternatives is built
// from exactly those two. So when nothing is creatable there is nothing to steer
// toward, and the authoritative negative stays at 检查库存 where it arrives whole.
// This is the same reason an early hard stop after the capacity probe was reverted.
func TestZoneCardLeavesEveryZoneSelectableWhenNoneIsCreatable(t *testing.T) {
	exec := newZoneCapacityExecutor(map[string]bool{})
	opts := runToZoneCard(t, exec, map[string]any{"GpuType": "4090"})
	require.NotEmpty(t, opts, "a card with no options is the dead end this avoids")
	for _, o := range opts {
		assert.False(t, o.Disabled,
			"with nothing creatable anywhere the gate must not fire: %s", o.Value)
	}
}

// TestZoneCardIgnoresAProbeThatCouldNotAnswer keeps absence of evidence from
// becoming evidence of absence — the rule the option builders already follow for
// a missing capacity signal, now applied per zone.
func TestZoneCardIgnoresAProbeThatCouldNotAnswer(t *testing.T) {
	t.Run("no probe result at all", func(t *testing.T) {
		assert.Nil(t, zoneCreatability(nil))
		assert.Nil(t, zoneCreatability(map[string]any{}))
	})

	t.Run("a call that errored is unknown, not unavailable", func(t *testing.T) {
		known := zoneCreatability(encodeBatchOutcomes([]BatchOutcome{
			{Key: "cn-wlcb-03", Err: "timeout"},
			{Key: "cn-sh2-02", OK: true, Result: map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}},
		}))
		_, present := known["cn-wlcb-03"]
		assert.False(t, present, "an errored probe must leave the zone unjudged")
		assert.True(t, known["cn-sh2-02"])
	})

	t.Run("a success with no Specs is unknown, not unavailable", func(t *testing.T) {
		known := zoneCreatability(encodeBatchOutcomes([]BatchOutcome{
			{Key: "cn-wlcb-03", OK: true, Result: map[string]any{"Specs": []any{}}},
		}))
		_, present := known["cn-wlcb-03"]
		assert.False(t, present,
			"an empty spec list is a probe that told us nothing, not a zone that is full")
	})
}

// TestGPUCardTreatsAnUnansweredComboAsPossible is the same rule on the GPU card,
// and it is the one that decides whether a flaky upstream read can block a
// create. A model is grayed out only when EVERY zone it is offered in came back
// with an actual "no"; a probe that errored or returned nothing leaves the model
// selectable and the authoritative refusal to 检查库存.
func TestGPUCardTreatsAnUnansweredComboAsPossible(t *testing.T) {
	answered := map[string]bool{
		capacityComboKey("4090", "cn-sh2-02"):  false,
		capacityComboKey("A800", "cn-wlcb-01"): true,
		capacityComboKey("4090", "cn-wlcb-01"): false,
	}

	// Every zone answered "no" → a real dead end.
	creatable, known := gpuModelCreatable(answered, "4090", []string{"cn-wlcb-01", "cn-sh2-02"})
	assert.True(t, known)
	assert.False(t, creatable)

	// One zone was never answered → the model must stay possible, even though the
	// zone that DID answer said no. Treating the gap as a refusal would let one
	// failed HTTP call hide a buyable GPU.
	creatable, known = gpuModelCreatable(answered, "4090", []string{"cn-wlcb-01", "cn-unprobed-09"})
	assert.False(t, known, "a gap is not an answer")
	assert.True(t, creatable, "and an unanswered model must not be grayed out")

	// No probe results at all: nothing is judged.
	creatable, known = gpuModelCreatable(nil, "4090", []string{"cn-wlcb-01"})
	assert.False(t, known)
	assert.True(t, creatable)
}

// TestZoneCapacityProbeSkipsUntilAnImageIsResolved pins the ordering the gate
// depends on. Creatability is a property of the IMAGE too, so a probe fired
// before the image is pinned would gate the card on some other image's answer.
func TestZoneCapacityProbeSkipsUntilAnImageIsResolved(t *testing.T) {
	step := stepProbeZoneCapacity()

	noImage := formWfCtx(t, map[string]any{"GpuType": "4090"})
	delete(noImage.StepResults, "查询镜像")
	skip, err := step.SkipIf(noImage)
	require.NoError(t, err)
	assert.True(t, skip, "with no image resolved there is nothing truthful to ask")

	withImage := formWfCtx(t, map[string]any{"GpuType": "4090", "CompShareImageId": "img-001"})
	skip, err = step.SkipIf(withImage)
	require.NoError(t, err)
	require.False(t, skip, "a resolved image + GPU is exactly when the question is answerable")
	calls, err := step.BuildArgsBatch(withImage)
	require.NoError(t, err)
	require.Len(t, calls, 4, "one call per (model, zone) offered — both hardware cards read these")
	assert.Equal(t, capacityComboKey("4090", "cn-wlcb-01"), calls[0].Key)
	assert.Equal(t, capacityComboKey("4090", "cn-sh2-02"), calls[1].Key)
	for _, c := range calls {
		assert.Equal(t, c.Key, capacityComboKey(c.Args["GpuType"].(string), c.Args["Zone"].(string)),
			"each call must be filed under the combination it actually asked about")
	}

	// A pinned GPU that no card will re-open narrows the fan-out back down: the
	// wider probe pays for a choice the user still has, not for one they made.
	pinned := formWfCtx(t, map[string]any{
		"GpuType": "4090", "GuidedGpuLocked": true, "CompShareImageId": "img-004",
		"Zone": "cn-wlcb-01", "Gpu": float64(1), "Cpu": float64(16), "Memory": float64(65536),
	})
	pinnedCalls, err := step.BuildArgsBatch(pinned)
	require.NoError(t, err)
	assert.Len(t, pinnedCalls, 2, "only the pinned model's zones")
}

// TestGuidedCreateWiresTheZoneProbeBeforeTheZoneCard is the anti-orphan gate: a
// probe defined but not placed — or placed after the card it is meant to gate —
// is a unit-tested function with no effect (the shape capacityCreatable had
// before this change).
func TestGuidedCreateWiresTheZoneProbeBeforeTheZoneCard(t *testing.T) {
	steps := CreateInstanceGuidedDef().Steps
	probeAt, zoneAt, gpuAt := -1, -1, -1
	for i, s := range steps {
		switch s.Name {
		case zoneCapacityStepName:
			probeAt = i
		case "选择可用区":
			zoneAt = i
		case "选择 GPU":
			gpuAt = i
		}
	}
	require.NotEqual(t, -1, probeAt, "the capacity probe must be IN the guided flow")
	require.NotEqual(t, -1, zoneAt)
	require.NotEqual(t, -1, gpuAt)
	// It used to run BETWEEN these two, on the reasoning that it "needs the chosen
	// GPU". It does not — it fans out over the models instead — and sitting there
	// left the GPU card as the one place a user could pick something unbuyable.
	assert.Less(t, probeAt, gpuAt, "the probe must gate the GPU card too")
	assert.Less(t, probeAt, zoneAt, "…and the zone card, which reads the same answers")
}
