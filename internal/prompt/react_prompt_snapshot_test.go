package prompt

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

const (
	// 2026-06-05: workflow and diagnosis catalogs are generated from registries.
	mutatingReActPromptSHA256 = "e21b6aeffb52c65861fc3e7889aca8d35770a71feb5e1ed025261ad96db56670"
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
