package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

const generatedRegistryDigestExpected = "2c9a16e5d7f24590e34d325cef18f1024c304d100019158b9e11e1a5309c1dc6"

func computeRegistryDigest(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	h := sha256.Sum256(norm)
	return hex.EncodeToString(h[:])
}
