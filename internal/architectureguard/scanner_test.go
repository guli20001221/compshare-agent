package architectureguard

import (
	"path/filepath"
	"testing"
)

func TestNoNewSemanticRegexKeywordOrDecisionSite(t *testing.T) {
	root, err := FindRepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	current, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := LoadBaseline(filepath.Join(root, "internal", "architectureguard", "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	if unexpected := Unexpected(current, reviewed); len(unexpected) > 0 {
		t.Fatalf("new semantic patch sites are forbidden; use Agent/Capability/Action contracts instead: %#v", unexpected)
	}
}
