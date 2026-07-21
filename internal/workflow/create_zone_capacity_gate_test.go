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

	// One call per candidate zone, and no more: the probe must not re-ask per spec.
	require.Len(t, exec.capacityArgs, 2)
	asked := map[string]bool{}
	for _, args := range exec.capacityArgs {
		zone, _ := args["Zone"].(string)
		asked[zone] = true
		assert.NotEmpty(t, args["CompShareImageId"],
			"creatability is per image — a probe without one answers a different question")
	}
	assert.Equal(t, map[string]bool{"cn-sh2-02": true, "cn-wlcb-01": true}, asked)
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
	require.Len(t, calls, 2, "one call per candidate zone of the selected GPU")
	assert.Equal(t, "cn-wlcb-01", calls[0].Key)
	assert.Equal(t, "cn-sh2-02", calls[1].Key)
	for _, c := range calls {
		assert.Equal(t, c.Key, c.Args["Zone"], "each call must ask about its OWN zone")
	}
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
	require.NotEqual(t, -1, probeAt, "the per-zone capacity probe must be IN the guided flow")
	require.NotEqual(t, -1, zoneAt)
	require.NotEqual(t, -1, gpuAt)
	assert.Greater(t, probeAt, gpuAt, "the probe needs the chosen GPU")
	assert.Less(t, probeAt, zoneAt, "…and must run before the card it gates")
}
