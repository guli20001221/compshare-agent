package readprojection

import (
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryConversionIsMonotonic is the shape of the bug, not one input.
//
// The converter carried a `memory < 1024 → print the number as GB` shortcut, so
// 1023 MB rendered as "1023 GB" and 1024 MB as "1 GB" — bigger input, smaller
// output, and one of them off by a factor of a thousand. Whether a sub-GB
// instance can exist is not the question: a size that reads 1000× too large is
// not something to leave in place because its input is believed impossible.
func TestMemoryConversionIsMonotonic(t *testing.T) {
	for _, mb := range []int{512, 1023, 1024, 2048, 49152, 65536, 98304, 122880} {
		got := resourceMemoryLabel(mb)
		require.NotEmpty(t, got)
		assert.NotContains(t, got, "内存未知", "%d MB is a real size", mb)
	}
	assert.Equal(t, "0.5 GB", resourceMemoryLabel(512))
	assert.Equal(t, "1 GB", resourceMemoryLabel(1024))
	assert.Equal(t, "64 GB", resourceMemoryLabel(65536))
	assert.Equal(t, "120 GB", resourceMemoryLabel(122880))
	// The inversion itself: 1023 MB must render as LESS than 1024 MB does.
	assert.NotEqual(t, "1023 GB", resourceMemoryLabel(1023),
		"1023 MB is just under 1 GB, not a thousand times more than 1024 MB")
}

// The legacy upstream initialization spelling must not reach the user in
// English. CLAUDE.md records `Install` as still accepted for response
// compatibility, and an unmapped state falls through to the raw value.
func TestLegacyInitializingStateIsTranslated(t *testing.T) {
	for _, state := range []string{"Initializing", "Installing", "Install"} {
		assert.Equal(t, "初始化中", resourceStateLabel(state), "state %q", state)
	}
	assert.Equal(t, "SomethingNew", resourceStateLabel("SomethingNew"),
		"an unknown state shows itself rather than being guessed at")
}

// TestChargeTypeLabelComesFromTheParsedVocabulary keeps the instance list on the
// same word list the server parses. `Year` is not a mode the product sells, so
// it must NOT be given an invented Chinese label.
func TestChargeTypeLabelComesFromTheParsedVocabulary(t *testing.T) {
	for _, value := range []string{
		deployment.ChargeTypePostpay, deployment.ChargeTypeDay,
		deployment.ChargeTypeMonth, deployment.ChargeTypeSpot,
	} {
		label := resourceChargeTypeLabel(value)
		resolved, ok := deployment.ExplicitChargeTypeFromPhrase(label)
		require.True(t, ok, "%s rendered as %q, which the server cannot parse back", value, label)
		assert.Equal(t, value, resolved, "%s rendered as %q, which means a different mode", value, label)
	}
	// The deprecated spelling must not appear as a distinct mode.
	assert.Equal(t, resourceChargeTypeLabel("Postpay"), resourceChargeTypeLabel("Dynamic"))
	assert.Equal(t, "Year", resourceChargeTypeLabel("Year"),
		"an unsold mode shows itself rather than getting a label the product never uses")
}

func TestRenderResourceSummaryUsesTheVocabularyLabel(t *testing.T) {
	got := RenderResourceSummary([]entity.InstanceSnapshot{{
		UHostId: "uhost-a", Name: "n", State: "Running", GPU: 0,
		CPU: 8, Memory: 65536, ChargeType: deployment.ChargeTypeMonth,
	}}, ResourceEnvelopeMeta{})
	assert.Contains(t, got, "包月")
	assert.NotContains(t, got, "按月",
		"the create card calls this 包月; a second name for it in the instance list is a discrepancy the user cannot resolve")
}

// TestFormatMonitorFloatReadsAsAMeasurementNotAFloat64: 平均 is computed here as
// sum/len, and the formatter printed the shortest round-tripping form — so a real
// CPU series rendered 「平均 0.0743801652892562%」. Seventeen digits assert a
// precision the monitoring API does not have, in a line meant to be skimmed.
func TestFormatMonitorFloatReadsAsAMeasurementNotAFloat64(t *testing.T) {
	// The exact value that produced the 17-digit output: 9/121 of a percent.
	assert.Equal(t, "0.07", formatMonitorFloat(9.0/121.0))
	assert.Equal(t, "12.5", formatMonitorFloat(12.5))
	assert.Equal(t, "12", formatMonitorFloat(12.0), "no trailing .00 on a whole number")
	assert.Equal(t, "96.55", formatMonitorFloat(96.55))
}

// A real zero and a rounds-to-zero are different claims. This package's standing
// rule is that a value we cannot prove is zero is never shown as zero — missing
// data surfaces as 无法确认, and a 0.004% average must not be rendered 「0%」,
// which reads as an idle machine.
func TestFormatMonitorFloatNeverRoundsANonZeroDownToZero(t *testing.T) {
	assert.Equal(t, "0", formatMonitorFloat(0), "a genuine zero is still zero")
	assert.Equal(t, "<0.01", formatMonitorFloat(0.004))
	assert.Equal(t, "<0.01", formatMonitorFloat(0.0000001))
	assert.Equal(t, ">-0.01", formatMonitorFloat(-0.004), "a negative sliver keeps its sign")
}
