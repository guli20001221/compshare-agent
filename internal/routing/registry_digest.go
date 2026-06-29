package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

const generatedRegistryDigestExpected = "9693c4311ceb1f757203beed4ff9daee62267dc58969348d4f533fdd80eb8704"

func computeRegistryDigest(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	h := sha256.Sum256(norm)
	return hex.EncodeToString(h[:])
}
