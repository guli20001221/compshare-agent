package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

const generatedRegistryDigestExpected = "c9867b237dbe694311e1ec71174d717239bc4a292fb52ca8e080a521a16c347c"

func computeRegistryDigest(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	h := sha256.Sum256(norm)
	return hex.EncodeToString(h[:])
}
