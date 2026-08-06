package main

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// The address rewrite is off unless a deployment asks for it, and when it IS asked for a
// missing gateway URL is a boot error rather than a silent downgrade. The downgrade is the
// dangerous outcome: agent.ssh_ops.internal_ipv6 is set exactly on deployments where the
// public address does not work, so falling back to it would yield a lane that boots, logs
// "enabled", and then times out on every single instance — the failure this resolver exists
// to end, reproduced with the fix installed.
func TestInstanceOpsHostResolver_OffByDefaultAndLoudWhenMisconfigured(t *testing.T) {
	cfg := gateCfg()
	hr, err := instanceOpsHostResolver(cfg, noopDescriber{})
	require.NoError(t, err)
	require.Nil(t, hr, "no internal_ipv6 setting must leave the lane dialling the advertised address")

	cfg.Agent.SSHOps.InternalIPv6 = true
	_, err = instanceOpsHostResolver(cfg, noopDescriber{})
	require.Error(t, err, "internal_ipv6 without agent.sts.iam_url must fail boot, not fall back")
	require.Contains(t, err.Error(), "iam_url")

	cfg.Agent.STS.IAMURL = "http://internal.example"
	hr, err = instanceOpsHostResolver(cfg, noopDescriber{})
	require.NoError(t, err)
	require.NotNil(t, hr)
}

// A misconfigured rewrite must take the whole server boot down with it, for the same reason
// a misconfigured harness does: a lane that cannot enter any box is not a degraded lane, it
// is a broken one, and it should say so at startup rather than once per user.
func TestServerInstanceOpsRunner_InternalIPv6MisconfigIsBootError(t *testing.T) {
	cfg := gateCfg()
	cfg.Agent.SSHOps.InternalIPv6 = true // no agent.sts.iam_url
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	_, err := serverInstanceOpsRunner(cfg, gateEnv(map[string]string{"COMPSHARE_SSH_OPS": "1"}), noopDescriber{}, db)
	require.Error(t, err)
}

// The boot line is what an operator greps to confirm what is running, so it has to name the
// route: "enabled" plus a timeout on every instance is exactly the state this change fixes,
// and the two routes are otherwise indistinguishable from outside.
func TestServerInstanceOpsRunner_BootLineNamesTheDialledRoute(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	cfg := gateCfg()
	cfg.Agent.SSHOps.InternalIPv6 = true
	cfg.Agent.STS.IAMURL = "http://internal.example"
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	r, err := serverInstanceOpsRunner(cfg, gateEnv(map[string]string{"COMPSHARE_SSH_OPS": "1"}), noopDescriber{}, db)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.Contains(t, buf.String(), "internal IPv6")
	require.Contains(t, buf.String(), "http://internal.example")
}
