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

// A prefix set without internal_ipv6 is inert because only UHost IPv4 addresses
// enter the translation path. Reject that contradictory configuration at boot.
func TestPublicIPv6PrefixWithoutInternalIPv6FailsTheBoot(t *testing.T) {
	sc := baseSSHOpsConfig()
	sc.PublicIPv6Prefix = "2002:a40:2e05::"
	sc.InternalIPv6 = false

	_, err := buildSSHOpsService(sc, "m", "k", nil, sshops.AuditWriter(nil))
	if err == nil {
		t.Fatal("want a boot error, got a service that would silently ignore the prefix")
	}
	if !strings.Contains(err.Error(), "internal_ipv6") {
		t.Errorf("the error must name the setting that is missing, got %v", err)
	}
}

// Reject malformed prefixes at boot instead of failing the first diagnosis.
func TestMalformedPublicIPv6PrefixFailsTheBoot(t *testing.T) {
	for _, prefix := range []string{"2002:a40:2e05::/48", "10.64.46.5", "2002:a40:2e0g::"} {
		sc := baseSSHOpsConfig()
		sc.PublicIPv6Prefix = prefix
		sc.InternalIPv6 = true

		if _, err := buildSSHOpsService(sc, "m", "k", nil, sshops.AuditWriter(nil)); err == nil {
			t.Errorf("prefix %q must fail the boot", prefix)
		}
	}
}

// The shared baseline has no translated-address route; omitting it must remain valid.
func TestNoPublicIPv6PrefixBootsUnchanged(t *testing.T) {
	sc := baseSSHOpsConfig()
	sc.InternalIPv6 = true

	svc, err := buildSSHOpsService(sc, "m", "k", nil, sshops.AuditWriter(nil))
	if err != nil {
		t.Fatalf("a lane without a prefix must still boot: %v", err)
	}
	if svc == nil {
		t.Fatal("no service built")
	}
}
