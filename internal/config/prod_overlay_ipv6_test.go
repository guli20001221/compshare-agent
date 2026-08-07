package config

import (
	"net"
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
	// Same reasoning for the translation prefix: a developer machine reaches instances at their
	// EIP and has no route to any translator, so a prefix here would add two dead candidates and
	// then REFUSE the run, turning the lane off locally.
	if base.Agent.SSHOps.PublicIPv6Prefix != "" {
		t.Errorf("agent.ssh_ops.public_ipv6_prefix must stay unset in the shared baseline, got %q",
			base.Agent.SSHOps.PublicIPv6Prefix)
	}
}

// The prefix is an experiment, so this does NOT require it to be present — clearing it once the
// question is settled must not fail the build. What it does require is that a prefix which IS
// present is coherent: an unparseable one, or one set without internal_ipv6, produces a lane that
// boots and then refuses every instance, and the run would read as "the prefix does not work"
// when nothing was ever dialled.
func TestProdOverlayPublicIPv6PrefixIsCoherentWhenSet(t *testing.T) {
	prod, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.prod.yaml"))
	if err != nil {
		t.Fatalf("load production overlay: %v", err)
	}
	prefix := prod.Agent.SSHOps.PublicIPv6Prefix
	if prefix == "" {
		t.Skip("no translation prefix configured — nothing to check")
	}
	if ip := net.ParseIP(prefix); ip == nil || ip.To4() != nil {
		t.Errorf("agent.ssh_ops.public_ipv6_prefix %q is not an IPv6 address", prefix)
	}
	if !prod.Agent.SSHOps.InternalIPv6 {
		t.Error("public_ipv6_prefix without internal_ipv6 does nothing: the candidate list only " +
			"replaces a bare-IPv4 advertised host, which is what internal_ipv6 turns on")
	}
}
