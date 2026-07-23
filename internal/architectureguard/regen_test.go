package architectureguard

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRegenBaseline rewrites baseline.json from a fresh Scan. It is the reseed
// mechanism the guard needs whenever scanner coverage widens or legacy semantic
// sites are deleted (Unexpected() only flags NEW sites, so removals leave stale
// allowlist entries that this prunes). Gated so a normal test run never rewrites
// the reviewed baseline: run `REGEN_ARCH_BASELINE=1 go test ./internal/architectureguard/ -run TestRegenBaseline`.
func TestRegenBaseline(t *testing.T) {
	if os.Getenv("REGEN_ARCH_BASELINE") == "" {
		t.Skip("set REGEN_ARCH_BASELINE=1 to regenerate baseline.json")
	}
	root, err := FindRepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseline(filepath.Join(root, "internal", "architectureguard", "baseline.json"), scanned); err != nil {
		t.Fatal(err)
	}
}
