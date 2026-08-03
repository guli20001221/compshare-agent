package store

import (
	"net/url"
	"testing"
)

func TestDSNWithHostOverrideIPv6PreservesConnectionDetails(t *testing.T) {
	got, err := dsnWithHostOverride(
		"postgresql://user:password@old.example:5432/compshare?sslmode=disable",
		"2003:da8:2004:1000:0a3c:7623:2712:f9c0",
	)
	if err != nil {
		t.Fatalf("dsnWithHostOverride returned error: %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse overridden DSN: %v", err)
	}
	if u.Hostname() != "2003:da8:2004:1000:0a3c:7623:2712:f9c0" {
		t.Fatalf("host = %q", u.Hostname())
	}
	if u.Port() != "5432" {
		t.Fatalf("port = %q", u.Port())
	}
	if u.Path != "/compshare" || u.Query().Get("sslmode") != "disable" {
		t.Fatalf("connection details changed: path=%q query=%q", u.Path, u.RawQuery)
	}
}

func TestDSNWithHostOverrideLeavesDSNUnchangedWhenUnset(t *testing.T) {
	const dsn = "postgresql://user:password@old.example:5432/compshare?sslmode=disable"
	got, err := dsnWithHostOverride(dsn, "")
	if err != nil {
		t.Fatalf("dsnWithHostOverride returned error: %v", err)
	}
	if got != dsn {
		t.Fatalf("DSN changed without an override: %q", got)
	}
}
