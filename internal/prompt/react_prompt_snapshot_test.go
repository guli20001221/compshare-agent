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
	mutatingReActPromptSHA256 = "af685e87dad469f05e50418c7d8d724f01c32f02125b632656f1b23191cc8cb4"
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
