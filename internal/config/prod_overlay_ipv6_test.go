package config

import (
	"path/filepath"
	"testing"
)

// The internal-IPv6 route is switched on ONLY by deploy/conf/config.prod.yaml. If the overlay
// merge does not carry a newly-added nested bool through, the flag stays false, the lane keeps
// dialling the address production cannot reach, and nothing anywhere says so — the change would
// ship, boot cleanly, log "enabled", and no-op. That is the failure this pins.
//
// It reads the SHIPPED files rather than a fixture, because a fixture would only prove the merge
// works on a file I wrote in the same breath as the assertion.
func TestProdOverlayEnablesTheInternalIPv6Route(t *testing.T) {
	prod, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.prod.yaml"))
	if err != nil {
		t.Fatalf("load production overlay: %v", err)
	}
	if !prod.Agent.SSHOps.InternalIPv6 {
		t.Error("agent.ssh_ops.internal_ipv6 must be true in the production overlay; " +
			"without it production keeps dialling the public EIP it has no route to")
	}
	// The rewrite refuses the run when it cannot reach the gateway, so an overlay that turns it
	// on without one turns the lane off for every instance. cmd's boot check makes that an error;
	// this catches it before anybody has to boot.
	if prod.Agent.STS.IAMURL == "" {
		t.Error("internal_ipv6 requires agent.sts.iam_url; the overlay enables it without one")
	}
	// Inheritance must still hold, or "only network overrides live here" has quietly stopped
	// being true and the overlay is dropping product settings from the baseline.
	if !prod.Agent.SSHOps.AllowWrites || prod.Agent.SSHOps.HarnessPath == "" {
		t.Errorf("the overlay must inherit the baseline's ssh_ops settings, got allow_writes=%v harness_path=%q",
			prod.Agent.SSHOps.AllowWrites, prod.Agent.SSHOps.HarnessPath)
	}
}

// The baseline is what a developer machine and every local CLI run load. There the EIP is the only
// reachable address and internal.api.ucloud.cn does not route at all, so the flag being absent is
// load-bearing, not an oversight — and an absent-vs-false distinction is exactly the kind of thing
// that gets "tidied up" by someone syncing the two files.
func TestBaselineLeavesTheInternalIPv6RouteOff(t *testing.T) {
	base, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.local.yaml"))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if base.Agent.SSHOps.InternalIPv6 {
		t.Error("agent.ssh_ops.internal_ipv6 must stay off in the shared baseline; " +
			"turning it on there breaks every local run of the lane")
	}
}
