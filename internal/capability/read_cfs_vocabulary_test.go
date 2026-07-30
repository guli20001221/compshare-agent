package capability

import (
	"testing"

	"github.com/compshare-agent/internal/readprojection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCFSAndInstancesNameABillingModeTheSameWay is the drift this fixes: the CFS
// list printed the raw upstream enum, so one account's storage read 「计费 Month」
// while its instances read 包月 — one concept, two names, with nothing telling
// the user they are the same thing.
//
// It asserts against readprojection.ChargeTypeLabel rather than a literal so the
// test cannot be satisfied by a second hand-written table that happens to agree
// today. The literal is asserted once, separately, so a silently-emptied label
// function would not make this vacuous.
func TestCFSAndInstancesNameABillingModeTheSameWay(t *testing.T) {
	require.Equal(t, "包月", readprojection.ChargeTypeLabel("Month"), "the shared vocabulary itself")

	for _, wire := range []string{"Month", "Day", "Postpay", "Dynamic", "Spot"} {
		reply := renderCFSInfoReply(map[string]any{"CFSSet": []any{map[string]any{
			"CfsId": "cfs-x", "Name": "train", "Size": float64(1000), "ChargeType": wire,
		}}})
		assert.Contains(t, reply, "计费 "+readprojection.ChargeTypeLabel(wire),
			"CFS must name %q with the same word the instance list uses", wire)
		assert.NotContains(t, reply, "计费 "+wire, "the wire enum must not reach the user")
	}
}

// TestAnUnmappedBillingModeShowsItselfRatherThanAWrongLabel: CFS accepts a Year
// charge type that the instance purchase vocabulary has no phrase for. Showing
// the raw value is the honest outcome; inventing 包年 would put a word in the
// product that nothing else parses.
func TestAnUnmappedBillingModeShowsItselfRatherThanAWrongLabel(t *testing.T) {
	reply := renderCFSInfoReply(map[string]any{"CFSSet": []any{map[string]any{
		"CfsId": "cfs-x", "Name": "train", "ChargeType": "Year",
	}}})
	assert.Contains(t, reply, "计费 Year")
}
