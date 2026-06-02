package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

const generatedRegistryDigestExpected = "786a9e3020d64341de735d2a0cf33e2efe5e068ccda98aef2e87c42c9610d248"

func computeRegistryDigest(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	h := sha256.Sum256(norm)
	return hex.EncodeToString(h[:])
}
