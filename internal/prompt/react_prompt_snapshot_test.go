package prompt

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

const (
	// 2026-06-05: workflow and diagnosis catalogs are generated from registries.
	// 2026-06-05 (remediation): operation boundary / state-refresh / no-pretext /
	// vague-failure rules now single-sourced from cards.go (shared with the
	// flag-on cards), so the rule text lives in one place. SHA updated to match.
	// 2026-06-18: narrow deploy/create boundary to hardware-first creation only.
	// 2026-06-20: remove duplicate CreateInstanceWorkflow routing sentence from the shared card.
	mutatingReActPromptSHA256 = "c19d3c49b615c0e9052aaf0f70a79f3b3253ecb0c76fff8958768a04c28215d6"
	// 2026-06-05: read-only diagnosis catalog is generated from the diagnosis registry.
	readOnlyReActPromptSHA256 = "5aae720ff488add4eb47c22379bfed6b8746c1ff80bd62594098e43b622ff55b"
)

func TestReActPromptSnapshot_Mutating(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: true})
	assertPromptSHA256(t, "mutating", p, mutatingReActPromptSHA256)
}

func TestReActPromptSnapshot_ReadOnly(t *testing.T) {
	p := BuildSystemWithOptions("test context", BuildOptions{MutatingToolsEnabled: false})
	assertPromptSHA256(t, "read_only", p, readOnlyReActPromptSHA256)
}

func assertPromptSHA256(t *testing.T, name, prompt, want string) {
	t.Helper()
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
	if got != want {
		t.Fatalf("%s ReAct prompt SHA drifted\n got: %s\nwant: %s\nlength: %d", name, got, want, len([]byte(prompt)))
	}
}
