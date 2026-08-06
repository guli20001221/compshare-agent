package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The internal-IPv6 route ships without ever having reached the real gateway — nothing routes
// there from a development machine — so the first production run IS the experiment, and its
// three outcomes have to be told apart from the REPLY, by someone who may not have server-log
// access. This pins the one that is new: the address could not be derived.
//
// Folding it into the generic bucket would send the user to 「到控制台查看实例状态」 about an
// instance that is fine, and would make "the gateway did not answer" indistinguishable from
// "the gateway answered and the box did not" — which are different teams.
func TestInstanceOps_AddressUnavailableBlamesTheDeploymentNotTheInstance(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsAddressUnavailable}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.True(t, strings.HasPrefix(out, finalReplyPrefix), "a failed rewrite is a terminal refusal")
	require.Contains(t, out, "内网地址", "the refusal must name the layer that failed")
	require.Contains(t, out, "与实例本身无关", "the user's instance is not implicated and the text must say so")
	require.NotContains(t, out, "到控制台查看实例状态",
		"sending the user to a console with nothing wrong on it is the advice this branch exists to avoid")

	// It must NOT become a catch-all: every other terminal failure keeps the retry text, because
	// for those retrying is genuinely the right advice.
	generic := &fakeInstanceOpsRunner{err: errors.New("sshops: audit begin failed")}
	out2 := newInstanceOpsEngine(generic, alwaysConfirm).
		executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)
	require.Contains(t, out2, "请稍后重试")
	require.NotContains(t, out2, "内网地址")
}
