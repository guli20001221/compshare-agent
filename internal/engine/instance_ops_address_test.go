package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Address derivation fails before the lane enters the instance. The reply must preserve that
// evidence boundary: say what did not run, leave the underlying cause unresolved, and offer
// next steps the user can actually perform.
func TestInstanceOps_AddressUnavailableReportsTheUnresolvedEntryFailure(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsAddressUnavailable}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.True(t, strings.HasPrefix(out, finalReplyPrefix), "a failed rewrite is a terminal refusal")
	require.Contains(t, out, "没有进入实例")
	require.Contains(t, out, "没有执行任何实例内命令")
	require.Contains(t, out, "尚无法判断根因")
	require.Contains(t, out, "控制台", "the refusal must give the user an actionable fallback")
	require.NotContains(t, out, "与实例本身无关")
}
