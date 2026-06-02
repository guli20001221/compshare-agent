package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

// generatedRegistryDigestExpected pins the LF-normalized sha256 of
// registry_gen.go. This is stronger than ADR-004 §179's "git status clean":
// it also catches a hand-edit that happens to re-gofmt identically. Update this
// constant whenever the skill set legitimately changes — regenerate via
// `go generate ./internal/skills`, then paste the new digest (the
// TestGeneratedRegistry_DigestPinned failure prints the computed value).
const generatedRegistryDigestExpected = "dd45adee2d200d65f27b14da1a576d6a9fe3af690b59461868e79876fdd4736c"

// computeRegistryDigest returns the LF-normalized sha256 of the generated
// registry source, so the pin is byte-stable across CRLF/LF checkouts (mirrors
// internal/knowledge.ComputeCorpusDigest).
func computeRegistryDigest(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	h := sha256.Sum256(norm)
	return hex.EncodeToString(h[:])
}
