package capability

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetAcceleratorDoesNotOfferToEnableWhatIsAlreadyOn: the reply used to end
// with one fixed clause, so an enabled accelerator reported
// 「网络加速已开通。……如需开通，我会走确认流程。」 — the same sentence stating a fact and
// then offering to bring it about. A reader cannot tell which half is the
// mistake, so the whole answer stops being usable.
func TestNetAcceleratorDoesNotOfferToEnableWhatIsAlreadyOn(t *testing.T) {
	on, empty := renderNetAcceleratorStatusReply(map[string]any{"Optimized": true})
	require.False(t, empty)
	assert.Contains(t, on, "已开通")
	assert.NotContains(t, on, "如需开通", "nothing is off, so there is nothing to offer to enable")
	assert.Contains(t, on, "不会直接修改配置", "the read-only boundary is still stated")

	off, empty := renderNetAcceleratorStatusReply(map[string]any{"Optimized": false})
	require.False(t, empty)
	assert.Contains(t, off, "未开通")
	assert.Contains(t, off, "如需开通", "the offer is the useful next step when it IS off")
}

// A multi-region account is the reason this takes a flag instead of deleting the
// clause: with one region off, offering to enable it is still correct.
func TestNetAcceleratorOffersEnableWhenAnyRegionIsOff(t *testing.T) {
	mixed, _ := renderNetAcceleratorStatusReply(map[string]any{"Info": []any{
		map[string]any{"Region": "cn-bj2", "Optimized": true},
		map[string]any{"Region": "cn-sh2", "Optimized": false},
	}})
	assert.Contains(t, mixed, "如需开通", "one region is off — the offer applies to it")

	allOn, _ := renderNetAcceleratorStatusReply(map[string]any{"Info": []any{
		map[string]any{"Region": "cn-bj2", "Optimized": true},
		map[string]any{"Region": "cn-sh2", "Optimized": true},
	}})
	assert.NotContains(t, allOn, "如需开通", "every region is on — the offer is self-contradictory")
}

// TestPricingCollapsesZonesQuotingTheSamePrice: pricingSpecs fans a zone-less
// request out over every zone offering the GPU, which is right — a zone can
// price differently or expose different charge types. What it cannot do is know
// whether they DID differ, so identical quotes printed as N near-identical
// sections and the user had to diff them by eye.
func TestPricingCollapsesZonesQuotingTheSamePrice(t *testing.T) {
	rows := []gpuPriceRow{
		pricingQuoteRow("华北一C (cn-bj2-03)", 1.69),
		pricingQuoteRow("华北二C (cn-wlcb-03)", 1.69),
		pricingQuoteRow("上海二B (cn-sh2-02)", 1.69),
	}

	reply := renderPricingReply(rows)

	require.Contains(t, reply, "1.69", "the fixture must actually produce a price, or this proves nothing")
	assert.Equal(t, 1, strings.Count(reply, "### "), "one quote, one section:\n"+reply)
	for _, zone := range []string{"华北一C (cn-bj2-03)", "华北二C (cn-wlcb-03)", "上海二B (cn-sh2-02)"} {
		assert.Contains(t, reply, zone, "merging must not drop a zone the user could pick")
	}
}

// The other direction, and the reason this is a merge rather than a truncation:
// a zone that really is cheaper keeps its own section. Collapsing that would be
// hiding the one difference the user asked about.
func TestPricingKeepsZonesWhosePriceDiffers(t *testing.T) {
	rows := []gpuPriceRow{
		pricingQuoteRow("华北一C (cn-bj2-03)", 1.69),
		pricingQuoteRow("上海二B (cn-sh2-02)", 1.42),
	}

	reply := renderPricingReply(rows)

	require.Contains(t, reply, "1.69")
	require.Contains(t, reply, "1.42")
	assert.Equal(t, 2, strings.Count(reply, "### "), "different prices stay separate:\n"+reply)
}

// pricingQuoteRow builds a row in the shape the renderer actually reads:
// PriceDetails[].Instance keyed by ChargeType (mapChargeTypeToInstance), under a
// non-catalog Kind so pricingBillingTableForKind takes the account-price branch.
// An invented shape (a "Price" key, or the default empty Kind) renders
// 「价格数据缺失」 for every row — which merges too, and would have made the
// collapse test pass while proving nothing.
func pricingQuoteRow(zone string, postpay float64) gpuPriceRow {
	return gpuPriceRow{
		Name: "4090", Zone: zone, GPU: 1, Cpu: 8, Memory: 64,
		Kind: "当前账号价格（含折扣）",
		RawData: map[string]any{"PriceDetails": []any{
			map[string]any{"ChargeType": "Postpay", "Instance": postpay},
		}},
	}
}
