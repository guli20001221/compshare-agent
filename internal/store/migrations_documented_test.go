package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryMigrationIsNamedInTheDeployDoc closes the gap a live check found on
// 2026-07-29: the root README told operators to apply `deploy/migrations/000*.sql`,
// a glob that stops at 0009 and silently skipped 0010 and 0011.
//
// Silently is the operative word. A missing table does not always fail loudly:
// 0011's ssh_ops_audit is fail-closed, so without it the in-instance ops lane
// disables itself and the server starts normally — the feature just does nothing.
// The failures that DO surface name a COLUMN, not a migration
// ("verify schema messages.turn_protocol: column \"turn_id\" does not exist"),
// so a deployer has no thread back to the file they skipped.
//
// A doc cannot be kept correct by remembering to edit it: adding 0012 must fail
// the build until it is written down.
func TestEveryMigrationIsNamedInTheDeployDoc(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	doc, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read migrations README: %v", err)
	}

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		found++
		if !strings.Contains(string(doc), name) {
			t.Errorf("migration %s is not named in deploy/migrations/README.md — "+
				"an operator applying the documented list would skip it", name)
		}
	}
	if found == 0 {
		t.Fatal("no migrations found; the test would pass vacuously")
	}
}
