package capability

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCFSListSeparatesCurrentModesFromLegacyResponseCompatibility(t *testing.T) {
	labels := map[string]string{
		"Month": "包月", "Day": "包日", "Year": "包年",
		"Dynamic": "旧版按小时计费", "Postpay": "存量后付费", "Spot": "抢占式",
	}
	for wire, label := range labels {
		reply := renderCFSInfoReply(map[string]any{"CFSSet": []any{map[string]any{
			"CfsId": "cfs-x", "Name": "train", "Size": float64(1000), "ChargeType": wire,
		}}})
		assert.Contains(t, reply, "计费 "+label)
		assert.NotContains(t, reply, "计费 "+wire, "the wire enum must not reach the user")
	}
}

func TestAnUnmappedBillingModeShowsItselfRatherThanAWrongLabel(t *testing.T) {
	reply := renderCFSInfoReply(map[string]any{"CFSSet": []any{map[string]any{
		"CfsId": "cfs-x", "Name": "train", "ChargeType": "future-mode",
	}}})
	assert.Contains(t, reply, "计费 future-mode")
}
