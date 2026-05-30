package skills

import (
	"bytes"
	"os"
	"testing"
)

const generatedFile = "registry_gen.go"

// normalizeLF mirrors computeRegistryDigest's newline normalization so byte
// comparisons are CRLF/LF agnostic.
func normalizeLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
}

// TestGeneratedRegistry_MatchesDisk is the codegen drift gate: regenerating from
// disk must reproduce the committed registry_gen.go byte-for-byte (after LF
// normalization). Mirrors the CI `go generate && git diff --exit-code` check
// in-process so a stale registry fails `go test ./...`.
func TestGeneratedRegistry_MatchesDisk(t *testing.T) {
	want, err := GenerateRegistry(seededRoot)
	if err != nil {
		t.Fatalf("GenerateRegistry: %v", err)
	}
	got, err := os.ReadFile(generatedFile)
	if err != nil {
		t.Fatalf("read %s: %v", generatedFile, err)
	}
	if !bytes.Equal(normalizeLF(got), normalizeLF(want)) {
		t.Fatalf("%s is stale — run `go generate ./internal/skills` and commit the result", generatedFile)
	}
}

// TestGeneratedRegistry_DigestPinned verifies registry_gen.go matches the pinned
// digest. Stronger than the drift gate: it pins a known-good snapshot. On a
// legitimate skill change, regenerate, then paste the printed digest into
// registry_digest.go.
func TestGeneratedRegistry_DigestPinned(t *testing.T) {
	src, err := os.ReadFile(generatedFile)
	if err != nil {
		t.Fatalf("read %s: %v", generatedFile, err)
	}
	got := computeRegistryDigest(src)
	if got != generatedRegistryDigestExpected {
		t.Fatalf("registry digest mismatch:\n  got  %s\n  want %s\n(update generatedRegistryDigestExpected after an intentional skill change)", got, generatedRegistryDigestExpected)
	}
}

// TestGeneratedRegistry_SemanticParityWithLoader gives generatedSkills a
// consumer and asserts the codegen output stays semantically in sync with the
// on-disk loader (same names, same required_tools).
func TestGeneratedRegistry_SemanticParityWithLoader(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	gen := GeneratedSkills()
	if len(gen) != l.Len() {
		t.Fatalf("generated %d skills, loader has %d", len(gen), l.Len())
	}
	for _, g := range gen {
		ls, ok := l.Fetch(g.Name)
		if !ok {
			t.Errorf("generated skill %q absent from loader", g.Name)
			continue
		}
		if !equalStrings(g.RequiredTools, ls.RequiredTools) {
			t.Errorf("skill %q required_tools drift: gen=%v disk=%v", g.Name, g.RequiredTools, ls.RequiredTools)
		}
		if g.Path != ls.Path {
			t.Errorf("skill %q path drift: gen=%q disk=%q", g.Name, g.Path, ls.Path)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
