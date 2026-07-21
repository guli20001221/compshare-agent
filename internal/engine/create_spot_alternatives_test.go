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

// engineWithCatalogSpy returns an Engine whose reads go to a spy, plus a pointer
// to the availability-catalog arguments it was called with.
func engineWithCatalogSpy() (*Engine, *[]map[string]any) {
	seen := &[]map[string]any{}
	spy := toolExecutorFunc(func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeAvailableCompShareInstanceTypes" {
			*seen = append(*seen, args)
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

// TestSpotAlternativesAreQueriedFromTheSpotCatalog guards the concrete bug: the
// remedy list was fetched with no arguments, i.e. from the ON-DEMAND catalog,
// and then presented as what is creatable for a Spot create. stepQueryInstanceTypes
// already scopes the same upstream action by InstanceType=spot; the two callers
// have to agree or the reply recommends cards that may be just as unavailable.
func TestSpotAlternativesAreQueriedFromTheSpotCatalog(t *testing.T) {
	eng, seenPtr := engineWithCatalogSpy()

	eng.createFailureReplyWithAlternatives(t.Context(), "售罄", nil, spotFailure(t, "Spot"))
	seen := *seenPtr
	require.Len(t, seen, 1, "the remedy must consult the catalog exactly once")
	assert.Equal(t, "spot", seen[0]["InstanceType"],
		"a Spot failure must read the SPOT catalog; the on-demand list is a different question")

	*seenPtr = nil
	eng.createFailureReplyWithAlternatives(t.Context(), "售罄", nil, spotFailure(t, "Postpay"))
	seen = *seenPtr
	require.Len(t, seen, 1)
	assert.NotContains(t, seen[0], "InstanceType",
		"an on-demand failure must not narrow to the spot catalog")
}
