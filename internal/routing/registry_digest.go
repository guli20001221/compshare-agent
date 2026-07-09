package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

const generatedRegistryDigestExpected = "53c777f073c3e7d2626aaa7867dd73fe9235e54836c0ad734360a5e48fe55519"

func computeRegistryDigest(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	h := sha256.Sum256(norm)
	return hex.EncodeToString(h[:])
}
