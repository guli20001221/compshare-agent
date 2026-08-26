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

// Candidate addresses that all fail TCP preflight are not an address-derivation
// failure and do not prove which network, port, service, or instance layer failed.
func TestInstanceOps_SSHPreflightUnreachableReportsTheObservedBoundary(t *testing.T) {
	unreachable := newInstanceOpsEngine(
		&fakeInstanceOpsRunner{err: ErrInstanceOpsSSHPreflightUnreachable}, alwaysConfirm,
	).executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)
	require.Contains(t, unreachable, "候选地址")
	require.Contains(t, unreachable, "SSH 端口建立 TCP 连接")
	require.Contains(t, unreachable, "未进入实例")
	require.Contains(t, unreachable, "无法确定具体原因")
	require.Contains(t, unreachable, "无法判断用户原始故障是否属于实例内部")
	require.NotContains(t, unreachable, "网络 / 安全组未放通",
		"a failed TCP probe cannot select one unobserved cause")
}
