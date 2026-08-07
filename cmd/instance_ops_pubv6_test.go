package main

import (
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/sshops"
)

// baseSSHOpsConfig is the minimum that gets past the harness validation, so each case below
// varies exactly one thing.
func baseSSHOpsConfig() config.SSHOpsConfig {
	return config.SSHOpsConfig{
		HarnessPath: "deploy/ssh_ops_harness/harness.py",
		BaseURL:     "https://example.invalid/anthropic",
		APIKey:      "k",
		Model:       "m",
		Timeout:     time.Minute,
	}
}

// A prefix set without internal_ipv6 is invisible: the candidate list only ever replaces a bare
// IPv4 advertised host, and that rewrite is what internal_ipv6 turns on. Booting anyway would
// produce a lane that looks configured, dials the same address as before, and lets the
// experiment be reported as "the prefix does not work" without a single candidate being tried.
func TestPublicIPv6PrefixWithoutInternalIPv6FailsTheBoot(t *testing.T) {
	sc := baseSSHOpsConfig()
	sc.PublicIPv6Prefix = "2002:a40:2e05::"
	sc.InternalIPv6 = false

	_, err := buildSSHOpsService(sc, "m", "k", sshops.AuditWriter(nil))
	if err == nil {
		t.Fatal("want a boot error, got a service that would silently ignore the prefix")
	}
	if !strings.Contains(err.Error(), "internal_ipv6") {
		t.Errorf("the error must name the setting that is missing, got %v", err)
	}
}

// A typo in the prefix would otherwise surface as a refusal on the FIRST real diagnosis, in the
// middle of the gray-account run — which reads as a failed experiment rather than a failed edit.
func TestMalformedPublicIPv6PrefixFailsTheBoot(t *testing.T) {
	for _, prefix := range []string{"2002:a40:2e05::/48", "10.64.46.5", "2002:a40:2e0g::"} {
		sc := baseSSHOpsConfig()
		sc.PublicIPv6Prefix = prefix
		sc.InternalIPv6 = true

		if _, err := buildSSHOpsService(sc, "m", "k", sshops.AuditWriter(nil)); err == nil {
			t.Errorf("prefix %q must fail the boot", prefix)
		}
	}
}

// The default has to stay a no-op, or every deployment that never opted in inherits an
// experiment. Absence of the key is the state the shared baseline ships.
func TestNoPublicIPv6PrefixBootsUnchanged(t *testing.T) {
	sc := baseSSHOpsConfig()
	sc.InternalIPv6 = true

	svc, err := buildSSHOpsService(sc, "m", "k", sshops.AuditWriter(nil))
	if err != nil {
		t.Fatalf("a lane without a prefix must still boot: %v", err)
	}
	if svc == nil {
		t.Fatal("no service built")
	}
}
