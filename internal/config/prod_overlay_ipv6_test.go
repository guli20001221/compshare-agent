package config

import (
	"net"
	"path/filepath"
	"testing"
)

// Read the shipped overlay because internal IPv6 routing is production-specific;
// a synthetic fixture would not verify the deployed configuration.
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
	if prod.Agent.SSHOps.HarnessPath == "" {
		t.Error("the overlay must inherit the baseline's ssh_ops harness path")
	}
}

// Internal address translation belongs only to deployments that have the
// required gateway access. Local runs use the instance address as advertised.
func TestBaselineLeavesTheInternalIPv6RouteOff(t *testing.T) {
	base, err := Load(filepath.Join("..", "..", "deploy", "conf", "config.local.yaml"))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if base.Agent.SSHOps.InternalIPv6 {
		t.Error("agent.ssh_ops.internal_ipv6 must stay off in the shared baseline; " +
			"turning it on there breaks every local run of the lane")
	}
	// A translated public-IPv4 candidate is likewise deployment-specific.
	if base.Agent.SSHOps.PublicIPv6Prefix != "" {
		t.Errorf("agent.ssh_ops.public_ipv6_prefix must stay unset in the shared baseline, got %q",
			base.Agent.SSHOps.PublicIPv6Prefix)
	}
}

// A production translation prefix is optional, but when present it must be a
// coherent IPv6 address and internal routing must be enabled.
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
