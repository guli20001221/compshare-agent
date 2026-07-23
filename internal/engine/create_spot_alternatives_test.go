package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/workflow"
)

// toolExecutorFunc adapts a plain function to tools.ToolExecutor so a test can
// observe the ARGUMENTS a read was issued with — the whole subject here is which
// catalog the remedy asks for.
type toolExecutorFunc func(action string, args map[string]any) (map[string]any, error)

func (f toolExecutorFunc) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	return f(action, args)
}

// catalogEntry is one AvailableInstanceTypes row in the shape ParseAvailableGPUs
// reads (Zone / Name / Status / GraphicsMemory.Value are all load-bearing).
func catalogEntry(name string, vramGB int) map[string]any {
	return map[string]any{
		"Name": name, "Zone": "cn-sh2-02", "Status": "Normal",
		"GraphicsMemory": map[string]any{"Value": float64(vramGB)},
		"Performance":    map[string]any{"Value": float64(vramGB)},
		"MachineSizes":   []any{map[string]any{"Gpu": float64(1)}},
	}
}

// engineWithCatalogSpy returns an Engine whose reads go to a spy, plus a pointer
// to the availability-catalog arguments it was called with.
//
// The catalog it answers with is the FULL one, because that is what upstream
// returns for every accepted InstanceType value — and the inventory carries the
// real SpotUnsupportedGpuTypes list, so a test can tell "filtered for Spot" apart
// from "asked the wrong catalog".
func engineWithCatalogSpy() (*Engine, *[]map[string]any) {
	seen := &[]map[string]any{}
	spy := toolExecutorFunc(func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeAvailableCompShareInstanceTypes":
			*seen = append(*seen, args)
			return map[string]any{"AvailableInstanceTypes": []any{
				catalogEntry("4090_48G", 48),
				catalogEntry("A800", 80),
				catalogEntry("3090", 24),
			}}, nil
		case "DescribeCompShareGpuInventory":
			return map[string]any{
				"SpotUnsupportedGpuTypes": []any{"4090_48G", "H20", "P40", "2080"},
			}, nil
		}
		return map[string]any{}, nil
	})
	return &Engine{safeExecutor: newSafeToolExecutor(spy, nil, nil, false)}, seen
}

// spotFailure returns the failure record a REAL sold-out create leaves behind for
// the given billing mode. It drives the actual workflow rather than hand-writing a
// draft: the draft is a typed, versioned encoding, and a fixture that guessed its
// shape would let createFailureTarget return nothing while every assertion below
// still passed against the guess.
func spotFailure(t *testing.T, chargeType string) *workflow.StepFailure {
	t.Helper()
	wfEng := workflow.NewEngine(soldOutExecutor{}, func(_ string, _ map[string]any) bool { return true }, nil)
	result, err := wfEng.Run(context.Background(), workflow.CreateInstanceDef(),
		map[string]any{"GpuType": "4090", "ChargeType": chargeType}, soldOutZoneCatalog())
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Message, "库存不足", "this must be the sold-out path")
	require.NotNil(t, result.Failure)
	return result.Failure
}

// TestCreateFailureTargetCarriesTheChargeType pins the input the remedy needs.
// Availability is per resource pool — Spot and on-demand are different pools —
// so a suggestion computed without the charge type answers about the wrong one.
func TestCreateFailureTargetCarriesTheChargeType(t *testing.T) {
	failure := spotFailure(t, "Spot")
	gpuType, zone, chargeType := createFailureTarget(failure)
	assert.Equal(t, "4090", gpuType)
	assert.Equal(t, "cn-sh2-02", zone)
	assert.Equal(t, "Spot", chargeType,
		"the failed create's billing mode must survive onto the record; the remedy is scoped by it")

	_, _, none := createFailureTarget(nil)
	assert.Empty(t, none, "no record, no claim about the charge type")
}

// TestSpotShortageOffersTheOtherPoolFirst is the product half. When a Spot create
// runs out, the likeliest fix is the pool that is not empty — the SAME hardware on
// demand — not a different GPU inside the same empty Spot pool. Offering only
// other GPU models also implies the shortage is about the hardware when it is
// about the pool.
func TestSpotShortageOffersTheOtherPoolFirst(t *testing.T) {
	eng, _ := engineWithCatalogSpy()

	spotReply := eng.createFailureReplyWithAlternatives(t.Context(),
		"4090_48G 1 卡当前库存不足（售罄）", nil, spotFailure(t, "Spot"))
	assert.Contains(t, spotReply, "按量付费",
		"a Spot shortage must point at the other resource pool, which is the remedy most likely to work")

	postpayReply := eng.createFailureReplyWithAlternatives(t.Context(),
		"4090_48G 1 卡当前库存不足（售罄）", nil, spotFailure(t, "Postpay"))
	assert.NotContains(t, postpayReply, "抢占式用的是独立的资源池",
		"an on-demand shortage must not advertise a switch the user did not make")
}

// TestTheRemedyNeverScopesTheCatalogByPool guards a fix that was pointed the wrong
// way. Scoping this query by InstanceType=spot reads as the careful thing to do —
// ask about the pool that actually failed. Upstream accepts the value and answers
// with NOTHING: DescribeAvailableCompShareInstanceTypes appends a row only for
// uhost/all (uhost-compshare-api formatResponse), measured live 2026-07-22 at
// rows=0 for spot against rows=19 for the others. So the scoped call does not
// narrow the remedy, it deletes it — a Spot sold-out would offer no alternative at
// all, which is indistinguishable from "there is nothing else".
func TestTheRemedyNeverScopesTheCatalogByPool(t *testing.T) {
	for _, chargeType := range []string{"Spot", "Postpay"} {
		eng, seenPtr := engineWithCatalogSpy()
		eng.createFailureReplyWithAlternatives(t.Context(), "售罄", nil, spotFailure(t, chargeType))
		seen := *seenPtr
		require.Len(t, seen, 1, "%s: the remedy must consult the catalog exactly once", chargeType)
		assert.NotContains(t, seen[0], "InstanceType",
			"%s: scoping this query by pool returns an empty catalog upstream", chargeType)
	}
}

// TestSpotRemedyDropsCardsNotSoldOnSpot is the half that scoping was reaching for,
// done against a source that can actually answer it.
//
// A Spot create on 4090_48G does not fail because the pool ran dry — the platform
// does not sell that card on Spot at all (SpotUnsupportedGpuTypes). It reaches the
// capacity gate looking exactly like a shortage, so the sold-out reply is free to
// recommend an equally impossible card and invite a retry that can never succeed.
func TestSpotRemedyDropsCardsNotSoldOnSpot(t *testing.T) {
	eng, _ := engineWithCatalogSpy()
	spotReply := eng.createFailureReplyWithAlternatives(t.Context(), "售罄", nil, spotFailure(t, "Spot"))
	assert.NotContains(t, spotReply, "4090_48G",
		"a card the platform does not offer on Spot is not an alternative for a Spot create")
	assert.Contains(t, spotReply, "A800", "the eligible cards still get offered")

	// The exclusion is Spot-only: on demand, 4090_48G is a perfectly good suggestion.
	postpayReply := eng.createFailureReplyWithAlternatives(t.Context(), "售罄", nil, spotFailure(t, "Postpay"))
	assert.Contains(t, postpayReply, "4090_48G",
		"the Spot-only exclusion must not leak into an on-demand failure")
}
